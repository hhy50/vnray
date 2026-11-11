package metrics

import (
	"encoding/json"
	"math"
	"sync"
	"testing"
)

func TestCounterAndGauge(t *testing.T) {
	resetTestRegistry(t)
	c := NewCounter(t.Name() + "_test_counter_total")
	c.Inc()
	c.Add(5)
	c.Add(-1)
	if got := c.Value(); got != 6 {
		t.Fatalf("counter value = %d, want 6", got)
	}

	g := NewGauge(t.Name() + "_test_gauge")
	g.Set(10)
	g.Add(-4)
	if got := g.Value(); got != 6 {
		t.Fatalf("gauge value = %d, want 6", got)
	}

	if NewCounter(t.Name()+"_test_counter_total").s != c.s {
		t.Fatal("duplicate registration should return the same series")
	}
}

func TestCounterRejectsNegative(t *testing.T) {
	resetTestRegistry(t)
	c := NewCounter(t.Name() + "_test_negative_total")
	c.Add(3)
	c.Add(-2)
	if got := c.Value(); got != 3 {
		t.Fatalf("counter value = %d, want 3 (negative adds ignored)", got)
	}
}

func TestHistogramBucketsAndSum(t *testing.T) {
	resetTestRegistry(t)
	h := NewHistogram(t.Name()+"_test_histogram_seconds", []float64{0.5, 1})
	h.Observe(0.25)
	h.Observe(0.75)
	h.Observe(2)

	if got := h.Count(); got != 3 {
		t.Fatalf("count = %d, want 3", got)
	}
	if got := h.Sum(); math.Abs(got-3) > 1e-9 {
		t.Fatalf("sum = %v, want 3", got)
	}

	snaps := Snapshot()
	var found *Series
	for i := range snaps {
		if snaps[i].Name == t.Name()+"_test_histogram_seconds" {
			found = &snaps[i]
		}
	}
	if found == nil {
		t.Fatal("histogram missing from snapshot")
	}
	// 三只桶：<=0.5、<=1、+Inf。
	want := []int64{1, 1, 1}
	for i, b := range found.Buckets {
		if b.Count != want[i] {
			t.Fatalf("bucket %d (le=%v) count = %d, want %d", i, b.Upper, b.Count, want[i])
		}
	}
}

func TestSnapshotSortedAndKinds(t *testing.T) {
	resetTestRegistry(t)
	NewCounter("zz_last_total")
	NewGauge("aa_first")
	NewHistogram("middle_seconds", DurationBounds)
	snaps := Snapshot()
	for i := 1; i < len(snaps); i++ {
		if snaps[i-1].Name > snaps[i].Name {
			t.Fatalf("snapshot not sorted: %s before %s", snaps[i-1].Name, snaps[i].Name)
		}
	}
	var kinds = map[string]bool{}
	for _, s := range snaps {
		kinds[s.Type] = true
	}
	if !kinds[TypeCounter] || !kinds[TypeGauge] || !kinds[TypeHistogram] {
		t.Fatalf("missing kinds in snapshot: %v", kinds)
	}
}

func TestDisabledNoOps(t *testing.T) {
	resetTestRegistry(t)
	c := NewCounter(t.Name() + "_test_gate_total")
	g := NewGauge(t.Name() + "_test_gate_gauge")
	h := NewHistogram(t.Name()+"_test_gate_seconds", DurationBounds)
	c.Add(2)
	g.Set(7)
	h.Observe(0.001)
	before, beforeC, beforeG := h.Count(), c.Value(), g.Value()

	SetEnabled(false)
	defer SetEnabled(true)
	if Enabled() {
		t.Fatal("Enabled() = true after SetEnabled(false)")
	}
	c.Inc()
	c.Add(3)
	g.Add(-1)
	g.Set(0)
	h.Observe(0.002)

	if got := c.Value(); got != beforeC {
		t.Fatalf("counter changed while disabled: %d -> %d", beforeC, got)
	}
	if got := g.Value(); got != 0 {
		t.Fatalf("gauge must track state while disabled: %d -> %d", beforeG, got)
	}
	if got := h.Count(); got != before {
		t.Fatalf("histogram count changed while disabled: %d -> %d", before, got)
	}

	SetEnabled(true)
	if !Enabled() {
		t.Fatal("Enabled() = false after SetEnabled(true)")
	}
	c.Inc()
	if got := c.Value(); got != beforeC+1 {
		t.Fatalf("counter after re-enable = %d, want %d", got, beforeC+1)
	}
}

func TestDuplicateKindPanics(t *testing.T) {
	resetTestRegistry(t)
	NewGauge(t.Name() + "_test_dup_name")
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on conflicting registration")
		}
	}()
	NewCounter(t.Name() + "_test_dup_name")
}

func TestConcurrentObserve(t *testing.T) {
	resetTestRegistry(t)
	h := NewHistogram(t.Name()+"_test_concurrent_seconds", DurationBounds)
	const goroutines, perG = 8, 1000
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				h.Observe(0.001)
			}
		}()
	}
	wg.Wait()
	if got := h.Count(); got != goroutines*perG {
		t.Fatalf("count = %d, want %d", got, goroutines*perG)
	}
	var total int64
	snaps := Snapshot()
	for _, s := range snaps {
		if s.Name == t.Name()+"_test_concurrent_seconds" {
			for _, b := range s.Buckets {
				total += b.Count
			}
		}
	}
	if total != goroutines*perG {
		t.Fatalf("bucket total = %d, want %d", total, goroutines*perG)
	}
}

func resetTestRegistry(t *testing.T) {
	t.Helper()
	mu.Lock()
	seriesByName = map[string]*series{}
	registeredSeries.Store(nil)
	mu.Unlock()
	SetEnabled(true)
	t.Cleanup(func() { SetEnabled(true) })
}

func TestGaugeLifecycleAcrossToggle(t *testing.T) {
	resetTestRegistry(t)
	g := NewGauge("active")
	SetEnabled(false)
	g.Add(1)
	SetEnabled(true)
	g.Add(-1)
	if g.Value() != 0 {
		t.Fatalf("unbalanced lifecycle: %d", g.Value())
	}
	g.Add(1)
	SetEnabled(false)
	g.Add(-1)
	SetEnabled(true)
	if g.Value() != 0 {
		t.Fatalf("stale lifecycle: %d", g.Value())
	}
}

func TestHistogramJSON(t *testing.T) {
	resetTestRegistry(t)
	h := NewHistogram("latency", []float64{1})
	for _, v := range []float64{0.5, 2, math.NaN(), math.Inf(1), -1} {
		h.Observe(v)
	}
	data, err := json.Marshal(Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) || h.Count() != 2 {
		t.Fatalf("invalid snapshot: %s", data)
	}
}

func BenchmarkGaugeAdd(b *testing.B) {
	g := NewGauge("benchmark_active")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			g.Add(1)
			g.Add(-1)
		}
	})
}
