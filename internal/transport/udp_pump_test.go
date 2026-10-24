package transport

// ProxyDatagramBidirectional 的单元测试：双向搬运、上行限速计费、两侧统计、单向 EOF
// 整体收尾、空闲超时关停。泵的两个端点各包一层 net.Pipe 的对端，测试从对端注入/读取
// 全部含封帧。

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/protocol"
)

// TestProxyDatagramBidirectional 双向搬运、上行限速计费、两侧统计与收尾。
func TestProxyDatagramBidirectional(t *testing.T) {
	a, aPeer := net.Pipe()
	b, bPeer := net.Pipe()
	aD := protocol.NewDatagramConn(a)
	bD := protocol.NewDatagramConn(b)
	aT := protocol.NewDatagramConn(aPeer)
	bT := protocol.NewDatagramConn(bPeer)
	defer aPeer.Close()

	lim := &countingLimiter{}
	stats := &PumpStats{}
	done := make(chan struct{})
	go func() {
		ProxyDatagramBidirectional(aD, bD, "t", 0, ProxyOptions{UpLimit: lim, Stats: stats})
		close(done)
	}()

	// 上行：从 a 侧对端注入 → 从 b 侧对端收到。
	if err := aT.WritePacket([]byte("hello")); err != nil {
		t.Fatalf("write up: %v", err)
	}
	got, err := bT.ReadPacket()
	if err != nil || string(got) != "hello" {
		t.Fatalf("up packet = %q, %v", got, err)
	}
	// 下行：从 b 侧对端注入 → 从 a 侧对端收到。
	if err := bT.WritePacket([]byte("world")); err != nil {
		t.Fatalf("write down: %v", err)
	}
	got, err = aT.ReadPacket()
	if err != nil || string(got) != "world" {
		t.Fatalf("down packet = %q, %v", got, err)
	}

	// 上行只有 5 字节被限速器计费。统计由泵协程在写返回后才递增，本测试与泵
	// 之间没有同步点（net.Pipe 的读只保证数据到位），轮询等待统计收敛。
	waitFor := func(want int64) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if lim.waited.Load() == want &&
				stats.UpRead.Load() == want && stats.UpWrite.Load() == want &&
				stats.DownRead.Load() == want && stats.DownWrite.Load() == want {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("stats not converged: limiter=%d up=%d/%d down=%d/%d, want %d",
			lim.waited.Load(), stats.UpRead.Load(), stats.UpWrite.Load(),
			stats.DownRead.Load(), stats.DownWrite.Load(), want)
	}
	waitFor(5)

	// 关闭 a 侧对端 → 泵读到 EOF → 整体收尾。
	_ = aPeer.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not return after one side closed")
	}
}

// TestProxyDatagramBidirectionalIdle 空闲超时整体关停两端。
func TestProxyDatagramBidirectionalIdle(t *testing.T) {
	a, aPeer := net.Pipe()
	b, bPeer := net.Pipe()
	aD := protocol.NewDatagramConn(a)
	bD := protocol.NewDatagramConn(b)
	done := make(chan struct{})
	go func() {
		ProxyDatagramBidirectional(aD, bD, "idle", 100*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not idle-close")
	}
	// 两端都被关停：从对端读到 EOF。
	if _, err := aPeer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("a not closed after idle, read err = %v", err)
	}
	if _, err := bPeer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("b not closed after idle, read err = %v", err)
	}
	_ = aPeer.Close()
	_ = bPeer.Close()
}

// TestProxyDatagramBidirectionalActivityResetsIdle 有流量时不会触发空闲关停。
func TestProxyDatagramBidirectionalActivityResetsIdle(t *testing.T) {
	a, aPeer := net.Pipe()
	b, bPeer := net.Pipe()
	aD := protocol.NewDatagramConn(a)
	bD := protocol.NewDatagramConn(b)
	aT := protocol.NewDatagramConn(aPeer)
	bT := protocol.NewDatagramConn(bPeer)
	defer aPeer.Close()

	done := make(chan struct{})
	go func() {
		ProxyDatagramBidirectional(aD, bD, "busy", 100*time.Millisecond)
		close(done)
	}()

	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = aT.WritePacket([]byte("ping"))
			_, _ = bT.ReadPacket()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	// 持续活动远超过 idle 时长，泵不应自行关停。
	time.Sleep(250 * time.Millisecond)
	_ = aT.WritePacket([]byte("probe"))
	if got, err := bT.ReadPacket(); err != nil || string(got) != "probe" {
		t.Fatalf("pump idle-closed or corrupt despite activity: %q, %v", got, err)
	}
	close(stop)
	time.Sleep(50 * time.Millisecond)
	_ = aPeer.Close()
}
