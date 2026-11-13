package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/nakabonne/tstorage"

	"github.com/hhy50/vnray/internal/metrics"
)

func testPointer[T any](v T) *T { return &v }

func testSnapshot() Snapshot {
	return Snapshot{
		SampledAt: time.Now().UTC().Truncate(historyTimestampResolution),
		Server: metrics.ServerInfo{
			CPUPercent: testPointer(12.5), MemoryTotal: testPointer(uint64(math.MaxUint64)), MemoryUsed: testPointer(uint64(2 << 30)),
			ReceiveBytesPerSecond: testPointer(12345.6789), TransmitBytesPerSecond: testPointer(0.0), Interfaces: 2,
		},
		Process: metrics.ProcessInfo{
			CPUPercent: testPointer(123.25), ResidentBytes: testPointer(uint64(64 << 20)), Goroutines: 42, Gomaxprocs: 8,
			HeapAlloc: 1234567, HeapObjects: 4567, GoMemory: 12345678, NextGC: 9876543, GCCount: 12,
			GCPauseP99Seconds: testPointer(0.000123), GCCPUPercent: nil,
		},
		Users: []User{
			{ID: math.MaxUint64, Name: "alice", Protocol: "vmess", TCP: math.MinInt64, Requests: math.MaxInt64, Up: 1<<53 + 1, Down: 45678, UpRate: testPointer(0.0), DownRate: nil},
			{ID: 2, Name: "bob", Protocol: "ss", Requests: 12, UpRate: testPointer(0.25), DownRate: testPointer(4096.5)},
		},
	}
}

type countedStorage struct {
	tstorage.Storage
	writes int
	reads  int
	err    error
}

func (s *countedStorage) Select(metric string, labels []tstorage.Label, start, end int64) ([]*tstorage.DataPoint, error) {
	s.reads++
	return s.Storage.Select(metric, labels, start, end)
}

