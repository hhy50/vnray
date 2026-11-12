package traffic

import (
	"sync"
	"testing"

	"github.com/hhy50/vnray/internal/store"
)

func TestMonitoringConcurrentLifecycleAndRecreation(t *testing.T) {
	m := NewMeter(nil)
	m.RestoreUser(store.ProtocolVNV1, "alice")
	stats := m.UserStats(store.ProtocolVNV1, "alice")
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats.AddTCP(1)
			defer stats.AddTCP(-1)
			defer stats.OpenStream().CloseStream()
			stats.Request()
		}()
	}
	wg.Wait()
	if stats.TCP.Load() != 0 || stats.Streams.Load() != 0 || stats.Requests.Load() != 100 {
		t.Fatal("unbalanced concurrent lifecycle")
	}
	stats.AddTCP(1)
	stream := stats.OpenStream()
	m.ForgetUser(store.ProtocolVNV1, "alice")
	m.RestoreUser(store.ProtocolVNV1, "alice")
	next := m.UserStats(store.ProtocolVNV1, "alice")
	stats.AddTCP(-1)
	stream.CloseStream()
	if next == stats || next.TCP.Load() != 0 || next.Streams.Load() != 0 || next.Requests.Load() != 0 {
		t.Fatal("old lifecycle affected recreated user")
	}
}
