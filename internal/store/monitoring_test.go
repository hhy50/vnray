package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestMonitoringRetentionAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "monitoring.db")
	st, err := Open(ctx, "sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Truncate(time.Millisecond).Add(-time.Minute)
	for i := range 20 {
		err := st.SaveMonitoring(ctx, &MonitoringSample{Timestamp: start.Add(time.Duration(i) * time.Second).UnixMilli(), Payload: "{}"}, start)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(ctx, "sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.MonitoringHistory(ctx, start, time.Now(), 5)
	if err != nil || len(rows) != 5 {
		t.Fatalf("history: %v, %v", rows, err)
	}
	if rows[0].Timestamp != start.UnixMilli() || rows[4].Timestamp != start.Add(19*time.Second).UnixMilli() {
		t.Fatal("downsampling lost the first or latest sample")
	}
	cutoff := start.Add(10 * time.Second)
	if err := st.SaveMonitoring(ctx, nil, cutoff); err != nil {
		t.Fatal(err)
	}
	rows, err = st.MonitoringHistory(ctx, start, time.Now(), 120)
	if err != nil || len(rows) != 10 || rows[0].Timestamp != cutoff.UnixMilli() {
		t.Fatalf("expired history: %v, %v", rows, err)
	}
	for _, limit := range []int{0, 1, 121} {
		if _, err := st.MonitoringHistory(ctx, start, time.Now(), limit); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := st.SaveMonitoring(canceled, nil, time.Now()); err == nil {
		t.Fatal("canceled cleanup succeeded")
	}
	if _, err := st.MonitoringHistory(canceled, start, time.Now(), 120); err == nil {
		t.Fatal("canceled history succeeded")
	}
}
