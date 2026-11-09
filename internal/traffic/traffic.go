package traffic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
)

// FlushInterval 是流量增量落库的默认周期；进程退出时另补一次。
const FlushInterval = 10 * time.Second

// flushTimeout 防止慢库长期占住用户删除所需的落库锁。
const flushTimeout = 5 * time.Second

// userKey 是用户维度的记账键：同名 Key 在不同协议下是不同用户。
type userKey struct {
	Protocol store.Protocol
	Key      string
}

// entry 是一份计数器与上次成功落库时的读数，两者之差就是要写库的增量。
type entry struct {
	stats    *transport.PumpStats
	seenUp   int64
	seenDown int64
}

// Writer 是流量增量的持久化入口。
type Writer interface {
	AddUserTraffic(context.Context, store.Protocol, string, int64, int64) error
	AddClientTraffic(context.Context, string, int64, int64) error
	AddPortMappingTraffic(context.Context, uint64, int64, int64) error
}

// Meter 是流量记账器，按用户、客户端与端口映射累计字节。
type Meter struct {
	limits  *ratelimit.Registry
	flushMu sync.Mutex

	mu           sync.Mutex
	users        map[userKey]*entry
	clients      map[string]*entry
	portMappings map[uint64]*entry

	// flushInterval 是当前落库周期（纳秒）；reload 在周期变更时唤醒落库协程重建 ticker。
	flushInterval atomic.Int64
	reload        chan struct{}
}

// NewMeter 创建记账器；limits 是用户计数器的宿主，nil 时自建一份注册表。
func NewMeter(limits *ratelimit.Registry) *Meter {
	if limits == nil {
		limits = ratelimit.NewRegistry()
	}
	m := &Meter{
		limits:       limits,
		users:        make(map[userKey]*entry),
		clients:      make(map[string]*entry),
		portMappings: make(map[uint64]*entry),
		reload:       make(chan struct{}, 1),
	}
	m.flushInterval.Store(int64(FlushInterval))
	return m
}

// SetFlushInterval 热更新落库周期；非正数视为回到默认。落库协程未启动时无影响。
func (m *Meter) SetFlushInterval(d time.Duration) {
	if d <= 0 {
		d = FlushInterval
	}
	m.flushInterval.Store(int64(d))
	select {
	case m.reload <- struct{}{}:
	default:
	}
}

// UserStats 返回已登记用户的共享字节计数器；未登记或已删除的用户不计账。
func (m *Meter) UserStats(p store.Protocol, key string) *transport.PumpStats {
	if key == "" {
		return nil
	}
	k := userKey{Protocol: p, Key: key}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, registered := m.users[k]
	if !registered {
		return nil
	}
	if e == nil {
		e = &entry{stats: m.limits.Pair(ratelimit.Identity(p, key)).Stats}
		m.users[k] = e
	}
	return e.stats
}

// ClientStats 返回客户端维度的共享字节计数器（同一客户端的全部隧道合计）；空名不计账。
func (m *Meter) ClientStats(name string) *transport.PumpStats {
	if name == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.clients[name]
	if !ok {
		e = &entry{stats: &transport.PumpStats{}}
		m.clients[name] = e
	}
	return e.stats
}

// PortMappingStats 返回映射维度的共享字节计数器。
func (m *Meter) PortMappingStats(id uint64) *transport.PumpStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.portMappings[id]
	if !ok {
		e = &entry{stats: &transport.PumpStats{}}
		m.portMappings[id] = e
	}
	return e.stats
}

// batchWriter 由支持整批落库的存储实现。
type batchWriter interface {
	AddTrafficBatch(context.Context, store.TrafficBatch) error
}

// pendingEntry 是一条待落库条目及其读数：成功提交后才把 seen 推进到该读数。
type pendingEntry struct {
	entry *entry
	up    int64
	down  int64
}

// flushBatch 把一轮增量收成一批，在一个事务里写完。失败时整批回滚、seen 不动，
// 增量留到下一轮重试——与逐条路径的语义一致。
func (m *Meter) flushBatch(ctx context.Context, bw batchWriter) error {
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	batch := store.TrafficBatch{}
	var users, clients, mappings []pendingEntry
	m.mu.Lock()
	for k, e := range m.users {
		if e == nil {
			continue
		}
		if up, down := e.stats.UpWrite.Load(), e.stats.DownWrite.Load(); up != e.seenUp || down != e.seenDown {
			batch.Users = append(batch.Users, store.UserTrafficDelta{Protocol: k.Protocol, Key: k.Key, Up: up - e.seenUp, Down: down - e.seenDown})
			users = append(users, pendingEntry{entry: e, up: up, down: down})
		}
	}
	for name, e := range m.clients {
		if up, down := e.stats.UpWrite.Load(), e.stats.DownWrite.Load(); up != e.seenUp || down != e.seenDown {
			batch.Clients = append(batch.Clients, store.ClientTrafficDelta{Key: name, Up: up - e.seenUp, Down: down - e.seenDown})
			clients = append(clients, pendingEntry{entry: e, up: up, down: down})
		}
	}
	for id, e := range m.portMappings {
		if up, down := e.stats.UpWrite.Load(), e.stats.DownWrite.Load(); up != e.seenUp || down != e.seenDown {
			batch.PortMappings = append(batch.PortMappings, store.PortMappingTrafficDelta{ID: id, Up: up - e.seenUp, Down: down - e.seenDown})
			mappings = append(mappings, pendingEntry{entry: e, up: up, down: down})
		}
	}
	m.mu.Unlock()
	if err := bw.AddTrafficBatch(ctx, batch); err != nil {
		return fmt.Errorf("flush traffic batch: %w", err)
	}
	for _, group := range [][]pendingEntry{users, clients, mappings} {
		for _, p := range group {
			p.entry.seenUp, p.entry.seenDown = p.up, p.down
		}
	}
	return nil
}

