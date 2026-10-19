package plugin

// ss 插件：Shadowsocks AEAD TCP 入站 + 密码 → 客户端 路由。
// 密码路由与其限速配置存于 vn_user_bindings 表（protocol = ss），经注入的 UserStore 读写；
// 插件自身的启停状态、监听端口与加密方法留在 vn_plugin 表。

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/shadowsocks"
)

// pluginSS 是 ss 插件在 vn_plugin 表里的 name。
const pluginSS = "ss"

// DefaultSSMethod 是未显式配置时的加密方法（三种 AEAD 中安全性最好的一种）。
const DefaultSSMethod = "aes-256-gcm"

// SSRoute 是一条 ss 密码 → 客户端 的路由。
type SSRoute struct {
	Password string `json:"password"`
	Client   string `json:"client"`
}

// SSPlugin 提供 Shadowsocks 入站：认证密码后按路由把流量经绑定客户端的隧道送出。
type SSPlugin struct {
	mu        sync.Mutex
	store     store.Store
	users     UserStore
	exits     transport.ExitResolver
	limits    *ratelimit.Registry
	traffic   *traffic.Meter
	cancel    context.CancelFunc
	listener  net.Listener
	validator *shadowsocks.Validator
	port      int
	method    string
	running   bool
}

// setTraffic 注入流量记账器（插件管理器在装配时调用）。
func (p *SSPlugin) setTraffic(t *traffic.Meter) { p.traffic = t }

// newSSPlugin 创建 ss 插件；验证器在构造期即建立，用户页随时增删的密码不会丢。
func newSSPlugin(st store.Store, users UserStore, exits transport.ExitResolver, limits *ratelimit.Registry) *SSPlugin {
	method, _ := shadowsocks.ParseMethod(DefaultSSMethod)
	return &SSPlugin{
		store:     st,
		users:     users,
		exits:     exits,
		limits:    limits,
		validator: shadowsocks.NewValidator(method),
		method:    DefaultSSMethod,
	}
}

// Name 返回插件名。
func (p *SSPlugin) Name() string { return pluginSS }

// Status 返回当前状态：监听端口与加密方法。
func (p *SSPlugin) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Status{
		Name:    pluginSS,
		Enabled: p.running,
		Ports:   []PortKV{{Protocol: store.ProtocolSS, Port: p.port}},
		Params:  map[string]string{"method": p.method},
	}
}

