package monitor

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/metrics"
)

func TestHistoryTimestampQuantization(t *testing.T) {
	for _, offset := range []time.Duration{0, time.Nanosecond, 93*time.Millisecond + 123456*time.Nanosecond, 100*time.Millisecond - time.Nanosecond, 100 * time.Millisecond} {
		t.Run(offset.String(), func(t *testing.T) {
			h, db := newTestHistory(t)
			sample := testSnapshot()
			sample.SampledAt = sample.SampledAt.Add(offset)
			original := sample.SampledAt
			at := original.Truncate(100 * time.Millisecond)
			ctx := context.Background()
			if err := h.Save(ctx, sample); err != nil {
				t.Fatal(err)
			}
			rows, err := h.Read(ctx, at, original, 120, []uint64{sample.Users[0].ID, sample.Users[1].ID})
			if err != nil || len(rows) != 1 {
				t.Fatalf("quantized read: %d rows: %v", len(rows), err)
			}
			want := sample
			want.SampledAt = at
			want.Users = append([]User(nil), want.Users...)
			for i := range want.Users {
				want.Users[i].Name, want.Users[i].Protocol = "", ""
			}
			if !reflect.DeepEqual(rows[0], want) || !sample.SampledAt.Equal(original) {
				t.Fatalf("quantization changed values: %+v", rows[0])
			}
			points, err := db.Select(sampleMetric, nil, at.UnixMilli(), at.UnixMilli()+1)
			if err != nil || len(points) != 1 || points[0].Timestamp%100 != 0 {
				t.Fatalf("native storage is not 100ms aligned: %v %v", points, err)
			}
		})
	}
}

func TestHistoryNativeMillisecondRange(t *testing.T) {
	h, db := newTestHistory(t)
	sample := testSnapshot()
	first := sample.SampledAt
	ctx := context.Background()
	for i := range 2 {
		sample.SampledAt = first.Add(time.Duration(i) * historyTimestampResolution)
		sample.Process.GCCount++
		if err := h.Save(ctx, sample); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		points, err := h.selectPoints(ctx, sampleMetric, nil, first.UnixMilli(), sample.SampledAt.UnixMilli())
		if err != nil || len(points) != 1 || points[0].Timestamp != first.UnixMilli() {
			t.Fatalf("exclusive end: %v %v", points, err)
		}
		native, err := db.Select(sampleMetric, nil, first.UnixMilli(), sample.SampledAt.UnixMilli()+1)
		if err != nil || len(native) != 2 || native[0].Timestamp != first.UnixMilli() {
			t.Fatalf("native timestamps mutated: %v %v", native, err)
		}
	}
	h.now = func() time.Time { return first.Add(time.Hour + time.Nanosecond) }
	rows, err := h.Read(ctx, first, sample.SampledAt, 120, nil)
	if err != nil || len(rows) != 1 || !rows[0].SampledAt.Equal(sample.SampledAt) {
		t.Fatalf("sub-millisecond retention: %v %v", rows, err)
	}
}

func TestUsersHistoryDoesNotDuplicateLatestBucket(t *testing.T) {
	for _, name := range []string{"quantized", "original"} {
		t.Run(name, func(t *testing.T) {
			history := &memoryHistory{}
			s := New(Options{History: history})
			s.Configure(testSettings())
			ctx := context.Background()
			s.collect(ctx, metrics.NewSampler(), testSettings())
			p := s.current.Load()
			p.users.SampledAt = p.users.SampledAt.Truncate(historyTimestampResolution).Add(93*time.Millisecond + 123456*time.Nanosecond)
			history.rows[0].SampledAt = p.users.SampledAt
			if name == "quantized" {
				history.rows[0].SampledAt = p.users.SampledAt.Truncate(historyTimestampResolution)
			}
			data, err := s.UsersJSON(ctx, Query{Window: time.Hour, Points: 120})
			if err != nil {
				t.Fatal(err)
			}
			var response Response
			if err := json.Unmarshal(data, &response); err != nil {
				t.Fatal(err)
			}
			if len(response.History) != 1 || !response.History[0].SampledAt.Equal(p.users.SampledAt) {
				t.Fatalf("latest sample duplicated or truncated: %+v", response.History)
			}
		})
	}
}
