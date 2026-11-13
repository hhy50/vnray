package monitor

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type capacityQueryResult struct {
	Window         string  `json:"window"`
	Seconds        float64 `json:"seconds"`
	Points         int     `json:"points"`
	AllocatedBytes uint64  `json:"allocated_bytes"`
}

func TestHistoryCapacityQueries(t *testing.T) {
	root := os.Getenv("VNRAY_MONITOR_QUERY_DIR")
	if root == "" {
		t.Skip("set VNRAY_MONITOR_QUERY_DIR to an existing capacity output directory")
	}
	queryDir := os.Getenv("VNRAY_MONITOR_QUERY_CHILD_DIR")
	if queryDir == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHistoryCapacityQueries$", "-test.v", "-test.timeout=10m")
		cmd.Env = append(os.Environ(), "VNRAY_MONITOR_QUERY_CHILD_DIR="+t.TempDir())
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		return
	}
	data, err := os.ReadFile(filepath.Join(root, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var capacity capacityReport
	if err := json.Unmarshal(data, &capacity); err != nil {
		t.Fatal(err)
	}
	if capacity.Engine != "tstorage" {
		t.Fatal("capacity input must use tstorage")
	}
	source := filepath.Join(root, "history")
	path := filepath.Join(queryDir, "history")
	if err := filepath.WalkDir(source, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		target := filepath.Join(path, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	}); err != nil {
		t.Fatal(err)
	}
	opened := time.Now()
	history, err := newHistory(path, 7*24*time.Hour, func() time.Time { return capacity.Last })
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	report := struct {
		OpenSeconds float64               `json:"open_seconds"`
		Queries     []capacityQueryResult `json:"queries"`
	}{OpenSeconds: time.Since(opened).Seconds()}
	ids := make([]uint64, capacity.Users)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	windows := []time.Duration{5 * time.Minute, time.Hour, 7 * 24 * time.Hour}
	if raw := os.Getenv("VNRAY_MONITOR_QUERY_WINDOWS"); raw != "" {
		windows = nil
		for _, value := range strings.Split(raw, ",") {
			window, err := time.ParseDuration(value)
			if err != nil || window < 2*time.Duration(capacity.IntervalSeconds)*time.Second || window > 7*24*time.Hour || window%(time.Duration(capacity.IntervalSeconds)*time.Second) != 0 {
				t.Fatalf("invalid query window %q", value)
			}
			windows = append(windows, window)
		}
	}
	for _, window := range windows {
		start := capacity.Last.Add(-window + time.Duration(capacity.IntervalSeconds)*time.Second)
		if start.Before(capacity.First) {
			start = capacity.First
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		t.Logf("querying %s for %d users", window, capacity.Users)
		started := time.Now()
		rows, err := history.Read(context.Background(), start, capacity.Last, 120, ids)
		seconds := time.Since(started).Seconds()
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		want := min(120, int(capacity.Last.Sub(start)/(time.Duration(capacity.IntervalSeconds)*time.Second))+1)
		if len(rows) != want || !rows[0].SampledAt.Equal(start) || !rows[len(rows)-1].SampledAt.Equal(capacity.Last) {
			t.Fatalf("query %s: unexpected sample count or endpoints", window)
		}
		for _, row := range rows {
			if len(row.Users) != capacity.Users {
				t.Fatalf("query %s: incomplete users", window)
			}
		}
		report.Queries = append(report.Queries, capacityQueryResult{Window: window.String(), Seconds: seconds, Points: len(rows), AllocatedBytes: after.TotalAlloc - before.TotalAlloc})
		t.Logf("query %s: %.3fs, %d snapshots, %d bytes allocated", window, seconds, len(rows), after.TotalAlloc-before.TotalAlloc)
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "query-report.json"), append(data, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
}
