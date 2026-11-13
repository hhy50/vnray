package monitor

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/nakabonne/tstorage"
)

func TestHistoryNativeSeriesAndInvalidMetrics(t *testing.T) {
	h, db := newTestHistory(t)
	ctx := context.Background()
	sample := testSnapshot()
	if err := h.Save(ctx, sample); err != nil {
		t.Fatal(err)
	}
	at := sample.SampledAt.UnixMilli()
	for _, tc := range []struct {
		metric string
		want   float64
	}{
		{"up_hi", 2097152}, {"up_lo", 1}, {"tcp_hi", 2147483648}, {"tcp_lo", 0},
	} {
		points, err := db.Select(userPrefix+tc.metric, userLabels(math.MaxUint64), at, at+1)
		if err != nil || len(points) != 1 || points[0].Value != tc.want {
			t.Fatalf("%s: %v %v", tc.metric, points, err)
		}
	}
	for _, change := range []func(*Snapshot){
		func(s *Snapshot) { s.Server.CPUPercent = testPointer(math.NaN()) },
		func(s *Snapshot) { s.Process.CPUPercent = testPointer(math.Inf(1)) },
		func(s *Snapshot) { s.Users = append(s.Users, s.Users[0]) },
	} {
		bad := testSnapshot()
		change(&bad)
		if err := h.Save(ctx, bad); err == nil {
			t.Fatal("accepted invalid snapshot")
		}
	}
	if db.writes != 1 {
		t.Fatal("invalid values reached engine")
	}
	for _, limit := range []int{0, 1, 121} {
		if _, err := h.Read(ctx, sample.SampledAt, sample.SampledAt, limit, nil); err == nil {
			t.Fatal("accepted invalid limit")
		}
	}
	if _, err := NewHistory("", time.Hour); err == nil {
		t.Fatal("accepted empty directory")
	}
	if _, err := NewHistory(t.TempDir(), 0); err == nil {
		t.Fatal("accepted zero retention")
	}
}

func TestHistoryMissingFieldsAndPartialBatch(t *testing.T) {
	for _, kind := range []string{"no marker", "missing field", "invalid integer"} {
		t.Run(kind, func(t *testing.T) {
			h, db := newTestHistory(t)
			sample := testSnapshot()
			rows, err := snapshotRows(sample)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "no marker":
				rows = rows[:len(rows)-1]
			case "missing field":
				rows = rows[1:]
			case "invalid integer":
				for i := range rows {
					if rows[i].Metric == hostPrefix+"memory_total_hi" {
						rows[i].Value = math.NaN()
					}
				}
			}
			if err := db.InsertRows(rows); err != nil {
				t.Fatal(err)
			}
			got, err := h.Read(context.Background(), sample.SampledAt, sample.SampledAt, 120, nil)
			if kind == "no marker" {
				if err != nil || len(got) != 0 {
					t.Fatalf("published partial batch: %v %v", got, err)
				}
			} else if err == nil {
				t.Fatal("accepted incomplete or corrupt sample")
			}
		})
	}
}

type canceledReader struct {
	tstorage.Storage
	cancel context.CancelFunc
}

func (s canceledReader) Select(metric string, labels []tstorage.Label, start, end int64) ([]*tstorage.DataPoint, error) {
	if metric == hostPrefix+"memory_total_hi" {
		s.cancel()
	}
	return s.Storage.Select(metric, labels, start, end)
}

func TestHistoryCancellationBetweenSeries(t *testing.T) {
	h, db := newTestHistory(t)
	sample := testSnapshot()
	if err := h.Save(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.store = canceledReader{Storage: db, cancel: cancel}
	if _, err := h.Read(ctx, sample.SampledAt, sample.SampledAt, 120, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestHistoryRetentionReopenRetry(t *testing.T) {
	h, db := newTestHistory(t)
	opens := 0
	wantErr := errors.New("open failed")
	h.open = func(dir string, retention time.Duration) (tstorage.Storage, error) {
		opens++
		if retention != 2*time.Hour {
			t.Fatal("retention not forwarded")
		}
		if opens == 1 {
			return nil, wantErr
		}
		return openHistory(dir, retention)
	}
	if err := h.Configure(context.Background(), time.Hour); err != nil || opens != 0 || h.store != db {
		t.Fatal("unchanged retention reopened engine")
	}
	if err := h.Configure(context.Background(), 2*time.Hour); !errors.Is(err, wantErr) {
		t.Fatal(err)
	}
	if err := h.Save(context.Background(), testSnapshot()); err == nil {
		t.Fatal("write accepted without engine")
	}
	if err := h.Configure(context.Background(), 2*time.Hour); err != nil || opens != 2 {
		t.Fatalf("retry: %v, opens %d", err, opens)
	}
	if err := h.Save(context.Background(), testSnapshot()); err != nil {
		t.Fatal(err)
	}
	if err := h.Configure(context.Background(), 2*time.Hour); err != nil || opens != 2 {
		t.Fatal("same retention reopened engine")
	}
}

func TestHistoryGroupsSelectedReads(t *testing.T) {
	h, db := newTestHistory(t)
	sample := testSnapshot()
	first := sample.SampledAt
	ctx := context.Background()
	for i := range 1000 {
		sample.SampledAt = first.Add(time.Duration(i) * 2 * time.Second)
		sample.Process.GCCount = uint64(i)
		if err := h.Save(ctx, sample); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := h.Read(ctx, first, sample.SampledAt, 120, []uint64{sample.Users[0].ID, sample.Users[1].ID})
	if err != nil || len(rows) != 120 {
		t.Fatalf("history: %d %v", len(rows), err)
	}
	for _, row := range rows {
		want := uint64(row.SampledAt.Sub(first) / (2 * time.Second))
		if row.Process.GCCount != want {
			t.Fatalf("counter: %d, want %d", row.Process.GCCount, want)
		}
	}
	if db.reads > len(rows) {
		t.Fatalf("queries did not share series reads: %d", db.reads)
	}
}
