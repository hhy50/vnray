package concurrency

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestGateUncontendedAllocations(t *testing.T) {
	gate := NewGate()
	gate.SetMax(1)
	allocs := testing.AllocsPerRun(100, func() {
		if err := gate.Acquire(nil); err != nil {
			t.Fatal(err)
		}
		gate.Release()
	})
	if allocs != 0 {
		t.Fatalf("uncontended acquire/release allocated %g objects", allocs)
	}
}

type waitingContext struct {
	context.Context
	ready chan struct{}
	once  sync.Once
}

func (c *waitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.ready) })
	return c.Context.Done()
}

func TestGateRepeatedWakeups(t *testing.T) {
	for _, tc := range []struct {
		name string
		wake func(*Gate)
	}{
		{"release", func(g *Gate) { g.Release() }},
		{"raise-limit", func(g *Gate) { g.SetMax(2) }},
		{"unlimited", func(g *Gate) { g.SetMax(0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := NewGate()
			for range 3 {
				gate.SetMax(1)
				if err := gate.Acquire(nil); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				waiting := &waitingContext{Context: ctx, ready: make(chan struct{})}
				result := make(chan error, 1)
				go func() { result <- gate.AcquireContext(waiting) }()
				select {
				case <-waiting.ready:
				case <-ctx.Done():
					cancel()
					t.Fatal("acquire did not begin waiting")
				}
				tc.wake(gate)
				select {
				case err := <-result:
					cancel()
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					cancel()
					t.Fatal("waiter missed wakeup")
				}
				for gate.Usage() > 0 {
					gate.Release()
				}
			}
		})
	}
}

func BenchmarkGateUncontended(b *testing.B) {
	gate := NewGate()
	gate.SetMax(1)
	b.ReportAllocs()
	for b.Loop() {
		_ = gate.Acquire(nil)
		gate.Release()
	}
}
