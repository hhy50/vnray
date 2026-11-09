package vmess

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentReplayChecks(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "fresh"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			history := newSessionHistory()
			if expired {
				history.seen.Store(sessionKey{}, time.Now().Add(-2*sessionHistoryTTL))
			}
			filter := newReplayFilter()
			filter.lastGC.Store(time.Now().Add(-2 * time.Minute).UnixNano())
			history.lastGC.Store(time.Now().Add(-2 * time.Minute).UnixNano())
			start := make(chan struct{})
			var wg sync.WaitGroup
			var authAccepted, sessionAccepted atomic.Int64
			for range 64 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if filter.Check(authID{}) {
						authAccepted.Add(1)
					}
					if history.Check([16]byte{}, [16]byte{}, [16]byte{}) {
						sessionAccepted.Add(1)
					}
				}()
			}
			close(start)
			wg.Wait()
			if authAccepted.Load() != 1 || sessionAccepted.Load() != 1 {
				t.Fatalf("accepted auth=%d session=%d, want one each", authAccepted.Load(), sessionAccepted.Load())
			}
		})
	}
}
