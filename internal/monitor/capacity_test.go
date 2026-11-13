package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/metrics"
)

const (
	capacityUsers   = 10
	capacitySamples = 7 * 24 * 60 * 60 / 2
	capacitySeed    = 20261113
)

type capacityReport struct {
	Engine              string    `json:"engine"`
	GoVersion           string    `json:"go_version"`
	Platform            string    `json:"platform"`
	Users               int       `json:"users"`
	Samples             int       `json:"samples"`
	IntervalSeconds     int       `json:"interval_seconds"`
	PartitionSeconds    int       `json:"partition_seconds"`
	StoredSamples       int       `json:"stored_samples"`
	Partitions          int       `json:"partitions"`
	TotalBytes          int64     `json:"total_bytes"`
	SampledWriteHeapMax uint64    `json:"sampled_write_heap_alloc_max_bytes"`
	ElapsedSeconds      float64   `json:"elapsed_seconds"`
	First               time.Time `json:"first"`
	Last                time.Time `json:"last"`
}

func TestHistorySevenDayCapacity(t *testing.T) {
	if os.Getenv("VNRAY_MONITOR_CAPACITY") != "1" {
		t.Skip("set VNRAY_MONITOR_CAPACITY=1 for the opt-in disk test")
	}
	root := os.Getenv("VNRAY_MONITOR_CAPACITY_DIR")
	if root == "" {
		root = t.TempDir()
	}
	if os.Getenv("VNRAY_MONITOR_CAPACITY_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHistorySevenDayCapacity$", "-test.v", "-test.timeout=60m")
		cmd.Env = append(os.Environ(), "VNRAY_MONITOR_CAPACITY_CHILD=1", "VNRAY_MONITOR_CAPACITY_DIR="+root)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		return
	}
	samples := capacitySamples
	if raw := os.Getenv("VNRAY_MONITOR_CAPACITY_SAMPLES"); raw != "" {
		var err error
		samples, err = strconv.Atoi(raw)
		if err != nil || samples < 2 || samples > capacitySamples {
			t.Fatalf("invalid sample count %q", raw)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("capacity output directory must be empty")
	}
	g := newCapacityGenerator()
	first, last := g.next(0).SampledAt, g.next(samples-1).SampledAt
	path := filepath.Join(root, "history")
	h, err := newHistory(path, 7*24*time.Hour, func() time.Time { return last })
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	report := capacityReport{Engine: "tstorage", GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH, Users: capacityUsers, Samples: samples, IntervalSeconds: 2, PartitionSeconds: int(historyPartitionDuration.Seconds()), First: first, Last: last}
	started, progress := time.Now(), time.Now()
	g = newCapacityGenerator()
	for i := range samples {
		if err := h.Save(context.Background(), g.next(i)); err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		if i%1000 == 0 {
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			report.SampledWriteHeapMax = max(report.SampledWriteHeapMax, mem.HeapAlloc)
		}
		if time.Since(progress) > 20*time.Second {
			t.Logf("written %d/%d", i+1, samples)
			progress = time.Now()
		}
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	report.ElapsedSeconds = time.Since(started).Seconds()
	if err := filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		report.TotalBytes += info.Size()
		if entry.Name() == "meta.json" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var meta struct {
				NumDataPoints int `json:"numDataPoints"`
			}
			if err := json.Unmarshal(data, &meta); err != nil {
				return err
			}
			if meta.NumDataPoints > 0 {
				report.Partitions++
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reader, err := newHistory(path, 7*24*time.Hour, func() time.Time { return last })
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	markers, err := reader.selectPoints(context.Background(), sampleMetric, nil, first.UnixMilli(), last.UnixMilli()+1)
	if err != nil || len(markers) != samples {
		t.Fatalf("markers: %d, want %d: %v", len(markers), samples, err)
	}
	for i, marker := range markers {
		if marker.Timestamp != first.Add(time.Duration(i)*2*time.Second).UnixMilli() || marker.Value != 1 {
			t.Fatalf("marker %d mismatch", i)
		}
	}
	report.StoredSamples = len(markers)
	ids := make([]uint64, capacityUsers)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	rows, err := reader.Read(context.Background(), first, last, 120, ids)
	if err != nil || len(rows) != min(samples, 120) {
		t.Fatalf("history: %d rows: %v", len(rows), err)
	}
	g = newCapacityGenerator()
	selected := 0
	for i := range samples {
		want := g.next(i)
		if selected == len(rows) || !want.SampledAt.Equal(rows[selected].SampledAt) {
			continue
		}
		expected, err := snapshotRows(want)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := snapshotRows(rows[selected])
		if err != nil || !sameRows(actual, expected) {
			t.Fatalf("restored snapshot %d differs: %v", i, err)
		}
		selected++
	}
	if selected != len(rows) || !rows[0].SampledAt.Equal(first) || !rows[len(rows)-1].SampledAt.Equal(last) {
		t.Fatal("history endpoints lost")
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "report.json"), append(data, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", data)
}

type capacityGenerator struct {
	random *rand.Rand
	sample Snapshot
}

func newCapacityGenerator() *capacityGenerator {
	return &capacityGenerator{
		random: rand.New(rand.NewPCG(capacitySeed, 1)),
		sample: Snapshot{
			Users:   make([]User, capacityUsers),
			Server:  metrics.ServerInfo{MemoryTotal: testPointer(uint64(8 << 30)), Interfaces: 2},
			Process: metrics.ProcessInfo{Gomaxprocs: 8, NextGC: 256 << 20},
		},
	}
}

func (g *capacityGenerator) next(index int) Snapshot {
	s := &g.sample
	s.SampledAt = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(index) * 2 * time.Second)
	var up, down float64
	for i := range s.Users {
		u := &s.Users[i]
		u.ID, u.Name, u.Protocol = uint64(i+1), fmt.Sprintf("user-%02d", i+1), "vmess"
		active := (index/150+i)%4 != 0
		var upBytes, downBytes int64
		if active {
			upBytes = int64(1024 + g.random.IntN(256*1024))
			downBytes = int64(4096 + g.random.IntN(2*1024*1024))
			u.TCP = int64(1 + g.random.IntN(5))
			u.Requests += int64(g.random.IntN(8))
		} else {
			u.TCP = 0
		}
		u.Up += upBytes
		u.Down += downBytes
		u.UpRate, u.DownRate = testPointer(float64(upBytes)/2), testPointer(float64(downBytes)/2)
		up += *u.UpRate
		down += *u.DownRate
		if index == 0 {
			u.UpRate, u.DownRate = nil, nil
		}
	}
	s.Server.CPUPercent = testPointer(15 + 10*math.Sin(float64(index)/1800) + g.random.Float64()*15)
	s.Server.MemoryUsed = testPointer(uint64(2<<30) + uint64(g.random.IntN(256<<20)))
	s.Server.ReceiveBytesPerSecond = testPointer(down + g.random.Float64()*4096)
	s.Server.TransmitBytesPerSecond = testPointer(up + g.random.Float64()*4096)
	s.Process.CPUPercent = testPointer(5 + g.random.Float64()*30)
	s.Process.ResidentBytes = testPointer(uint64(128<<20) + uint64(g.random.IntN(32<<20)))
	s.Process.Goroutines = uint64(100 + g.random.IntN(50))
	s.Process.HeapAlloc = uint64(32<<20) + uint64(index%60)*(1<<20) + uint64(g.random.IntN(1<<20))
	s.Process.HeapObjects = uint64(20000 + g.random.IntN(10000))
	s.Process.GoMemory = s.Process.HeapAlloc + 16<<20
	s.Process.GCCount = uint64(index / 60)
	s.Process.GCPauseP99Seconds = testPointer(0.0001 + g.random.Float64()*0.0005)
	s.Process.GCCPUPercent = testPointer(g.random.Float64() * 2)
	return *s
}
