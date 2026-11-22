package ratelimit

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hhy50/vnray/internal/concurrency"
	"github.com/hhy50/vnray/internal/stats"
)

// Identity 返回隔离不同协议用户的运行态身份键。协议只作为命名空间参与拼接，
// 这里按底层类型约束而非具体类型取参，避免限速器反向依赖 store 的领域类型。
func Identity[P ~string](protocol P, key string) string { return string(protocol) + ":" + key }

// window 是补额窗口：每窗口发放 rate×window 的额度，容量同样如此。
// 窗口越细，单个请求一次能拿走的额度越小，代价是等待者唤醒更频繁。
const window = 100 * time.Millisecond

// quota 返回某速率下单个窗口的额度（字节）。
func quota(rate float64) float64 { return rate * window.Seconds() }

// Bucket 是容量为单窗口额度的窗口式令牌桶，支持取消和动态调速。
type Bucket struct {
	rate   atomic.Int64
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// NewBucket 创建令牌桶；非正速率表示不限速。
func NewBucket(bytesPerSec int64) *Bucket {
	b := &Bucket{last: time.Now()}
	b.rate.Store(max(bytesPerSec, 0))
	b.tokens = quota(float64(b.rate.Load()))
	return b
}

// refill 必须持锁调用，攒满一个窗口才补满额度，并把窗口起点钉回原网格，
// 否则每次访问的唤醒延迟会累积成漂移。
func (b *Bucket) refill(now time.Time) {
	elapsed := now.Sub(b.last)
	if elapsed < window {
		return
	}
	b.tokens = quota(float64(b.rate.Load()))
	b.last = now.Add(-(elapsed % window))
}

// SetRate 更新速率，不重建共享桶；阻塞中的等待者在下一个窗口边界按新速率重算。
// 从"不限速"开启限速时立即给满一个窗口，否则首字节要白等一整个窗口。
func (b *Bucket) SetRate(bytesPerSec int64) {
	bytesPerSec = max(bytesPerSec, 0)
	b.mu.Lock()
	defer b.mu.Unlock()
	old := b.rate.Load()
	if old == bytesPerSec {
		return
	}
	now := time.Now()
	b.refill(now)
	b.tokens = math.Min(b.tokens, quota(float64(bytesPerSec)))
	b.rate.Store(bytesPerSec)
	if old == 0 && bytesPerSec > 0 {
		b.tokens = quota(float64(bytesPerSec))
		b.last = now
	}
}

// Rate 返回字节/秒速率；0 表示不限速。
func (b *Bucket) Rate() int64 { return b.rate.Load() }

// Wait 按实际补充的令牌放行字节，取消可打断等待。
// 令牌够不上本轮所需时先扣走已有的那部分再等差额：已攒到的额度归本次调用，
// 不会被同一身份的其它隧道抢走。取消时把已扣未交付的额度退回。
func (b *Bucket) Wait(ctx context.Context, n int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if n <= 0 || b.rate.Load() == 0 {
		return nil
	}
	var timer *time.Timer
	remaining := float64(n)
	permitted := false
	defer func() {
		if timer != nil {
			timer.Stop()
		}
		if permitted {
			return
		}
		if used := float64(n) - remaining; used > 0 {
			b.mu.Lock()
			b.refill(time.Now())
			b.tokens = math.Min(quota(float64(b.rate.Load())), b.tokens+used)
			b.mu.Unlock()
		}
	}()
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.mu.Lock()
		rate := float64(b.rate.Load())
		if rate == 0 {
			b.mu.Unlock()
			permitted = true
			return nil
		}
		now := time.Now()
		b.refill(now)
		take := math.Min(remaining, quota(rate))
		got := math.Min(b.tokens, take)
		b.tokens -= got
		remaining -= got
		if got == take {
			b.mu.Unlock()
			continue
		}
		// 额度只在窗口边界补齐，睡到窗口起点加一个 window 即可。
		delay := b.last.Add(window).Sub(now)
		b.mu.Unlock()
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	permitted = true
	return nil
}

// Pair 是同一身份跨隧道共享的上下行桶和累计统计。
type Pair struct {
	Up    *Bucket
	Down  *Bucket
	Stats *stats.PumpStats
}

// Registry 保存用户配置的运行态投影，令牌和统计不落库。
type Registry struct {
	entries sync.Map
}

type registryEntry struct {
	pair Pair
	gate *concurrency.Gate
}

// NewRegistry 创建空的限速注册表。
func NewRegistry() *Registry {
	return &Registry{}
}

func (r *Registry) entry(identity string) *registryEntry {
	if value, ok := r.entries.Load(identity); ok {
		return value.(*registryEntry)
	}
	next := &registryEntry{
		pair: Pair{Up: NewBucket(0), Down: NewBucket(0), Stats: &stats.PumpStats{}},
		gate: concurrency.NewGate(),
	}
	value, _ := r.entries.LoadOrStore(identity, next)
	return value.(*registryEntry)
}

// Pair 返回共享运行态；查表仅在建立隧道时执行，不进入逐块转发热路径。
func (r *Registry) Pair(identity string) Pair {
	return r.entry(identity).pair
}

// Limits 返回上下行限速，单位为字节/秒。
func (r *Registry) Limits(identity string) (up, down int64) {
	e := r.Pair(identity)
	return e.Up.Rate(), e.Down.Rate()
}

// Usage 返回已转发的上下行累计字节数。
func (r *Registry) Usage(identity string) (up, down int64) {
	e := r.Pair(identity)
	return e.Stats.UpWrite.Load(), e.Stats.DownWrite.Load()
}

// Apply 将用户配置推入已有运行态，不替换活跃隧道持有的桶。
func (r *Registry) Apply(identity string, upBps, downBps int64) {
	e := r.Pair(identity)
	e.Up.SetRate(upBps)
	e.Down.SetRate(downBps)
}

// Gate 返回身份对应的并发闸；不存在时创建一张不限流的闸。
func (r *Registry) Gate(identity string) *concurrency.Gate {
	return r.entry(identity).gate
}

// Concurrency 设置身份的并发上限；非正数表示不限。
func (r *Registry) Concurrency(identity string, max int) {
	r.Gate(identity).SetMax(max)
}

// Current 返回身份的当前并发占用。
func (r *Registry) Current(identity string) int {
	return r.Gate(identity).Usage()
}

// Forget 移除注册表中的身份，不改变既有隧道的生命周期。
func (r *Registry) Forget(identity string) {
	r.entries.Delete(identity)
}
