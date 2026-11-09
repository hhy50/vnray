package portmap

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/store"
)

type controlMemoryStore struct {
	mu          sync.Mutex
	rows        map[uint64]store.PortMapping
	afterCommit func()
}

func (s *controlMemoryStore) ListPortMappings(ctx context.Context) ([]store.PortMapping, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []store.PortMapping
	for _, row := range s.rows {
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *controlMemoryStore) GetPortMapping(ctx context.Context, id uint64) (store.PortMapping, bool, error) {
	if err := ctx.Err(); err != nil {
		return store.PortMapping{}, false, err
	}
	s.mu.Lock()
	row, ok := s.rows[id]
	s.mu.Unlock()
	runtime.Gosched()
	return row, ok, nil
}

func (s *controlMemoryStore) SetPortMapping(ctx context.Context, row store.PortMapping) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.rows[row.ID] = row
	s.mu.Unlock()
	if s.afterCommit != nil {
		s.afterCommit()
	}
	return nil
}

func (s *controlMemoryStore) DeletePortMapping(ctx context.Context, id uint64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	_, ok := s.rows[id]
	delete(s.rows, id)
	s.mu.Unlock()
	if s.afterCommit != nil {
		s.afterCommit()
	}
	return ok, nil
}

type controlSyncFunc func(context.Context)

func (f controlSyncFunc) Sync(ctx context.Context) { f(ctx) }

func newControlMemoryStore() *controlMemoryStore {
	return &controlMemoryStore{rows: map[uint64]store.PortMapping{
		1: {ID: 1, Remark: "keep", Config: store.PortConfig{CertPEM: "certificate"}},
	}}
}

func TestControlConcurrentUpdatesKeepCommittedOrder(t *testing.T) {
	st := newControlMemoryStore()
	var published []int64
	control := NewControl(st, controlSyncFunc(func(ctx context.Context) {
		rows, err := st.ListPortMappings(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		published = append(published, rows[0].Config.UpLimit)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const changes = 48
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range changes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, ok, err := control.Update(ctx, 1, func(row *store.PortMapping) error {
				row.Config.UpLimit++
				return nil
			})
			if err != nil || !ok {
				t.Errorf("update: found=%v err=%v", ok, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	row, _, err := st.GetPortMapping(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if row.Config.UpLimit != changes || row.Remark != "keep" || row.Config.CertPEM != "certificate" {
		t.Fatalf("concurrent updates lost fields: %+v", row)
	}
	if len(published) != changes {
		t.Fatalf("published %d changes, want %d", len(published), changes)
	}
	for i, value := range published {
		if value != int64(i+1) {
			t.Fatalf("publication %d = %d, want %d", i, value, i+1)
		}
	}
}

func TestControlPublishesCommitAfterRequestCancellation(t *testing.T) {
	for _, operation := range []string{"save", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			type contextKey struct{}
			parent := context.WithValue(context.Background(), contextKey{}, "request")
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			st := newControlMemoryStore()
			st.afterCommit = cancel
			published := false
			control := NewControl(st, controlSyncFunc(func(publishCtx context.Context) {
				published = true
				if publishCtx.Err() != nil {
					t.Errorf("committed publication canceled: %v", publishCtx.Err())
				}
				if publishCtx.Value(contextKey{}) != "request" {
					t.Error("publication lost context values")
				}
				deadline, ok := publishCtx.Deadline()
				if !ok || time.Until(deadline) > syncTimeout {
					t.Error("publication has no bounded deadline")
				}
			}))
			var err error
			switch operation {
			case "save":
				err = control.Save(ctx, store.PortMapping{ID: 1})
			case "update":
				_, _, err = control.Update(ctx, 1, func(row *store.PortMapping) error {
					row.Enabled = true
					return nil
				})
			case "delete":
				_, err = control.Delete(ctx, 1)
			}
			if err != nil {
				t.Fatal(err)
			}
			if ctx.Err() == nil || !published {
				t.Fatalf("request canceled=%v published=%v", ctx.Err() != nil, published)
			}
		})
	}
}

func TestControlCanceledWaitDoesNotReachStore(t *testing.T) {
	st := newControlMemoryStore()
	control := NewControl(st, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		_, _, err := control.Update(ctx, 1, func(row *store.PortMapping) error {
			close(entered)
			<-release
			return nil
		})
		firstDone <- err
	}()
	<-entered
	waitCtx, waitCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- control.Save(waitCtx, store.PortMapping{ID: 2})
	}()
	waitCancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("queued save error = %v", err)
		}
	case <-ctx.Done():
		t.Error("canceled write stayed blocked behind active update")
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.GetPortMapping(context.Background(), 2); err != nil || ok {
		t.Fatalf("canceled write reached store: found=%v err=%v", ok, err)
	}
}

func TestControlFullUpdatePreservesStoredTraffic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := store.Open(ctx, "sqlite", filepath.Join(t.TempDir(), "mappings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	control := NewControl(st, nil)
	mapping := store.PortMapping{
		Client: "edge", RemotePort: 12000, LocalHost: "127.0.0.1", LocalPort: 80,
	}
	if err := control.Save(ctx, mapping); err != nil {
		t.Fatal(err)
	}
	rows, err := control.List(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("mapping list: rows=%v err=%v", rows, err)
	}
	id := rows[0].ID
	if err := st.AddPortMappingTraffic(ctx, id, 13, 17); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := control.Update(ctx, id, func(row *store.PortMapping) error {
		*row = mapping
		row.Remark = "edited"
		return nil
	}); err != nil || !ok {
		t.Fatalf("update: found=%v err=%v", ok, err)
	}
	current, ok, err := st.GetPortMapping(ctx, id)
	if err != nil || !ok {
		t.Fatalf("get mapping: found=%v err=%v", ok, err)
	}
	if current.Up != 13 || current.Down != 17 || current.Remark != "edited" {
		t.Fatalf("configuration update replaced traffic: %+v", current)
	}
}
