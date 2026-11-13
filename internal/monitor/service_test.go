package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hhy50/vnray/internal/metrics"
	"github.com/hhy50/vnray/internal/stats"
)

type memoryHistory struct {
	rows          []Snapshot
	reads, writes int
	maintenance   atomic.Int64
	closes        atomic.Int64
	err           error
}

func (h *memoryHistory) Save(_ context.Context, data Snapshot) error {
	if h.err != nil {
		return h.err
	}
	h.writes++
	data.Users = append([]User(nil), data.Users...)
	h.rows = append(h.rows, data)
	return nil
}

func (h *memoryHistory) Read(context.Context, time.Time, time.Time, int, []uint64) ([]Snapshot, error) {
	h.reads++
	rows := append([]Snapshot(nil), h.rows...)
	for i := range rows {
		rows[i].Users = append([]User(nil), rows[i].Users...)
	}
	return rows, h.err
}

func (h *memoryHistory) Configure(context.Context, time.Duration) error {
	h.maintenance.Add(1)
	return h.err
}

func (h *memoryHistory) Close() error { h.closes.Add(1); return nil }

func testSettings() Settings {
	return Settings{Enabled: true, Interval: 2 * time.Second, Retention: time.Hour}
}

func TestSamplesDirectoryCacheRatesAndHistory(t *testing.T) {
	history := &memoryHistory{}
	counters := &stats.PumpStats{}
	users := []UserSource{{ID: 1, Name: "alice", Protocol: "vmess", Stats: counters}, {ID: 2, Name: "alice", Protocol: "ss"}}
	loads := 0
	s := New(Options{History: history, Users: func(context.Context) ([]UserSource, error) { loads++; return users, nil }})
	s.Configure(testSettings())
	ctx := context.Background()
	sampler := metrics.NewSampler()
	s.collect(ctx, sampler, testSettings())
	first := s.current.Load()
	if len(first.users.Users) != 2 || first.users.Users[0].UpRate != nil {
		t.Fatalf("first sample: %+v", first.users)
	}
	counters.OpenTCP()
	counters.OpenStream()
	counters.Request()
	counters.UpWrite.Add(100)
	counters.DownWrite.Add(200)
	usersAt := s.sampleUsers(first.users.SampledAt.Add(2 * time.Second))
	active := usersAt[0]
	if active.TCP != 1 || active.Streams != 1 || active.Requests != 1 || *active.UpRate != 50 || *active.DownRate != 100 {
		t.Fatalf("active: %+v", active)
	}
	counters.CloseTCP()
	counters.CloseStream()
	s.collect(ctx, sampler, testSettings())
	if loads != 1 {
		t.Fatal("unchanged directory reloaded")
	}
	q := Query{Window: time.Hour, Points: 120}
	for range 3 {
		if _, err := s.UsersJSON(ctx, Query{}); err != nil {
			t.Fatal(err)
		}
	}
	if history.reads != 0 {
		t.Fatal("latest list read database")
	}
	for range 3 {
		if _, err := s.UsersJSON(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if history.reads != 1 {
		t.Fatal("history was not cached")
	}
	users = users[1:]
	s.InvalidateUsers()
	s.collect(ctx, sampler, testSettings())
	encoded, err := s.UsersJSON(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	for _, sample := range response.History {
		for _, user := range sample.Users {
			if user.ID == 1 {
				t.Fatal("deleted user history visible")
			}
		}
	}
	if loads != 2 || history.reads != 2 {
		t.Fatal("directory/history not invalidated")
	}
	settings := testSettings()
	settings.Enabled = false
	s.Configure(settings)
	if _, err := s.HostJSON(); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
	s.Configure(testSettings())
	if _, err := s.HostJSON(); !errors.Is(err, ErrWarming) {
		t.Fatal(err)
	}
	s.collect(ctx, sampler, testSettings())
	if s.current.Load().users.Users[0].UpRate != nil {
		t.Fatal("stale rate baseline reused")
	}
	stale := *s.current.Load()
	stale.host.SampledAt = time.Now().Add(-time.Hour)
	s.current.Store(&stale)
	if _, err := s.HostJSON(); !errors.Is(err, ErrWarming) {
		t.Fatal("expired sample returned")
	}
}

func TestLifecycleReloadAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		history := &memoryHistory{}
		s := New(Options{History: history})
		s.Configure(testSettings())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := s.Start(ctx); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		first := s.current.Load().host.SampledAt
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := s.current.Load().host.SampledAt.Sub(first); got != 2*time.Second {
			t.Fatalf("cadence: %v", got)
		}
		settings := testSettings()
		settings.Interval = 5 * time.Second
		s.Configure(settings)
		synctest.Wait()
		changed := s.current.Load().host.SampledAt
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if !s.current.Load().host.SampledAt.Equal(changed) {
			t.Fatal("old interval active")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if s.current.Load().host.SampledAt.Sub(changed) != 5*time.Second {
			t.Fatal("new interval not applied")
		}
		settings.Enabled = false
		s.Configure(settings)
		synctest.Wait()
		if s.current.Load() != nil {
			t.Fatal("disabled sampler retained snapshot")
		}
		maintained := history.maintenance.Load()
		time.Sleep(time.Minute)
		synctest.Wait()
		if history.maintenance.Load() <= maintained {
			t.Fatal("disabled history did not receive configuration maintenance")
		}
		settings.Enabled = true
		s.Configure(settings)
		synctest.Wait()
		if s.current.Load() == nil {
			t.Fatal("did not resume")
		}
		cancel()
		if err := s.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := s.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(context.Background()); err == nil {
			t.Fatal("restarted stopped service")
		}
		if history.closes.Load() != 1 {
			t.Fatal("history was not closed exactly once")
		}
	})
}

func TestStorageFailureAndCancellation(t *testing.T) {
	history := &memoryHistory{err: errors.New("write failure")}
	s := New(Options{History: history})
	s.Configure(testSettings())
	s.collect(context.Background(), metrics.NewSampler(), testSettings())
	if p := s.current.Load(); p == nil || p.users.HistoryStored {
		t.Fatal("latest lost or write error hidden")
	}
	if _, err := s.UsersJSON(context.Background(), Query{Window: time.Hour, Points: 120}); err == nil {
		t.Fatal("history failure hidden")
	}
	entered := make(chan struct{})
	s = New(Options{Users: func(ctx context.Context) ([]UserSource, error) { close(entered); <-ctx.Done(); return nil, ctx.Err() }})
	s.Configure(testSettings())
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Drain(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDrainClosesUnstartedHistory(t *testing.T) {
	history := &memoryHistory{}
	s := New(Options{History: history})
	for range 2 {
		if err := s.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if history.closes.Load() != 1 {
		t.Fatal("unstarted history was not closed exactly once")
	}
}

func TestConcurrentReadsAndConfiguration(t *testing.T) {
	s := New(Options{})
	s.Configure(testSettings())
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Drain(context.Background())
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_, _ = s.HostJSON()
				_, _ = s.UsersJSON(context.Background(), Query{})
				s.InvalidateUsers()
				s.Configure(testSettings())
			}
		}()
	}
	wg.Wait()
}

func BenchmarkLatestUsers(b *testing.B) {
	s := New(Options{})
	s.Configure(testSettings())
	s.collect(context.Background(), metrics.NewSampler(), testSettings())
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = s.UsersJSON(context.Background(), Query{})
	}
}
