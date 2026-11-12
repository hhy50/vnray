package monitor

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hhy50/vnray/internal/metrics"
)

type blockedHistory struct {
	entered chan struct{}
	release chan struct{}
}

func (*blockedHistory) Save(context.Context, time.Time, []byte, time.Time) error { return nil }
func (h *blockedHistory) Read(ctx context.Context, _, _ time.Time, _ int) ([][]byte, error) {
	close(h.entered)
	select {
	case <-h.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestHistoryWaitCancellation(t *testing.T) {
	history := &blockedHistory{entered: make(chan struct{}), release: make(chan struct{})}
	s := New(Options{History: history})
	s.Configure(testSettings())
	s.collect(context.Background(), metrics.NewSampler(), testSettings())
	query := Query{Window: time.Hour, Points: 120}
	done := make(chan error, 1)
	go func() { _, err := s.UsersJSON(context.Background(), query); done <- err }()
	<-history.entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.UsersJSON(ctx, query); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting query: %v", err)
	}
	close(history.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHistoryCacheExpiryAndDisabledLongIntervalCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		history := &memoryHistory{}
		s := New(Options{History: history})
		settings := testSettings()
		settings.Interval = time.Hour
		s.Configure(settings)
		s.collect(context.Background(), metrics.NewSampler(), settings)
		query := Query{Window: time.Second, Points: 120}
		if _, err := s.UsersJSON(context.Background(), query); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		if _, err := s.UsersJSON(context.Background(), query); err != nil {
			t.Fatal(err)
		}
		if history.reads != 2 {
			t.Fatal("history cache outlived its oldest sample")
		}
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer s.Drain(context.Background())
		synctest.Wait()
		prunes := history.prunes.Load()
		time.Sleep(time.Minute)
		synctest.Wait()
		if history.prunes.Load() <= prunes {
			t.Fatal("long sample interval delayed retention cleanup")
		}
		settings.Enabled = false
		s.Configure(settings)
		settings.Enabled = true
		s.Configure(settings)
		synctest.Wait()
		if _, err := s.HostJSON(); err != nil {
			t.Fatalf("rapid toggle left stale sample: %v", err)
		}
	})
}
