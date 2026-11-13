package monitor

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"hash"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nakabonne/tstorage"
)

type precisionCapacityResult struct {
	Precision    tstorage.TimestampPrecision `json:"precision"`
	Resolution   string                      `json:"resolution"`
	Phase        string                      `json:"phase"`
	Samples      int                         `json:"samples"`
	Series       int                         `json:"series"`
	DataBytes    int64                       `json:"data_bytes"`
	IndexBytes   int64                       `json:"index_bytes"`
	TotalBytes   int64                       `json:"total_bytes"`
	WriteSeconds float64                     `json:"write_seconds"`
}

func newTestPrecisionStorage(t *testing.T, path string, precision tstorage.TimestampPrecision) *History {
	t.Helper()
	st, err := tstorage.NewStorage(
		tstorage.WithDataPath(path),
		tstorage.WithPartitionDuration(historyPartitionDuration),
		tstorage.WithRetention(7*24*time.Hour),
		tstorage.WithTimestampPrecision(precision),
		tstorage.WithWALBufferedSize(-1),
	)
	if err != nil {
		t.Fatal(err)
	}
	h := &History{gate: make(chan struct{}, 1), store: st}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}

func TestHistoryTimestampPrecisionCapacity(t *testing.T) {
	if os.Getenv("VNRAY_MONITOR_PRECISION") != "1" {
		t.Skip("set VNRAY_MONITOR_PRECISION=1 for the opt-in precision comparison")
	}
	samples := 10800
	if raw := os.Getenv("VNRAY_MONITOR_PRECISION_SAMPLES"); raw != "" {
		var err error
		samples, err = strconv.Atoi(raw)
		if err != nil || samples < 2 || samples > capacitySamples {
			t.Fatal("invalid precision comparison sample count")
		}
	}
	root := os.Getenv("VNRAY_MONITOR_PRECISION_DIR")
	if root == "" {
		root = t.TempDir()
	}
	var phase time.Duration
	if raw := os.Getenv("VNRAY_MONITOR_PRECISION_PHASE"); raw != "" {
		var err error
		phase, err = time.ParseDuration(raw)
		if err != nil || phase < 0 || phase >= historyTimestampResolution {
			t.Fatal("invalid precision comparison phase")
		}
	}
	results := make([]precisionCapacityResult, 0, 3)
	for _, resolution := range []time.Duration{time.Nanosecond, time.Millisecond, historyTimestampResolution} {
		precision := tstorage.Milliseconds
		unit := time.Millisecond
		if resolution == time.Nanosecond {
			precision, unit = tstorage.Nanoseconds, time.Nanosecond
		}
		path := filepath.Join(root, resolution.String())
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("comparison output must not exist: %s", path)
		}
		writer := newTestPrecisionStorage(t, path, precision)
		g := newCapacityGenerator()
		g.sample.Users[0].Up = 1<<53 + 1
		var keys []tstorage.Row
		var expected []hash.Hash
		var first, last time.Time
		var encoded [16]byte
		started := time.Now()
		for i := range samples {
			sample := g.next(i)
			// 固定调度抖动，不改变流量生成器的随机序列。
			jitter := time.Duration((uint64(i)*2654435761 + 1234567) % 20000001)
			sample.SampledAt = sample.SampledAt.Add(phase + jitter)
			storedAt := sample.SampledAt.Truncate(resolution)
			if i == 0 {
				first = storedAt
			}
			last = storedAt
			rows, err := snapshotRows(sample)
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				keys = append([]tstorage.Row(nil), rows...)
				for range rows {
					expected = append(expected, sha256.New())
				}
			}
			for j := range rows {
				binary.LittleEndian.PutUint64(encoded[:8], uint64(storedAt.UnixNano()))
				binary.LittleEndian.PutUint64(encoded[8:], math.Float64bits(rows[j].Value))
				expected[j].Write(encoded[:])
				rows[j].Timestamp = storedAt.UnixNano() / int64(unit)
			}
			if err := writer.store.InsertRows(rows); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		result := precisionCapacityResult{Precision: precision, Resolution: resolution.String(), Phase: phase.String(), Samples: samples, Series: len(keys), WriteSeconds: time.Since(started).Seconds()}
		if err := filepath.WalkDir(path, func(name string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			result.TotalBytes += info.Size()
			switch entry.Name() {
			case "data":
				result.DataBytes += info.Size()
			case "meta.json":
				result.IndexBytes += info.Size()
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		reader := newTestPrecisionStorage(t, path, precision)
		for i, key := range keys {
			actual := sha256.New()
			count, until := 0, last.UnixNano()/int64(unit)+1
			// 分窗校验，避免上游 Select 拼接整周分区时产生平方级切片复制。
			for start := first.UnixNano() / int64(unit); start < until; {
				end := min(start+int64(time.Hour/unit), until)
				points, err := reader.store.Select(key.Metric, key.Labels, start, end)
				if err != nil {
					t.Fatalf("%s %s: %v", precision, key.Metric, err)
				}
				for _, p := range points {
					binary.LittleEndian.PutUint64(encoded[:8], uint64(p.Timestamp*int64(unit)))
					binary.LittleEndian.PutUint64(encoded[8:], math.Float64bits(p.Value))
					actual.Write(encoded[:])
				}
				count += len(points)
				start = end
			}
			if count != samples {
				t.Fatalf("%s %s: %d points, want %d", precision, key.Metric, count, samples)
			}
			if !bytes.Equal(actual.Sum(nil), expected[i].Sum(nil)) {
				t.Fatalf("%s %s: timestamp/value hash mismatch", precision, key.Metric)
			}
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "report.json"), append(data, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", data)
	if results[1].TotalBytes >= results[0].TotalBytes {
		t.Fatal("millisecond timestamps did not reduce disk usage")
	}
}