// ForgetUser 等待在途落库完成并移除用户，旧隧道持有的计数器不再参与落库。
func (m *Meter) ForgetUser(p store.Protocol, key string) {
	k := userKey{Protocol: p, Key: key}
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	m.mu.Lock()
	delete(m.users, k)
	m.limits.Forget(ratelimit.Identity(p, key))
	m.mu.Unlock()
}

// RestoreUser 登记用户，重建身份时惰性分配新的计数器。
func (m *Meter) RestoreUser(p store.Protocol, key string) {
	if key == "" {
		return
	}
	m.mu.Lock()
	k := userKey{Protocol: p, Key: key}
	if _, registered := m.users[k]; !registered {
		m.users[k] = nil
	}
	m.mu.Unlock()
}

// Flush 把自上次成功落库以来的增量写入存储。写失败的条目保留增量，下轮重试。
// 存储支持整批写入时走单事务，否则退化成逐条累加（每轮落库的往返次数随用户数增长）。
func (m *Meter) Flush(ctx context.Context, st Writer) error {
	if bw, ok := st.(batchWriter); ok {
		return m.flushBatch(ctx, bw)
	}
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	m.mu.Lock()
	users := make(map[userKey]*entry, len(m.users))
	for k, e := range m.users {
		if e != nil {
			users[k] = e
		}
	}
	clients := make(map[string]*entry, len(m.clients))
	for k, e := range m.clients {
		clients[k] = e
	}
	mappings := make(map[uint64]*entry, len(m.portMappings))
	for k, e := range m.portMappings {
		mappings[k] = e
	}
	m.mu.Unlock()
	var errs []error
	for k, e := range users {
		up, down := e.stats.UpWrite.Load(), e.stats.DownWrite.Load()
		du, dd := up-e.seenUp, down-e.seenDown
		if du == 0 && dd == 0 {
			continue
		}
		if err := st.AddUserTraffic(ctx, k.Protocol, k.Key, du, dd); err != nil {
			errs = append(errs, fmt.Errorf("flush user traffic: %w", err))
			continue
		}
		e.seenUp, e.seenDown = up, down
	}
	for name, e := range clients {
		up, down := e.stats.UpWrite.Load(), e.stats.DownWrite.Load()
		du, dd := up-e.seenUp, down-e.seenDown
		if du == 0 && dd == 0 {
			continue
		}
		if err := st.AddClientTraffic(ctx, name, du, dd); err != nil {
			errs = append(errs, fmt.Errorf("flush client %s traffic: %w", name, err))
			continue
		}
		e.seenUp, e.seenDown = up, down
	}
	for id, e := range mappings {
		up, down := e.stats.UpWrite.Load(), e.stats.DownWrite.Load()
		du, dd := up-e.seenUp, down-e.seenDown
		if du == 0 && dd == 0 {
			continue
		}
		if err := st.AddPortMappingTraffic(ctx, id, du, dd); err != nil {
			errs = append(errs, fmt.Errorf("flush port mapping %d traffic: %w", id, err))
			continue
		}
		e.seenUp, e.seenDown = up, down
	}
	return errors.Join(errs...)
}

// Run 启动定期落库协程；ctx 结束时补一次收尾落库，避免丢掉最后不到一个周期的字节。
// 落库周期可经 SetFlushInterval 热改，届时重建 ticker 从新周期起算。
func (m *Meter) Run(ctx context.Context, st Writer) <-chan struct{} {
	done := make(chan struct{})
	routine.Go("traffic.flusher", func() {
		defer close(done)
		m.run(ctx, st)
		if err := m.flushWithin(context.WithoutCancel(ctx), st, flushTimeout); err != nil {
			logging.Warn("traffic: final flush failed", "err", err)
		}
	})
	return done
}

func (m *Meter) run(ctx context.Context, st Writer) {
	t := time.NewTicker(time.Duration(m.flushInterval.Load()))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.reload:
			t.Reset(time.Duration(m.flushInterval.Load()))
		case <-t.C:
			if err := m.flushWithin(ctx, st, flushTimeout); err != nil && ctx.Err() == nil {
				logging.Warn("traffic: flush failed", "err", err)
			}
		}
	}
}

func (m *Meter) flushWithin(ctx context.Context, st Writer, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return m.Flush(ctx, st)
}
