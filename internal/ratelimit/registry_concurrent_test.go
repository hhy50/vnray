package ratelimit

import (
	"sync"
	"testing"

	"github.com/hhy50/vnray/internal/concurrency"
)

func TestRegistryConcurrentIdentity(t *testing.T) {
	r := NewRegistry()
	const workers = 64
	pairs := make([]Pair, workers)
	gates := make([]*concurrency.Gate, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				pairs[i] = r.Pair("alice")
				gates[i] = r.Gate("alice")
			} else {
				gates[i] = r.Gate("alice")
				pairs[i] = r.Pair("alice")
			}
		}()
	}
	close(start)
	wg.Wait()
	for i := range workers {
		if pairs[i] != pairs[0] || gates[i] != gates[0] {
			t.Fatal("one identity received multiple quotas")
		}
	}
	r.Apply("alice", 100, 200)
	r.Concurrency("alice", 1)
	if err := gates[0].Acquire(nil); err != nil {
		t.Fatal(err)
	}
	defer gates[0].Release()
	r.Forget("alice")
	nextPair, nextGate := r.Pair("alice"), r.Gate("alice")
	if nextPair.Up == pairs[0].Up || nextPair.Down == pairs[0].Down || nextPair.Stats == pairs[0].Stats || nextGate == gates[0] {
		t.Fatal("forgotten identity retained runtime state")
	}
	if nextPair.Up.Rate() != 0 || nextPair.Down.Rate() != 0 || nextGate.Max() != 0 || nextGate.Usage() != 0 {
		t.Fatal("new identity inherited limits or usage")
	}
	if pairs[0].Up.Rate() != 100 || pairs[0].Down.Rate() != 200 || gates[0].Usage() != 1 {
		t.Fatal("forget changed an existing tunnel's quota")
	}
}
