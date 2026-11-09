package shadowsocks

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentSaltReplay(t *testing.T) {
	filter := newSaltFilter()
	filter.lastGC.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	start := make(chan struct{})
	var wg sync.WaitGroup
	var accepted atomic.Int64
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if filter.Check([]byte("same-session-salt")) {
				accepted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d repeated salts, want one", accepted.Load())
	}
}
