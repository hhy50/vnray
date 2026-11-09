package portmap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
)

// acceptRetry 是 accept 出错后的重试间隔：监听器被临时性错误打断时不至于空转。
const acceptRetry = time.Second

type mappingStore interface {
	ListPortMappings(context.Context) ([]store.PortMapping, error)
}

// Manager 持有当前生效的监听器集合；Sync 做差量启停，Drain 全部关闭。
type Manager struct {
	sessions *transport.SessionManager
	st       mappingStore
	// traffic 是流量记账器；nil 时字节只计入全局隧道计数。
	traffic *traffic.Meter
	// limits 是限速注册表（与服务端用户限速共享，身份前缀 portmap: 隔离）；
	// nil 时不限速。
	limits *ratelimit.Registry

	syncMu    sync.Mutex
	mu        sync.Mutex
	running   map[uint64]*lnEntry
	ctx       context.Context
	cancel    context.CancelFunc
	runners   sync.WaitGroup
	drainOnce sync.Once
	drained   context.Context
}

// lnEntry 的 ctx 覆盖本代监听与连接，热停用只关闭监听。
type lnEntry struct {
	cfg       store.PortMapping
	ln        net.Listener
	ctx       context.Context
	cancel    context.CancelFunc
	upLimit   int64
	downLimit int64
}

// Identity 是端口映射在限速注册表里的身份（与服务端用户身份按前缀隔离）。
func Identity(id uint64) string { return "portmap:" + strconv.FormatUint(id, 10) }

