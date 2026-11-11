package metrics

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestProcParsers(t *testing.T) {
	total, idle, ok := parseCPU("cpu 100 2 30 400 10 5 3 1 50 2\ncpu0 1 2 3 4")
	if !ok || total != 551 || idle != 410 {
		t.Fatalf("cpu = %d, %d, %v", total, idle, ok)
	}
	if _, _, ok := parseCPU("cpu 10 bad 20 30"); ok {
		t.Fatal("malformed cpu accepted")
	}
	memory, used := parseMemory("MemTotal: 1000 kB\nMemAvailable: 300 kB\n")
	if *memory != 1024000 || *used != 716800 {
		t.Fatalf("memory = %v / %v", memory, used)
	}
	if _, used := parseMemory("MemTotal: 100 kB\nMemAvailable: 300 kB\n"); used != nil {
		t.Fatal("memory underflow")
	}
	if _, used := parseMemory("MemTotal: 100 kB\n"); used != nil {
		t.Fatal("missing available memory must be unavailable")
	}
	if parseNetwork("malformed input") != nil {
		t.Fatal("invalid network snapshot must be unavailable")
	}
	network := parseNetwork("lo: 10 0 0 0 0 0 0 0 20 0 0 0 0 0 0 0\neth0: 100 0 0 0 0 0 0 0 200 0 0 0 0 0 0 0\nbad: x")
	if len(network) != 1 || network["eth0"].transmit != 200 {
		t.Fatalf("network = %+v", network)
	}
}

func TestRatesAndCounterReset(t *testing.T) {
	previous := systemReading{cpuValid: true, cpuTotal: 100, cpuIdle: 50, network: map[string]networkCounter{"eth0": {100, 200}, "gone": {10000, 10000}}}
	current := systemReading{cpuValid: true, cpuTotal: 200, cpuIdle: 80, network: map[string]networkCounter{"eth0": {200, 400}, "new": {90000, 90000}}}
	first := serverInfo(current, previous, 2, false)
	if first.CPUPercent != nil || first.ReceiveBytesPerSecond != nil {
		t.Fatal("first sample has no rate baseline")
	}
	second := serverInfo(current, previous, 2, true)
	if *second.CPUPercent != 70 || *second.ReceiveBytesPerSecond != 50 || *second.TransmitBytesPerSecond != 100 {
		t.Fatalf("rates = %+v", second)
	}
	reset := serverInfo(previous, current, 2, true)
	if reset.CPUPercent != nil || *reset.ReceiveBytesPerSecond != 0 || *reset.TransmitBytesPerSecond != 0 {
		t.Fatalf("counter reset = %+v", reset)
	}
	if rate(1, 2, 1, 100) != nil || rate(2, 1, 0, 100) != nil {
		t.Fatal("invalid process rate")
	}
	if *rate(3, 1, 1, 100) != 200 {
		t.Fatal("multicore process CPU was clamped")
	}
}

func TestSamplerRuntimeAndReset(t *testing.T) {
	s := NewSampler()
	first := s.Collect()
	if first.Process.Goroutines == 0 || first.Process.HeapAlloc == 0 {
		t.Fatalf("runtime = %+v", first.Process)
	}
	if _, err := json.Marshal(first); err != nil {
		t.Fatal(err)
	}
	s.Collect()
	s.Reset()
	if sample := s.Collect(); sample.Process.CPUPercent != nil {
		t.Fatal("reset retained rate baseline")
	}
	SetEnabled(false)
	SetEnabled(true)
	if sample := s.Collect(); sample.Process.CPUPercent != nil {
		t.Fatal("toggle retained rate baseline")
	}
}

func TestConcurrentGaugesAcrossToggle(t *testing.T) {
	resetTestRegistry(t)
	g := NewGauge("active")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				g.Add(1)
				g.Add(-1)
			}
		})
	}
	wg.Go(func() {
		for range 1000 {
			SetEnabled(false)
			SetEnabled(true)
		}
	})
	wg.Wait()
	if g.Value() != 0 {
		t.Fatalf("gauge = %d", g.Value())
	}
}

func BenchmarkRuntimeSample(b *testing.B) {
	s := NewSampler()
	b.ReportAllocs()
	for b.Loop() {
		s.Collect()
	}
}