func (s *countedStorage) InsertRows(rows []tstorage.Row) error {
	s.writes++
	if s.err != nil {
		return s.err
	}
	return s.Storage.InsertRows(rows)
}
func newTestHistory(t *testing.T) (*History, *countedStorage) {
	t.Helper()
	h, err := newHistory("", time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countedStorage{Storage: h.store}
	h.store = counted
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	return h, counted
}

func TestHistorySeriesRoundTripAndDeduplication(t *testing.T) {
	h, db := newTestHistory(t)
	ctx := context.Background()
	sample := testSnapshot()
	first := sample.SampledAt
	if err := h.Save(ctx, sample); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		sample.SampledAt = sample.SampledAt.Add(2 * time.Second)
		sample.Users[0].Name = "renamed"
		sample.Users[0], sample.Users[1] = sample.Users[1], sample.Users[0]
		if err := h.Save(ctx, sample); err != nil {
			t.Fatal(err)
		}
	}
	if db.writes != 1 {
		t.Fatalf("unchanged values wrote %d batches", db.writes)
	}
	rows, err := h.Read(ctx, first, sample.SampledAt, 120, []uint64{sample.Users[0].ID, sample.Users[1].ID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("read: %v %v", rows, err)
	}
	want := sample
	want.SampledAt = first
	for i := range want.Users {
		want.Users[i].Name, want.Users[i].Protocol = "", ""
	}
	if !reflect.DeepEqual(rows[0], want) {
		t.Fatalf("numeric round trip: got %+v, want %+v", rows[0], want)
	}
	changes := []func(){
		func() { sample.Users[0].Requests++ },
		func() { sample.Users[0].UpRate = nil },
		func() { sample.Server.CPUPercent = testPointer(13.0) },
		func() { sample.Process.HeapAlloc++ },
		func() { sample.Users = sample.Users[:1] },
	}
	for _, change := range changes {
		before := db.writes
		change()
		sample.SampledAt = sample.SampledAt.Add(2 * time.Second)
		if err := h.Save(ctx, sample); err != nil {
			t.Fatal(err)
		}
		if db.writes != before+1 {
			t.Fatal("changed values were not written")
		}
		if err := h.Save(ctx, sample); err != nil || db.writes != before+1 {
			t.Fatalf("duplicate write: %v", err)
		}
	}
	rows, err = h.Read(ctx, first, sample.SampledAt, 2, []uint64{sample.Users[0].ID})
	if err != nil || len(rows) != 2 || !rows[0].SampledAt.Equal(first) || !rows[1].SampledAt.Equal(sample.SampledAt) {
		t.Fatalf("downsampling: %v %v", rows, err)
	}
	for _, row := range rows {
		if len(row.Users) != 1 {
			t.Fatal("read returned unrequested users")
		}
	}
}

func TestHistoryWriteFailureCancellationAndClose(t *testing.T) {
	h, db := newTestHistory(t)
	sample := testSnapshot()
	db.err = errors.New("write failure")
	for range 2 {
		if err := h.Save(context.Background(), sample); err == nil {
			t.Fatal("write failure hidden")
		}
		if h.last != nil {
			t.Fatal("failed write advanced baseline")
		}
	}
	db.err = nil
	if err := h.Save(context.Background(), sample); err != nil || db.writes != 3 {
		t.Fatalf("retry: writes=%d err=%v", db.writes, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.Save(ctx, sample); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := h.Read(ctx, sample.SampledAt, sample.SampledAt, 120, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := h.Configure(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Save(context.Background(), sample); err == nil {
		t.Fatal("wrote after close")
	}
}

type blockedStorage struct {
	tstorage.Storage
	entered chan struct{}
	release chan struct{}
}

func (s *blockedStorage) InsertRows(rows []tstorage.Row) error {
	close(s.entered)
	<-s.release
	return s.Storage.InsertRows(rows)
}
func TestHistoryCloseWaitsForWrite(t *testing.T) {
	h, db := newTestHistory(t)
	blocked := &blockedStorage{Storage: db, entered: make(chan struct{}), release: make(chan struct{})}
	h.store = blocked
	written := make(chan error, 1)
	sample := testSnapshot()
	go func() { written <- h.Save(context.Background(), sample) }()
	<-blocked.entered
	ctx, cancel := context.WithCancel(context.Background())
	read := make(chan error, 1)
	go func() { _, err := h.Read(ctx, sample.SampledAt, sample.SampledAt, 120, nil); read <- err }()
	cancel()
	if err := <-read; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closing, closed := make(chan struct{}), make(chan error, 1)
	go func() { close(closing); closed <- h.Close() }()
	<-closing
	select {
	case <-closed:
		t.Fatal("closed during write")
	default:
	}
	close(blocked.release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if db.writes != 1 {
		t.Fatal("in-flight write was lost")
	}
}

func TestHistoryDuplicateSave(t *testing.T) {
	h, _ := newTestHistory(t)
	sample := testSnapshot()
	if err := h.Save(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	if err := h.Save(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	rows, err := h.Read(context.Background(), sample.SampledAt, sample.SampledAt, 120, []uint64{sample.Users[0].ID})
	if err != nil || len(rows) != 1 || len(rows[0].Users) != 1 {
		t.Fatalf("duplicate recovered points: %v %v", rows, err)
	}
}

func TestHistoryDiskPersistenceAndAbruptExit(t *testing.T) {
	for _, mode := range []string{"close", "crash", "partitions"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			at := strconv.FormatInt(time.Now().Truncate(historyTimestampResolution).UnixNano(), 10)
			readAction := "read"
			if mode == "partitions" {
				readAction = "partition-read"
			}
			if mode == "crash" {
				readAction = "crash-read"
			}
			for _, action := range []string{mode, readAction} {
				cmd := exec.Command(os.Args[0], "-test.run=^TestHistoryDiskHelper$")
				cmd.Env = append(os.Environ(), "VNRAY_TSTORAGE_TEST_DIR="+dir, "VNRAY_TSTORAGE_TEST_ACTION="+action, "VNRAY_TSTORAGE_TEST_AT="+at)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s: %v\n%s", action, err, output)
				}
			}
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() {
				t.Fatalf("no tstorage directory: %v", err)
			}
		})
	}
}

func TestHistoryDiskHelper(t *testing.T) {
	dir := os.Getenv("VNRAY_TSTORAGE_TEST_DIR")
	if dir == "" {
		return
	}
	at, err := strconv.ParseInt(os.Getenv("VNRAY_TSTORAGE_TEST_AT"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHistory(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sample := testSnapshot()
	sample.SampledAt = time.Unix(0, at).UTC()
	ctx := context.Background()
	action := os.Getenv("VNRAY_TSTORAGE_TEST_ACTION")
	offsets := make([]time.Duration, 10)
	for i := range offsets {
		offsets[i] = time.Duration(i) * 2 * time.Second
	}
	if action == "partitions" || action == "partition-read" {
		offsets = []time.Duration{0, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 30*time.Minute + historyTimestampResolution, 30*time.Minute + 2*historyTimestampResolution, 40 * time.Minute}
	}
	if action != "read" && action != "partition-read" && action != "crash-read" {
		for i, offset := range offsets {
			sample.SampledAt = time.Unix(0, at).Add(offset).UTC()
			sample.Process.GCCount = uint64(i)
			if err := h.Save(ctx, sample); err != nil {
				t.Fatal(err)
			}
		}
		if action == "crash" {
			os.Exit(0)
		}
	} else {
		wantRows := len(offsets)
		if action == "crash-read" {
			// WAL 已关闭，未刷盘的内存分区随进程退出丢失。
			wantRows = 0
		}
		rows, err := h.Read(ctx, sample.SampledAt, sample.SampledAt.Add(offsets[len(offsets)-1]), 120, []uint64{sample.Users[0].ID, sample.Users[1].ID})
		if err != nil || len(rows) != wantRows {
			t.Fatalf("reopen read: %d rows, %v", len(rows), err)
		}
		for i, row := range rows {
			want := sample
			want.Users = append([]User(nil), sample.Users...)
			for j := range want.Users {
				want.Users[j].Name, want.Users[j].Protocol = "", ""
			}
			want.SampledAt = sample.SampledAt.Add(offsets[i])
			want.Process.GCCount = uint64(i)
			if !reflect.DeepEqual(row, want) {
				t.Fatalf("disk round trip %d: got %+v, want %+v", i, row, want)
			}
		}
		if action == "partition-read" {
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			partitions := 0
			for _, entry := range entries {
				if !entry.IsDir() || entry.Name() == "wal" {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dir, entry.Name(), "meta.json"))
				if err != nil {
					t.Fatal(err)
				}
				var meta struct {
					Min   int64 `json:"minTimestamp"`
					Max   int64 `json:"maxTimestamp"`
					Count int   `json:"numDataPoints"`
				}
				if err := json.Unmarshal(data, &meta); err != nil {
					t.Fatal(err)
				}
				if meta.Count == 0 {
					continue
				}
				partitions++
				if meta.Max-meta.Min > historyPartitionDuration.Milliseconds()+historyTimestampResolution.Milliseconds() {
					t.Fatal("partition exceeds boundary")
				}
			}
			if partitions != 3 {
				t.Fatalf("partitions: %d, want 3", partitions)
			}
		}
		if err := h.Configure(ctx, 2*time.Hour); err != nil {
			t.Fatal(err)
		}
		rows, err = h.Read(ctx, sample.SampledAt.Add(offsets[len(offsets)-1]+time.Minute), sample.SampledAt.Add(2*time.Hour), 120, nil)
		if err != nil || len(rows) != 0 {
			t.Fatalf("query returned out-of-window data: %v %v", rows, err)
		}
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryRetentionAndUserMembership(t *testing.T) {
	h, db := newTestHistory(t)
	first := time.Now().UTC().Truncate(historyTimestampResolution).Add(-time.Hour)
	now := first.Add(time.Hour)
	h.now = func() time.Time { return now }
	sample := testSnapshot()
	ids := []uint64{sample.Users[0].ID, sample.Users[1].ID}
	for i := range 10 {
		sample.SampledAt = first.Add(time.Duration(i) * time.Minute)
		sample.Process.GCCount = uint64(i)
		if i == 5 {
			sample.Users = sample.Users[:1]
		}
		if err := h.Save(context.Background(), sample); err != nil {
			t.Fatal(err)
		}
	}
	h.retention = 55 * time.Minute
	rows, err := h.Read(context.Background(), first, now, 120, ids)
	if err != nil || len(rows) != 5 || !rows[0].SampledAt.Equal(first.Add(5*time.Minute)) {
		t.Fatalf("retention boundary: %v %v", rows, err)
	}
	for _, row := range rows {
		if len(row.Users) != 1 {
			t.Fatal("deleted user survived")
		}
	}
	if err := h.Configure(context.Background(), 55*time.Minute); err != nil {
		t.Fatal(err)
	}
	if db.writes != 10 {
		t.Fatal("unexpected writes during retention maintenance")
	}
}

func TestHistoryTimestampResolutionBoundsAndCollision(t *testing.T) {
	h, _ := newTestHistory(t)
	sample := testSnapshot()
	sample.SampledAt = time.Now().UTC().Truncate(historyTimestampResolution).Add(123456 * time.Nanosecond)
	at := sample.SampledAt.Truncate(historyTimestampResolution)
	ctx := context.Background()
	if err := h.Save(ctx, sample); err != nil {
		t.Fatal(err)
	}
	for _, window := range [][2]time.Time{{at.Add(-time.Nanosecond), at.Add(-time.Nanosecond)}, {at.Add(time.Nanosecond), at.Add(100 * time.Nanosecond)}} {
		rows, err := h.Read(ctx, window[0], window[1], 120, nil)
		if err != nil || len(rows) != 0 {
			t.Fatalf("nanosecond boundary: %v %v", rows, err)
		}
	}
	sample.SampledAt = at.Add(historyTimestampResolution - time.Nanosecond)
	sample.Process.GCCount++
	if err := h.Save(ctx, sample); err == nil {
		t.Fatal("accepted same-resolution collision")
	}
	sample.SampledAt = at.Add(historyTimestampResolution)
	if err := h.Save(ctx, sample); err != nil {
		t.Fatal(err)
	}
	sample.Process.GCCount++
	if err := h.Save(ctx, sample); err == nil {
		t.Fatal("accepted timestamp collision")
	}
	sample.Process.GCCount--
	rows, err := h.Read(ctx, at, sample.SampledAt, 120, nil)
	if err != nil || len(rows) != 2 || !rows[0].SampledAt.Equal(at) || !rows[1].SampledAt.Equal(sample.SampledAt) {
		t.Fatalf("100ms round trip: %v %v", rows, err)
	}
}
