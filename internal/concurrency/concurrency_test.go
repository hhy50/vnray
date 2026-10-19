package concurrency

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGateAcquireRelease(t *testing.T) {
	g := NewGate()
	g.SetMax(2)
	if err := g.Acquire(nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Acquire(nil); err != nil {
		t.Fatal(err)
	}
	if g.Usage() != 2 {
		t.Fatalf("usage = %d, want 2", g.Usage())
	}
	unlocked := make(chan error, 1)
	go func() { unlocked <- g.Acquire(nil) }()
	// 让排队者进入等待，再释放一个位让它通过。
	time.Sleep(20 * time.Millisecond)
	g.Release()
	select {
	case err := <-unlocked:
		if err != nil {
			t.Fatalf("queued acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued acquire did not unblock after release")
	}
	if g.Usage() != 2 {
		t.Fatalf("usage = %d, want 2", g.Usage())
	}
	g.Release()
	g.Release()
	g.Release() // 多释空操作，不 panic
}

func TestGateUnlimited(t *testing.T) {
	g := NewGate() // max=0
	for i := 0; i < 100; i++ {
		if err := g.Acquire(nil); err != nil {
			t.Fatalf("unlimited acquire %d: %v", i, err)
		}
	}
}

func TestGateRaiseCapWakesWaiters(t *testing.T) {
	g := NewGate()
	g.SetMax(1)
	if err := g.Acquire(nil); err != nil {
		t.Fatal(err)
	}
	var unlocked atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.Acquire(nil); err == nil {
				unlocked.Add(1)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	// 调大 cap 到 4：让三个在等的都通过。
	g.SetMax(4)
	wg.Wait()
	if got := unlocked.Load(); got != 3 {
		t.Fatalf("unlocked = %d, want 3", got)
	}
}

func TestGateStopInterrupts(t *testing.T) {
	g := NewGate()
	g.SetMax(1)
	if err := g.Acquire(nil); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	close(stop) // 会话已关
	if err := g.Acquire(stop); err != ErrStopped {
		t.Fatalf("err = %v, want ErrStopped", err)
	}
}
