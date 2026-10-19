package transport

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingLimiter 记录被等待的字节总量；不真正睡眠。
type countingLimiter struct{ waited atomic.Int64 }

func (l *countingLimiter) Wait(_ context.Context, n int) error {
	l.waited.Add(int64(n))
	return nil
}

// slowLimiter 按 rate 字节/秒睡眠——令牌桶的最小替身，专给泵的节流测试用。
type slowLimiter struct{ rate float64 }

func (l *slowLimiter) Wait(ctx context.Context, n int) error {
	timer := time.NewTimer(time.Duration(float64(n) / l.rate * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// nopCloser 什么都不做的 Closer。
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// rwCloser 把任意的读端/写端拼成 ReadWriteCloser。
type rwCloser struct {
	io.Reader
	io.Writer
	io.Closer
}

// TestProxyBidirectionalLimitedAndStats 验证完整转发、按字节等待及双向统计。
func TestProxyBidirectionalLimitedAndStats(t *testing.T) {
	const total = 200 * 1024
	lim := &countingLimiter{}
	stats := &PumpStats{}

	a := rwCloser{
		Reader: strings.NewReader(strings.Repeat("x", total)),
		Writer: io.Discard,
		Closer: nopCloser{},
	}
	buf := &completedBuffer{total: total, done: make(chan struct{})}
	b := rwCloser{
		Reader: pumpEOF{done: buf.done},
		Writer: buf,
		Closer: nopCloser{},
	}

	done := make(chan struct{})
	go func() {
		ProxyBidirectional(a, b, "test.pump", ProxyOptions{UpLimit: lim, Stats: stats})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("泵没有在 5s 内结束")
	}

	if got := buf.Len(); got != total {
		t.Fatalf("b 收到 %d 字节, want %d", got, total)
	}
	if got := lim.waited.Load(); got != total {
		t.Fatalf("限速器被等待 %d 字节, want %d", got, total)
	}
	if got := stats.UpRead.Load(); got != total {
		t.Fatalf("UpRead = %d, want %d", got, total)
	}
	if got := stats.UpWrite.Load(); got != total {
		t.Fatalf("UpWrite = %d, want %d", got, total)
	}
	if got := stats.DownRead.Load(); got != 0 || stats.DownWrite.Load() != 0 {
		t.Fatalf("下行统计应为零, got %d/%d", stats.DownRead.Load(), stats.DownWrite.Load())
	}
}

// TestProxyBidirectionalThrottles 验证泵按字节等待，不依赖长时间采样。
func TestProxyBidirectionalThrottles(t *testing.T) {
	const total = 400 * 1024
	bucket := &slowLimiter{rate: 100 * 1024}

	a := rwCloser{
		Reader: strings.NewReader(strings.Repeat("x", total)),
		Writer: io.Discard,
		Closer: nopCloser{},
	}
	buf := &completedBuffer{total: total, done: make(chan struct{})}
	b := rwCloser{Reader: pumpEOF{done: buf.done}, Writer: buf, Closer: nopCloser{}}

	done := make(chan struct{})
	start := time.Now()
	go func() {
		ProxyBidirectional(a, b, "test.throttle", ProxyOptions{UpLimit: bucket})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("泵没有在 10s 内结束")
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond {
		t.Fatalf("400KB @100KB/s 只用了 %v，节流未生效", elapsed)
	}
	if buf.Len() != total {
		t.Fatalf("b 收到 %d 字节, want %d", buf.Len(), total)
	}
}

type pumpEOF struct{ done <-chan struct{} }

func (r pumpEOF) Read([]byte) (int, error) {
	<-r.done
	return 0, io.EOF
}

type completedBuffer struct {
	limitedBuffer
	total int
	done  chan struct{}
}

func (b *completedBuffer) Write(p []byte) (int, error) {
	n, err := b.limitedBuffer.Write(p)
	if b.Len() == b.total {
		close(b.done)
	}
	return n, err
}

type blockingLimiter struct {
	ready chan struct{}
	once  sync.Once
}

func (l *blockingLimiter) Wait(ctx context.Context, _ int) error {
	l.once.Do(func() { close(l.ready) })
	<-ctx.Done()
	return ctx.Err()
}

// recordCloser 记录 Close 是否被调用过（幂等），用于断言方向分离的关闭时序。
type recordCloser struct {
	once sync.Once
	done chan struct{}
}

func (c *recordCloser) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

// gatedReader 在 gate 关闭前一直阻塞，之后返回 EOF。
type gatedReader struct{ gate <-chan struct{} }

func (r gatedReader) Read([]byte) (int, error) {
	<-r.gate
	return 0, io.EOF
}

// TestProxyDirectionSeparation 验证方向分离：一侧干净读尽只关闭该侧的目的端
// （半关闭由端点自身提供），另一侧继续跑到自己的 EOF，两端都结束才整体收尾。
func TestProxyDirectionSeparation(t *testing.T) {
	bClosed := make(chan struct{})
	aClosed := make(chan struct{})
	a := rwCloser{Reader: strings.NewReader("upload"), Writer: io.Discard, Closer: &recordCloser{done: aClosed}}
	gate := make(chan struct{})
	b := rwCloser{Reader: gatedReader{gate: gate}, Writer: io.Discard, Closer: &recordCloser{done: bClosed}}

	done := make(chan struct{})
	go func() {
		ProxyBidirectional(a, b, "test.dirsep")
		close(done)
	}()

	// up 先读尽：只应关闭隧道侧 b；用户侧 a 必须保持，下载方向不因上行结束而中断。
	select {
	case <-bClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("up EOF did not close the tunnel side")
	}
	select {
	case <-aClosed:
		t.Fatal("user side was closed while the down direction was still running")
	default:
	}

	// down 读尽：用户侧关闭，整体收尾。
	close(gate)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not finish after the down direction ended")
	}
	select {
	case <-aClosed:
	default:
		t.Fatal("user side was not closed at teardown")
	}
}

func TestProxyCancelsLimiterOnDisconnect(t *testing.T) {
	a, peerA := net.Pipe()
	b, peerB := net.Pipe()
	defer peerA.Close()
	defer peerB.Close()
	lim := &blockingLimiter{ready: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		ProxyBidirectional(a, b, "test.disconnect", ProxyOptions{UpLimit: lim})
		close(done)
	}()
	if _, err := peerA.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lim.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("limiter did not start")
	}
	// 关泵自己持有的这一端：这一侧的读立即报错（io.ErrClosedPipe），走 abort 路径。
	// 关对端只会得到 io.EOF（干净结束，归方向分离管，见 TestProxyDirectionSeparation）。
	_ = b.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect did not release limiter")
	}
}

// limitedBuffer 是一个无限增长的内存写端。
type limitedBuffer struct{ data []byte }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *limitedBuffer) Len() int { return len(b.data) }

// eofReader 首次读即返回 EOF。
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }
