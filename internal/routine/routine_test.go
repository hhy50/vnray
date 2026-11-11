package routine

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/metrics"
)

func TestGoTracksActiveGauge(t *testing.T) {
	done := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	Go("test.routine.tracked", func() {
		once.Do(func() { close(started) })
		<-done
	})
	<-started

	g := gaugeFor("test.routine.tracked")
	if got := g.Value(); got < 1 {
		t.Fatalf("active gauge = %d, want >= 1 while running", got)
	}
	close(done)

	// 协程退出与计数归零之间没有同步点，轮询等待。
	deadline := time.Now().Add(2 * time.Second)
	for g.Value() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("active gauge = %d, want 0 after exit", g.Value())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGoCountsPanic(t *testing.T) {
	before := panicTotal.Value()
	saved := panicHandler
	// handler 调用完成前不能恢复 panicHandler，否则与被测协程的读取构成竞态。
	handled := make(chan struct{})
	panicHandler = func(name string, r any, stack []byte) {
		close(handled)
	}
	defer func() { panicHandler = saved }()

	Go("test.routine.panic", func() {
		panic("boom")
	})
	<-handled

	deadline := time.Now().Add(2 * time.Second)
	for panicTotal.Value() == before {
		if time.Now().After(deadline) {
			t.Fatalf("panic counter stuck at %d", before)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGaugeMetricNameHasLabel(t *testing.T) {
	g := gaugeFor("test.routine.named")
	if got := g.Value(); got < 0 {
		t.Fatalf("unexpected negative gauge: %d", got)
	}
	for _, s := range metrics.Snapshot() {
		if s.Name == `routine_goroutines{goroutine="test.routine.named"}` {
			return
		}
	}
	t.Fatalf("metric name not found in snapshot: want routine_goroutines{goroutine=...}, got %s",
		labelHint())
}

func labelHint() string {
	var names []string
	for _, s := range metrics.Snapshot() {
		if strings.HasPrefix(s.Name, "routine_goroutines") {
			names = append(names, s.Name)
		}
	}
	return strings.Join(names, ", ")
}

func TestGoLifecycleAcrossMetricsToggle(t *testing.T) {
	for _, initiallyEnabled := range []bool{false, true} {
		metrics.SetEnabled(initiallyEnabled)
		g := gaugeFor("test.routine.toggle")
		entered, release := make(chan struct{}), make(chan struct{})
		Go("test.routine.toggle", func() { close(entered); <-release })
		<-entered
		if g.Value() != 1 {
			t.Errorf("active while enabled=%v: %d", initiallyEnabled, g.Value())
		}
		metrics.SetEnabled(!initiallyEnabled)
		close(release)
		deadline := time.Now().Add(2 * time.Second)
		for g.Value() != 0 {
			if time.Now().After(deadline) {
				t.Fatalf("unbalanced gauge: %d", g.Value())
			}
			time.Sleep(time.Millisecond)
		}
	}
	metrics.SetEnabled(true)
}
