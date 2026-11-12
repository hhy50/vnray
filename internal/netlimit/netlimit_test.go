package netlimit

import (
	"net"
	"testing"
	"time"
)

// TestListenerCapsConns 验证上限生效：占满后不再交付新连接，关闭一条才继续。
func TestListenerCapsConns(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln := New(raw, 2)
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()

	var clients []net.Conn
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", raw.Addr().String())
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		clients = append(clients, c)
	}

	first := receive(t, accepted)
	second := receive(t, accepted)
	select {
	case c := <-accepted:
		_ = c.Close()
		t.Fatal("超过上限后仍交付了新连接")
	case <-time.After(150 * time.Millisecond):
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}
	third := receive(t, accepted)
	_ = second.Close()
	_ = third.Close()
}

// TestListenerReleasesOnce 验证重复 Close 不会多归还名额：否则上限会被绕过。
func TestListenerReleasesOnce(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln := New(raw, 1)
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()

	c1, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	t.Cleanup(func() { _ = c1.Close() })
	first := receive(t, accepted)
	_ = first.Close()
	_ = first.Close()

	c2, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	second := receive(t, accepted)

	// 名额只有一个：重复 Close 若多归还一次，这里会立刻收到第三条连接而不是超时。
	c3, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatalf("dial 3: %v", err)
	}
	t.Cleanup(func() { _ = c3.Close() })
	select {
	case c := <-accepted:
		_ = c.Close()
		t.Fatal("重复 Close 归还了多余名额")
	case <-time.After(150 * time.Millisecond):
	}
	_ = second.Close()
}

func TestNewZeroKeepsListener(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if got := New(raw, 0); got != raw {
		t.Fatalf("max=0 应原样返回监听器")
	}
}

func receive(t *testing.T, ch <-chan net.Conn) net.Conn {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("等待 Accept 超时")
		return nil
	}
}
