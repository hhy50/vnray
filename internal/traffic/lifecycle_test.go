package traffic

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/store"
)

type blockingTrafficWriter struct {
	Writer
	entered chan struct{}
	release chan struct{}
}

func (s *blockingTrafficWriter) AddUserTraffic(ctx context.Context, p store.Protocol, key string, up, down int64) error {
	close(s.entered)
	select {
	case <-s.release:
		return s.Writer.AddUserTraffic(ctx, p, key, up, down)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestFlushDoesNotBlockCounterRegistration(t *testing.T) {
	st := testStore(t)
	m := NewMeter(nil)
	m.RestoreUser(store.ProtocolVNV1, "alice")
	m.UserStats(store.ProtocolVNV1, "alice").UpWrite.Add(10)
	writer := &blockingTrafficWriter{Writer: st, entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flushed := make(chan error, 1)
	go func() { flushed <- m.Flush(ctx, writer) }()
	select {
	case <-writer.entered:
	case <-ctx.Done():
		t.Fatal("flush did not reach storage")
	}
	registered := make(chan struct{})
	go func() {
		m.RestoreUser(store.ProtocolSS, "bob")
		m.UserStats(store.ProtocolSS, "bob")
		m.ClientStats("edge")
		m.PortMappingStats(1)
		close(registered)
	}()
	select {
	case <-registered:
	case <-ctx.Done():
		t.Error("database write blocked new tunnel counters")
	}
	close(writer.release)
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	assertUserTraffic(t, st, 10, 0)
}

func TestConcurrentFlushCountsEachDeltaOnce(t *testing.T) {
	st := testStore(t)
	m := NewMeter(nil)
	m.RestoreUser(store.ProtocolVNV1, "alice")
	m.UserStats(store.ProtocolVNV1, "alice").UpWrite.Add(100)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Flush(context.Background(), st); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	assertUserTraffic(t, st, 100, 0)
}

func TestServiceFlushesAfterDataPlaneStops(t *testing.T) {
	st := testStore(t)
	m := NewMeter(nil)
	m.RestoreUser(store.ProtocolVNV1, "alice")
	m.SetFlushInterval(time.Hour)
	svc := NewService(m, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	m.UserStats(store.ProtocolVNV1, "alice").UpWrite.Add(42)
	drainCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := svc.Drain(drainCtx); err != nil {
		t.Fatal(err)
	}
	assertUserTraffic(t, st, 42, 0)
	if err := svc.Drain(drainCtx); err != nil {
		t.Fatal(err)
	}
	assertUserTraffic(t, st, 42, 0)
	if err := svc.Start(context.Background()); err == nil {
		t.Fatal("drained service restarted")
	}
}

type deadlineTrafficWriter struct {
	Writer
	entered chan context.Context
}

func (w *deadlineTrafficWriter) AddUserTraffic(ctx context.Context, _ store.Protocol, _ string, _, _ int64) error {
	w.entered <- ctx
	<-ctx.Done()
	return ctx.Err()
}

type flushContextKey struct{}

func TestTimedFlushReleasesUserDeletion(t *testing.T) {
	m := NewMeter(nil)
	m.RestoreUser(store.ProtocolVNV1, "alice")
	m.UserStats(store.ProtocolVNV1, "alice").UpWrite.Add(1)
	writer := &deadlineTrafficWriter{entered: make(chan context.Context, 1)}
	parent := context.WithValue(context.Background(), flushContextKey{}, "periodic")
	flushed := make(chan error, 1)
	go func() { flushed <- m.flushWithin(parent, writer, 50*time.Millisecond) }()
	select {
	case ctx := <-writer.entered:
		if _, ok := ctx.Deadline(); !ok || ctx.Value(flushContextKey{}) != "periodic" {
			t.Fatal("flush lost its deadline or parent values")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("flush did not reach storage")
	}
	forgotten := make(chan struct{})
	go func() {
		m.ForgetUser(store.ProtocolVNV1, "alice")
		close(forgotten)
	}()
	select {
	case err := <-flushed:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timed flush error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow storage ignored the per-flush deadline")
	}
	select {
	case <-forgotten:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out flush kept user deletion blocked")
	}
	if parent.Err() != nil || m.UserStats(store.ProtocolVNV1, "alice") != nil {
		t.Fatal("flush canceled its parent or deletion did not finish")
	}
}
