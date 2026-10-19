package traffic

import (
	"context"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/store"
)

// TestSetFlushIntervalHotReload 断言落库周期热改即时生效：默认周期是 10s，这里改成
// 10ms 后必须在很短时间内看到增量落库，否则说明 reload 没起作用。
func TestSetFlushIntervalHotReload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := testStore(t)
	m := NewMeter(nil)
	m.SetFlushInterval(10 * time.Millisecond)
	m.Run(ctx, st)

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
