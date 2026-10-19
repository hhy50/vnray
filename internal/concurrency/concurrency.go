package concurrency

// Gate 是共享并发闸：同时放行的资源数不超过 max（0/负 = 不限），超限时 Acquire
// 排队等待空位，直到有资源释放、cap 调大或 stop 通道关闭。cap 可在运行期热改，
// 对在等待者即时生效。用户级与客户端级的并发限制共用本实现。

import (
	"errors"
	"sync"
)

// ErrStopped 表示 Acquire 因 stop 通道关闭而中断返回。
var ErrStopped = errors.New("concurrency gate stopped")

// Gate 是并发闸。
type Gate struct {
	mu     sync.Mutex
	condCh chan struct{} // 资源释放或 cap 变化时 close+重建，唤醒等待者
	max    int
	used   int
}

// NewGate 创建一张不限流的闸。
func NewGate() *Gate { return &Gate{condCh: make(chan struct{})} }

// SetMax 更新并发上限；非正数表示不限。唤醒在等待的 Acquire 重新判定。
func (g *Gate) SetMax(n int) {
	g.mu.Lock()
	if n < 0 {
		n = 0
	}
	if g.max != n {
		g.max = n
		g.wakeLocked()
	}
	g.mu.Unlock()
}

// Max 返回并发上限。
func (g *Gate) Max() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.max
}

// Usage 返回当前在用的资源数。
func (g *Gate) Usage() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used
}

// Acquire 占一个资源位；超限时排队等待。stop 非空时在其关闭时中断（返回
// ErrStopped）；为 nil 则只等空位，不因调用方关闭而中断。上限 0/负时立即通过。
func (g *Gate) Acquire(stop <-chan struct{}) error {
	for {
		g.mu.Lock()
		if g.max <= 0 || g.used < g.max {
			g.used++
			g.mu.Unlock()
			return nil
		}
		ch := g.condCh
		g.mu.Unlock()
		if stop == nil {
			<-ch
			continue
		}
		select {
		case <-stop:
			return ErrStopped
		case <-ch:
		}
	}
}

// Release 释放一个资源位并唤醒一个等待者；多释空操作。
func (g *Gate) Release() {
	g.mu.Lock()
	if g.used > 0 {
		g.used--
		g.wakeLocked()
	}
	g.mu.Unlock()
}

// wakeLocked 必须持锁调用：唤醒所有等待者重新判定（它们会各自 CAS 抢占）。
func (g *Gate) wakeLocked() {
	close(g.condCh)
	g.condCh = make(chan struct{})
}
