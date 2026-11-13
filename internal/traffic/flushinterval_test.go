package traffic

import (
	"context"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/store"
)

// TestSetFlushIntervalHotReload 验证缩短落库周期后增量及时写入。
func TestSetFlushIntervalHotReload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := testStore(t)
	m := NewMeter(nil)
	m.RestoreUser(store.ProtocolVNV1, "alice")
	m.SetFlushInterval(10 * time.Millisecond)
	done := m.Run(ctx, st)
	defer func() { cancel(); <-done }()

	m.UserStats(store.ProtocolVNV1, "alice").UpWrite.Add(42)

	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := st.ListUserTraffic(ctx)
		if err == nil && len(rows) == 1 && rows[0].Up >= 42 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("traffic not flushed within 2s: rows=%v err=%v", rows, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