// NewManager 创建端口映射管理器；sessions 为会话管理器（客户端选路唯一来源），
// st 为映射规则持久层，traffic 与 limits 可为 nil。
func NewManager(ctx context.Context, sessions *transport.SessionManager, st mappingStore, traffic *traffic.Meter, limits *ratelimit.Registry) *Manager {
	ctx, cancel := context.WithCancel(ctx)
	return &Manager{
		sessions: sessions,
		st:       st,
		traffic:  traffic,
		limits:   limits,
		running:  make(map[uint64]*lnEntry),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Name 实现 extension.Service。
func (m *Manager) Name() string { return "PortMapService" }

// Start 实现 extension.Service：进程生命周期内先按库中规则建好监听。
func (m *Manager) Start(ctx context.Context) error {
	if err := m.ctx.Err(); err != nil {
		return err
	}
	m.Sync(ctx)
	return nil
}

// Drain 停止接入，终止存量转发并等待所有任务退出。
func (m *Manager) Drain(ctx context.Context) error {
	m.cancel()
	m.drainOnce.Do(func() {
		// 取消后穿过登记锁，确保所有 runner Add 已结束。
		m.mu.Lock()
		m.mu.Unlock()
		drained, complete := context.WithCancel(context.Background())
		m.drained = drained
		routine.Go("portmap.drain", func() {
			defer complete()
			m.runners.Wait()
		})
	})
	select {
	case <-m.drained.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Sync 重读映射表并差量应用到监听器集合：删除/停用/配置变化的监听器关闭重建，
// 其余保持不动（不惊扰在途连接）。ctx 只约束读库；监听器挂在各自协程上。
func (m *Manager) Sync(ctx context.Context) {
	// 读取与应用属于一次串行更新，不能让旧查询覆盖新监听状态。
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	if m.ctx.Err() != nil {
		return
	}
	rows, err := m.st.ListPortMappings(ctx)
	if err != nil {
		logging.Error("portmap: list mappings failed", "err", err)
		return
	}
	desired := make(map[uint64]store.PortMapping, len(rows))
	for _, r := range rows {
		if r.Enabled {
			desired[r.ID] = r
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return
	}
	for id, e := range m.running {
		want, ok := desired[id]
		if !ok {
			m.stopEntry(id, e)
			continue
		}
		if !sameMapping(want, e.cfg) {
			m.stopEntry(id, e)
			continue
		}
		delete(desired, id)
		// 数据面没变、只改了限速：原地重推速率，不重建监听器（在途连接不断）。
		if m.limits != nil && (want.Config.UpLimit != e.upLimit ||
			want.Config.DownLimit != e.downLimit) {
			m.limits.Apply(Identity(id), want.Config.UpLimit, want.Config.DownLimit)
			e.upLimit, e.downLimit = want.Config.UpLimit, want.Config.DownLimit
			logging.Info("portmap: limits updated", "id", id,
				"up_bps", want.Config.UpLimit, "down_bps", want.Config.DownLimit)
		}
	}
	for id, cfg := range desired {
		m.startEntry(id, cfg)
	}
}

// sameMapping 比较两条映射的数据面配置；ID 与审计字段不参与。
func sameMapping(a, b store.PortMapping) bool {
	return a.Client == b.Client && a.RemotePort == b.RemotePort &&
		a.LocalHost == b.LocalHost && a.LocalPort == b.LocalPort &&
		a.Config.CertPEM == b.Config.CertPEM && a.Config.KeyPEM == b.Config.KeyPEM
}

// startEntry 启动一条映射的监听器；端口被占（数据面/插件/其他映射）只记错误，
// 不中断其余映射，下次 Sync 会再尝试。限速经注册表下发：改配置 Sync 重推速率，
// 在途隧道持有的桶原地变速，不重建。
func (m *Manager) startEntry(id uint64, cfg store.PortMapping) {
	if cfg.Client == transport.LocalClientName {
		// 保留名归服务端本机出口，端口映射不支持它；显式跳过而非静默 offline。
		logging.Warn("portmap: reserved local client name, skipping mapping",
			"id", id, "client", cfg.Client)
		return
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.RemotePort))
	if err != nil {
		logging.Error("portmap: listen failed", "id", id, "port", cfg.RemotePort,
			"client", cfg.Client, "err", err)
		return
	}
	if cfg.Config.CertPEM != "" || cfg.Config.KeyPEM != "" {
		cert, err := tls.X509KeyPair([]byte(cfg.Config.CertPEM), []byte(cfg.Config.KeyPEM))
		if err != nil {
			_ = ln.Close()
			logging.Error("portmap: load TLS certificate failed", "id", id, "port", cfg.RemotePort,
				"client", cfg.Client, "err", err)
			return
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	}
	if m.limits != nil {
		m.limits.Apply(Identity(id), cfg.Config.UpLimit, cfg.Config.DownLimit)
	}
	ctx, cancel := context.WithCancel(m.ctx)
	e := &lnEntry{cfg: cfg, ln: ln, ctx: ctx, cancel: cancel,
		upLimit: cfg.Config.UpLimit, downLimit: cfg.Config.DownLimit}
	m.running[id] = e
	logging.Info("portmap: listener started", "id", id, "port", cfg.RemotePort,
		"client", cfg.Client, "target", fmt.Sprintf("%s:%d", cfg.LocalHost, cfg.LocalPort),
		"up_bps", cfg.Config.UpLimit, "down_bps", cfg.Config.DownLimit)
	m.runners.Add(1)
	routine.Go(fmt.Sprintf("portmap.accept.%d", id), func() {
		defer m.runners.Done()
		m.runEntry(id, e)
	})
}

func (m *Manager) runEntry(id uint64, e *lnEntry) {
	var watcher sync.WaitGroup
	watcher.Add(1)
	routine.Go("portmap.listener.cancel", func() {
		defer watcher.Done()
		<-e.ctx.Done()
		_ = e.ln.Close()
	})
	defer watcher.Wait()
	defer e.cancel()
	var connections sync.WaitGroup
	defer connections.Wait()
	defer func() {
		m.mu.Lock()
		if m.running[id] == e {
			m.stopEntry(id, e)
		}
		m.mu.Unlock()
	}()
	m.acceptLoop(id, e, &connections)
}

// stopEntry 关闭一个监听器并从 running 表移除；调用方必须持有 m.mu。
func (m *Manager) stopEntry(id uint64, e *lnEntry) {
	// 保留本代连接，runnerctx 在连接收尾或父级取消时结束。
	_ = e.ln.Close()
	delete(m.running, id)
	if m.limits != nil {
		m.limits.Forget(Identity(id))
	}
	logging.Info("portmap: listener stopped", "id", id, "port", e.cfg.RemotePort)
}

// acceptLoop 受理映射端口上的入站连接；主动停止（Drain/Sync 重建）时退出，
// 其余错误按固定间隔重试。
func (m *Manager) acceptLoop(id uint64, e *lnEntry, connections *sync.WaitGroup) {
	for {
		conn, err := e.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || e.ctx.Err() != nil {
				return
			}
			logging.Warn("portmap: accept failed", "id", id, "port", e.cfg.RemotePort, "err", err)
			select {
			case <-e.ctx.Done():
				return
			case <-time.After(acceptRetry):
				continue
			}
		}
		m.mu.Lock()
		if e.ctx.Err() != nil || m.running[id] != e {
			m.mu.Unlock()
			_ = conn.Close()
			return
		}
		m.mu.Unlock()
		connections.Add(1)
		routine.Go("portmap.conn", func() {
			defer connections.Done()
			defer func() { _ = conn.Close() }()
			m.handleConn(e.ctx, id, e.cfg, conn)
		})
	}
}

// handleConn 桥接一条入站连接：按映射找客户端会话，开 PortMap 流让客户端拨本地
// 目标，成功后双向搬运。连接失败只关连接，监听器不受影响。
func (m *Manager) handleConn(ctx context.Context, id uint64, cfg store.PortMapping, conn net.Conn) {
	remote := conn.RemoteAddr().String()
	dest := dispatch.Destination{Host: cfg.LocalHost, Port: cfg.LocalPort}
	if !dest.IsValid() {
		logging.Error("portmap: invalid mapping target", "id", id, "target", dest)
		_ = conn.Close()
		return
	}
	info, ok := m.sessions.SessionByName(cfg.Client)
	if !ok {
		logging.Warn("portmap: client offline, closing inbound conn",
			"id", id, "client", cfg.Client, "remote", remote)
		_ = conn.Close()
		return
	}
	sess, _ := info.Meta.(*transport.Session)
	if sess == nil {
		logging.Warn("portmap: client session carries no client view",
			"id", id, "client", cfg.Client, "remote", remote)
		_ = conn.Close()
		return
	}
	tunnel, err := sess.OpenPortMapContext(ctx, dest)
	if err != nil {
		logging.Warn("portmap: open client stream failed",
			"id", id, "client", cfg.Client, "target", dest.String(), "remote", remote, "err", err)
		_ = conn.Close()
		return
	}
	defer func() { _ = tunnel.Close() }()
	logging.Info("portmap: relay established",
		"id", id, "client", cfg.Client, "target", dest.String(), "remote", remote)
	o := transport.ProxyOptions{Context: ctx}
	if m.traffic != nil {
		o.Stats = m.traffic.PortMappingStats(id)
		o.ClientStats = m.traffic.ClientStats(cfg.Client)
	}
	// 同一映射的所有连接共享同一对桶（Pair 命名空间按映射隔离），合计不超配额。
	if m.limits != nil {
		pair := m.limits.Pair(Identity(id))
		o.UpLimit, o.DownLimit = pair.Up, pair.Down
	}
	transport.ProxyBidirectional(conn, tunnel, "portmap.pump", o)
	logging.Info("portmap: relay closed",
		"id", id, "client", cfg.Client, "target", dest.String(), "remote", remote)
}

// ListeningIDs 返回当前在监听的映射 ID 集合，供管理页标注运行状态。
func (m *Manager) ListeningIDs() map[uint64]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[uint64]bool, len(m.running))
	for id := range m.running {
		out[id] = true
	}
	return out
}
