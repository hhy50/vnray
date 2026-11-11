package metrics

import (
	"math"
	"sync"
	"testing"
)

func TestCounterAndGauge(t *testing.T) {
	c := NewCounter("test_counter_total")
	c.Inc()
	c.Add(5)
	c.Add(-1)
	if got := c.Value(); got != 6 {
		t.Fatalf("counter value = %d, want 6", got)
	}

	g := NewGauge("test_gauge")
	g.Set(10)
	g.Add(-4)
	if got := g.Value(); got != 6 {
		t.Fatalf("gauge value = %d, want 6", got)
	}

	if NewCounter("test_counter_total").s != c.s {
		t.Fatal("duplicate registration should return the same series")
	}
}

func TestCounterRejectsNegative(t *testing.T) {
	c := NewCounter("test_negative_total")
	c.Add(3)
	c.Add(-2)
	if got := c.Value(); got != 3 {
		t.Fatalf("counter value = %d, want 3 (negative adds ignored)", got)
	}
}

func TestHistogramBucketsAndSum(t *testing.T) {
	h := NewHistogram("test_histogram_seconds", []float64{0.5, 1})
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
		if snaps[i].Name == "test_histogram_seconds" {
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
	NewCounter("zz_last_total")
	NewGauge("aa_first")
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
	c := NewCounter("test_gate_total")
	g := NewGauge("test_gate_gauge")
	h := NewHistogram("test_gate_seconds", DurationBounds)
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
	if got := g.Value(); got != beforeG {
		t.Fatalf("gauge changed while disabled: %d -> %d", beforeG, got)
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
	NewGauge("test_dup_name")
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on conflicting registration")
		}
	}()
	NewCounter("test_dup_name")
}

func TestConcurrentObserve(t *testing.T) {
	h := NewHistogram("test_concurrent_seconds", DurationBounds)
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
		if s.Name == "test_concurrent_seconds" {
			for _, b := range s.Buckets {
				total += b.Count
			}
		}
	}
	if total != goroutines*perG {
		t.Fatalf("bucket total = %d, want %d", total, goroutines*perG)
	}
}