// SyncUser 把用户表的一条 ss 变更同步进运行中的验证器：管理台在用户页增删或启用/停用
// ss 用户时，入站认证必须立刻跟随，否则会出现"表里有、连不上"或"删了还能连"的偏斜。
// 其他协议的变更与本插件无关，直接忽略。
func (p *SSPlugin) SyncUser(proto store.Protocol, key, client string, status store.UserStatus, deleted bool) {
	if proto != store.ProtocolSS {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// 停用与删除对验证器而言动作相同：摘掉，密码不再能试解通过。
	if deleted || status != store.UserEnabled {
		p.validator.Remove(key)
		return
	}
	if err := p.validator.Add(key, client); err != nil {
		logging.Warn("ss: sync user failed", "err", err)
	}
}

// Restore 进程启动时从存储恢复启停状态、方法与密码路由。
func (p *SSPlugin) Restore(ctx context.Context) {
	p.mu.Lock()
	p.restoreLocked(ctx)
	p.mu.Unlock()
}

func (p *SSPlugin) restoreLocked(ctx context.Context) {
	cur, ok, _ := p.store.GetPlugin(ctx, pluginSS)
	if m := cur.Config.Method; m != "" {
		if _, err := shadowsocks.ParseMethod(m); err != nil {
			logging.Warn("ss: stored method invalid", "method", m)
		} else if m != p.method {
			p.setMethodLocked(m)
		}
	}
	for _, r := range p.ssRoutes() {
		if err := p.validator.Add(r.Password, r.Client); err != nil {
			logging.Warn("ss: restore user failed", "err", err)
		}
	}
	// 与 v2ray 同理：端口独立于启停恢复，停用态也要认得上次配的端口。
	if port := cur.Config.Ports[store.ProtocolSS]; port > 0 {
		p.port = port
	}
	if !ok || cur.Status != store.PluginEnabled || p.port == 0 {
		return
	}
	if err := p.startLocked(ctx, p.port); err != nil {
		logging.Warn("ss: restore listener failed", "err", err)
	}
}

// saveLocked 把启停状态、端口与方法写进 vn_plugin；status 为空串表示沿用库中原值
// （停用态改端口时不该顺手把状态改掉）。调用方必须持锁。
func (p *SSPlugin) saveLocked(ctx context.Context, status store.PluginStatus, port int, method string) error {
	cur, ok, _ := p.store.GetPlugin(ctx, pluginSS)
	if !ok {
		cur = store.PluginState{Name: pluginSS}
	}
	cur.Name = pluginSS
	if status != "" {
		cur.Status = status
	}
	if cur.Config.Ports == nil {
		cur.Config.Ports = map[store.Protocol]int{}
	}
	cur.Config.Ports[store.ProtocolSS] = port
	cur.Config.Method = method
	return p.store.SetPlugin(ctx, cur)
}

// Enable 启用插件：校验并持久化端口与方法，然后立即开始监听。
// params 里 port 必填，method 可选（缺省沿用已存值或默认方法）。
func (p *SSPlugin) Enable(ctx context.Context, params map[string]string) error {
	port, err := strconv.Atoi(params["port"])
	if err != nil || port <= 0 || port >= 65536 {
		return fmt.Errorf("plugin: invalid ss port %q", params["port"])
	}
	method := params["method"]
	if method != "" {
		if _, err := shadowsocks.ParseMethod(method); err != nil {
			return fmt.Errorf("plugin: %w", err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if method == "" {
		method = p.method
	}
	if err := p.saveLocked(ctx, store.PluginEnabled, port, method); err != nil {
		return err
	}
	if method != p.method {
		p.setMethodLocked(method)
	}
	if p.running && p.port == port {
		return nil
	}
	p.stopLocked()
	return p.startLocked(ctx, port)
}

// Disable 停用插件：停止监听并持久化状态。
func (p *SSPlugin) Disable(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.saveLocked(ctx, store.PluginDisabled, p.port, p.method); err != nil {
		return err
	}
	p.stopLocked()
	return nil
}

// SetPort 修改监听端口并持久化：停用时只改待启用端口（下次启用生效）；运行中则热切换
// ——先用一个立即关闭的探针监听预检新端口可绑，通过后关旧监听、起新监听，失败时旧
// 监听原样保留。旧监听上的存量连接不随监听关闭而断开（连接各有协程），只有新连接
// 走新端口。
func (p *SSPlugin) SetPort(ctx context.Context, port int) error {
	if port <= 0 || port >= 65536 {
		return fmt.Errorf("plugin: invalid ss port %d", port)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running && p.port == port {
		return nil
	}
	if !p.running {
		if err := p.saveLocked(ctx, "", port, p.method); err != nil {
			return err
		}
		p.port = port
		logging.Info("ss: port updated", "port", port, "running", false)
		return nil
	}
	old := p.port
	probe, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("plugin: ss listen :%d: %w", port, err)
	}
	_ = probe.Close()
	p.stopLocked()
	if err := p.startLocked(ctx, port); err != nil {
		// 预检通过仍可能竞态失败：回滚到旧端口，别把在用的服务弄丢。
		if rerr := p.startLocked(ctx, old); rerr != nil {
			logging.Error("ss: rollback to previous port failed", "port", old, "err", rerr)
		}
		return err
	}
	if err := p.saveLocked(ctx, store.PluginEnabled, port, p.method); err != nil {
		return err
	}
	logging.Info("ss: port updated", "port", port, "running", true)
	return nil
}

// Stop 运行中插件立即停止监听（进程收尾用，不改动持久化状态）。
func (p *SSPlugin) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
}

// setMethodLocked 切换加密方法并重建验证器（主密钥派生随方法而变），
// 路由从用户表重灌——它是唯一真源。调用方必须持锁。
func (p *SSPlugin) setMethodLocked(method string) {
	m, _ := shadowsocks.ParseMethod(method)
	p.method = method
	p.validator = shadowsocks.NewValidator(m)
	for _, r := range p.ssRoutes() {
		if err := p.validator.Add(r.Password, r.Client); err != nil {
			logging.Warn("ss: reload user failed", "err", err)
		}
	}
}

// startLocked 启动 Shadowsocks 监听；调用方必须持锁。
func (p *SSPlugin) startLocked(_ context.Context, port int) error {
	method, err := shadowsocks.ParseMethod(p.method)
	if err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("plugin: ss listen :%d: %w", port, err)
	}
	// 监听器的生命周期只由 Stop（Drain 收尾）显式管理，不随调用方 ctx 消亡：
	// 管理台的 Enable/SetPort 带的是 HTTP 请求 ctx，请求返回即取消，绑上去
	// 会把刚起的监听立刻关掉（插件显示"运行中"但端口无人监听）。
	runCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.listener = ln
	p.port = port
	p.running = true
	logging.Info("ss: inbound started", "port", port, "method", method.Name)

	routine.Go("plugin.ss.watch", func() {
		<-runCtx.Done()
		_ = ln.Close()
	})
	routine.Go("plugin.ss.accept", func() {
		server := shadowsocks.NewServer(p.validator, p.handle)
		if err := server.Serve(ln); err != nil && runCtx.Err() == nil {
			logging.Warn("ss: inbound stopped", "err", err)
		}
	})
	return nil
}

// stopLocked 停止监听；调用方必须持锁。
func (p *SSPlugin) stopLocked() {
	if !p.running {
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	if p.listener != nil {
		_ = p.listener.Close()
	}
	p.running = false
	logging.Info("ss: inbound stopped", "port", p.port)
}

// ssRoutes 返回当前全部启用中的密码 → 客户端 路由（从用户表读取）。停用的用户不是路由，
// 这里就不列出——它同时决定了重启后验证器的灌入范围。
func (p *SSPlugin) ssRoutes() []SSRoute {
	all := p.users.ListUsers()
	out := make([]SSRoute, 0, len(all))
	for _, u := range all {
		if u.Protocol != store.ProtocolSS || u.Status != store.UserEnabled {
			continue
		}
		out = append(out, SSRoute{Password: u.Key, Client: u.Client})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Password < out[j].Password })
	return out
}

// SSRoutes 返回按密码排序的路由列表（管理台展示用）。
func (p *SSPlugin) SSRoutes() []SSRoute { return p.ssRoutes() }

// AccountLimits 返回 ss 账号当前的上下行限速（字节/秒；0 = 不限速）。
func (p *SSPlugin) AccountLimits(account string) (up, down int64) {
	u, ok := p.users.LookupUser(store.ProtocolSS, account)
	if !ok {
		return 0, 0
	}
	return u.Config.UpLimit, u.Config.DownLimit
}

// handle 把解码完成的 Shadowsocks 连接经绑定客户端的隧道送达目标。
func (p *SSPlugin) handle(password, client string, target shadowsocks.Target, conn *shadowsocks.Conn) {
	log := p.userLog(password)
	exit, ok := p.exits.Resolve(client)
	if !ok {
		if client == transport.LocalClientName {
			log.Warn("ss: server:self bound but server_proxy is disabled, closing ss conn",
				"client", client, "target", target.String())
		} else {
			log.Warn("ss: bound client offline, closing ss conn",
				"client", client, "target", target.String())
		}
		_ = conn.Close()
		return
	}

	// 用户级并发：占一个并发位再建隧道，超限排队。
	if p.limits != nil {
		gate := p.limits.Gate(ratelimit.Identity(store.ProtocolSS, password))
		if err := gate.Acquire(nil); err != nil {
			log.Warn("ss: user concurrency limit reached", "client", client, "target", target.String(), "err", err)
			_ = conn.Close()
			return
		}
		defer gate.Release()
	}
	tunnel, err := exit.OpenTunnel(
		dispatch.Destination{Host: target.Host, Port: target.Port},
		protocol.TunnelRequest{Origin: "ss"})
	if err != nil {
		log.Warn("ss: open tunnel failed",
			"client", client, "target", target.String(), "err", err)
		_ = conn.Close()
		return
	}
	log.Info("ss: tunnel established", "client", client, "target", target.String())
	// 按密码限速与统计：上行限 ss 连接的 reader（用户发多快），
	// 下行限隧道 reader（客户端送多快）；字节按密码与 client 两个维度各记一份。
	var o transport.ProxyOptions
	if p.limits != nil {
		pair := p.limits.Pair(ratelimit.Identity(store.ProtocolSS, password))
		o.UpLimit, o.DownLimit, o.Stats = pair.Up, pair.Down, pair.Stats
	}
	if p.traffic != nil {
		o.Stats = p.traffic.UserStats(store.ProtocolSS, password)
		o.ClientStats = p.traffic.ClientStats(client)
	}
	transport.ProxyBidirectional(conn, tunnel, "plugin.ss.tunnel", o)
	_ = conn.Close()
	_ = tunnel.Close()
	log.Info("ss: tunnel closed", "client", client, "target", target.String())
}

// userLog 返回该 ss 账号的会话日志器：级别取用户配置里的 log_level（默认 info），
// user 字段用掩码后的密码，另带上管理台显示名——隧道日志不再逐处重复传 user。
func (p *SSPlugin) userLog(password string) *logging.Scoped {
	level, name := "", ""
	if p.users != nil {
		if u, ok := p.users.LookupUser(store.ProtocolSS, password); ok {
			level = u.Config.LogLevel
			name = u.Name
		}
	}
	attrs := []any{"user", shadowsocks.MaskPassword(password)}
	if name != "" {
		attrs = append(attrs, "name", name)
	}
	return logging.NewScoped(level, attrs...)
}
