package ratelimit

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hhy50/vnray/internal/concurrency"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
)

// Identity 返回隔离不同协议用户的运行态身份键。
func Identity(p store.Protocol, key string) string { return string(p) + ":" + key }

// Bucket 是容量为一秒速率、支持取消和动态调速的共享令牌桶。
type Bucket struct {
	rate    atomic.Int64
	mu      sync.Mutex
	tokens  float64
	last    time.Time
	changed chan struct{}
}

// NewBucket 创建令牌桶；非正速率表示不限速。
func NewBucket(bytesPerSec int64) *Bucket {
	b := &Bucket{last: time.Now(), changed: make(chan struct{})}
	b.rate.Store(max(bytesPerSec, 0))
	b.tokens = float64(b.rate.Load())
	return b
}

// refill 必须持锁调用，调速前以旧速率结算已有令牌。
func (b *Bucket) refill(now time.Time) {
	rate := float64(b.rate.Load())
	b.tokens = math.Min(rate, b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
}

// SetRate 更新速率并唤醒已有等待者，不重建共享桶。
func (b *Bucket) SetRate(bytesPerSec int64) {
	bytesPerSec = max(bytesPerSec, 0)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rate.Load() == bytesPerSec {
		return
	}
	b.refill(time.Now())
	b.tokens = math.Min(b.tokens, float64(bytesPerSec))
	b.rate.Store(bytesPerSec)
	close(b.changed)
	b.changed = make(chan struct{})
}

// Rate 返回字节/秒速率；0 表示不限速。
func (b *Bucket) Rate() int64 { return b.rate.Load() }

// Wait 按实际补充的令牌放行字节，取消或调速均可打断等待。
func (b *Bucket) Wait(ctx context.Context, n int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if n <= 0 || b.rate.Load() == 0 {
		return nil
	}
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	remaining := float64(n)
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.mu.Lock()
		rate := float64(b.rate.Load())
		if rate == 0 {
			b.mu.Unlock()
			return nil
		}
		b.refill(time.Now())
		take := math.Min(remaining, rate)
		if b.tokens >= take {
			b.tokens -= take
			remaining -= take
			b.mu.Unlock()
			continue
		}
		delay := max(time.Nanosecond, time.Duration((take-b.tokens)/rate*float64(time.Second)))
		changed := b.changed
		b.mu.Unlock()
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
	return nil
}

// Pair 是同一身份跨隧道共享的上下行桶和累计统计。
type Pair struct {
	Up    *Bucket
	Down  *Bucket
	Stats *transport.PumpStats
}

// Registry 保存用户配置的运行态投影，令牌和统计不落库。
type Registry struct {
	mu      sync.RWMutex
	entries map[string]Pair
	gates   map[string]*concurrency.Gate
}

// NewRegistry 创建空的限速注册表。
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]Pair), gates: make(map[string]*concurrency.Gate)}
}

// Pair 返回共享运行态；查表仅在建立隧道时执行，不进入逐块转发热路径。
func (r *Registry) Pair(identity string) Pair {
	r.mu.RLock()
	e, ok := r.entries[identity]
	r.mu.RUnlock()
	if ok {
		return e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok = r.entries[identity]; !ok {
		e = Pair{Up: NewBucket(0), Down: NewBucket(0), Stats: &transport.PumpStats{}}
		r.entries[identity] = e
	}
	return e
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
	r.mu.RLock()
	g, ok := r.gates[identity]
	r.mu.RUnlock()
	if ok {
		return g
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok = r.gates[identity]; !ok {
		g = concurrency.NewGate()
		r.gates[identity] = g
	}
	return g
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
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, identity)
	delete(r.gates, identity)
}
