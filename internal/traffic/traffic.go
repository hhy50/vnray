package traffic

import (
	"context"
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

// flushTimeout 是收尾落库的等待上限：此时 ctx 已取消，只能另起一个短超时。
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

// Meter 是流量记账器：用户与客户端各持一份计数器，定期落库。
// 计数器在隧道建立时取用，Flush 由单个协程驱动，故用互斥锁保护登记表即可。
type Meter struct {
	limits *ratelimit.Registry

	mu      sync.Mutex
	users   map[userKey]*entry
	clients map[string]*entry

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
		limits:  limits,
		users:   make(map[userKey]*entry),
		clients: make(map[string]*entry),
		reload:  make(chan struct{}, 1),
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

// UserStats 返回用户维度的共享字节计数器（同一用户的多条隧道合计）。
func (m *Meter) UserStats(p store.Protocol, key string) *transport.PumpStats {
	k := userKey{Protocol: p, Key: key}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.users[k]
	if !ok {
		e = &entry{stats: m.limits.Pair(ratelimit.Identity(p, key)).Stats}
		m.users[k] = e
	}
	return e.stats
}

// ClientStats 返回客户端维度的共享字节计数器（同一客户端的全部隧道合计）。
func (m *Meter) ClientStats(name string) *transport.PumpStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.clients[name]
	if !ok {
		e = &entry{stats: &transport.PumpStats{}}
		m.clients[name] = e
	}
	return e.stats
}

// Flush 把自上次成功落库以来的增量写进两张表。写失败的条目保留增量，下轮重试。
func (m *Meter) Flush(ctx context.Context, st store.Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, e := range m.users {
		up, down := e.stats.UpWrite.Load(), e.stats.DownWrite.Load()
		du, dd := up-e.seenUp, down-e.seenDown
		if du == 0 && dd == 0 {
			continue
		}
		if err := st.AddUserTraffic(ctx, k.Protocol, k.Key, du, dd); err != nil {
			logging.Warn("traffic: flush user traffic failed",
				"protocol", k.Protocol, "user_key", k.Key, "err", err)
			continue
		}
		e.seenUp, e.seenDown = up, down
	}
	for name, e := range m.clients {
		up, down := e.stats.UpWrite.Load(), e.stats.DownWrite.Load()
		du, dd := up-e.seenUp, down-e.seenDown
		if du == 0 && dd == 0 {
			continue
		}
		if err := st.AddClientTraffic(ctx, name, du, dd); err != nil {
			logging.Warn("traffic: flush client traffic failed", "client", name, "err", err)
			continue
		}
		e.seenUp, e.seenDown = up, down
	}
}

// Run 启动定期落库协程；ctx 结束时补一次收尾落库，避免丢掉最后不到一个周期的字节。
// 落库周期可经 SetFlushInterval 热改，届时重建 ticker 从新周期起算。
func (m *Meter) Run(ctx context.Context, st store.Store) {
	routine.Go("traffic.flusher", func() {
		t := time.NewTicker(time.Duration(m.flushInterval.Load()))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				flushCtx, cancel := context.WithTimeout(context.Background(), flushTimeout)
				m.Flush(flushCtx, st)
				cancel()
				return
			case <-m.reload:
				t.Reset(time.Duration(m.flushInterval.Load()))
			case <-t.C:
				m.Flush(ctx, st)
			}
		}
	})
}
