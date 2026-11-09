package admin

import (
	"sync"
	"testing"
)

func TestConcurrentLoginFailures(t *testing.T) {
	limiter := newLoginLimiter()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range loginMaxFails {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			limiter.fail("same-account")
			limiter.allowed("same-account")
		}()
	}
	close(start)
	wg.Wait()
	if allowed, _ := limiter.allowed("same-account"); allowed {
		t.Fatal("concurrent failures did not lock out login")
	}
	limiter.succeed("same-account")
	if allowed, _ := limiter.allowed("same-account"); !allowed {
		t.Fatal("successful login did not clear failures")
	}
}
