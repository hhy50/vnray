package storagebench

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log/slog"
	"math"
	"strconv"
	"testing"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

func writePrometheus(t *testing.T, f fixture, dir string) func() {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	lsets := make([]labels.Labels, len(f.Series))
	lookup := make(map[string]int)
	for j, s := range f.Series {
		values := []string{"__name__", s.Metric}
		if s.UserID != 0 {
			values = append(values, "user_id", strconv.FormatUint(s.UserID, 10))
		}
		lsets[j] = labels.FromStrings(values...)
		lookup[lsets[j].String()] = j
	}
	var writer *tsdb.BlockWriter
	var appender storage.Appender
	var refs []storage.SeriesRef
	var first, last int64
	closeWriter := func() {
		if err := appender.Commit(); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	readSamples(t, f, func(i int, s sample) {
		if i == 0 {
			first = s.at / 1000000
		}
		last = s.at / 1000000
		if i%901 == 0 {
			if writer != nil {
				closeWriter()
			}
			var err error
			writer, err = tsdb.NewBlockWriter(logger, dir, 30*60*1000)
			if err != nil {
				t.Fatal(err)
			}
			refs = make([]storage.SeriesRef, len(f.Series))
			appender = writer.Appender(ctx)
		}
		if s.at%1000000 != 0 {
			t.Fatal("fixture timestamp is not exactly representable as milliseconds")
		}
		for j, v := range s.values {
			ref, err := appender.Append(refs[j], lsets[j], s.at/1000000, v)
			if err != nil {
				t.Fatal(err)
			}
			refs[j] = ref
		}
	})
	closeWriter()
	return func() {
		db, err := tsdb.OpenDBReadOnly(dir, "", logger)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		querier, err := db.Querier(first, last)
		if err != nil {
			t.Fatal(err)
		}
		defer querier.Close()
		matcher := labels.MustNewMatcher(labels.MatchRegexp, "__name__", ".+")
		set := querier.Select(ctx, false, nil, matcher)
		seriesCount := 0
		for set.Next() {
			series := set.At()
			j, ok := lookup[series.Labels().String()]
			if !ok {
				t.Fatal("unknown series", series.Labels())
			}
			digest := sha256.New()
			var pair [16]byte
			count := 0
			it := series.Iterator(nil)
			for it.Next() != chunkenc.ValNone {
				at, v := it.At()
				binary.LittleEndian.PutUint64(pair[:], uint64(at*1000000))
				binary.LittleEndian.PutUint64(pair[8:], math.Float64bits(v))
				digest.Write(pair[:])
				count++
			}
			if err := it.Err(); err != nil {
				t.Fatal(err)
			}
			checkHash(t, digest, hex.EncodeToString(f.seriesHashes[j].Sum(nil)), count, f.Samples)
			seriesCount++
		}
		if err := set.Err(); err != nil {
			t.Fatal(err)
		}
		if seriesCount != len(f.Series) {
			t.Fatalf("series count %d", seriesCount)
		}
	}
}
