package traffic

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/store"
)

func TestDeletedUsersReleaseRegistration(t *testing.T) {
	m := NewMeter(nil)
	for i := range 10_000 {
		key := fmt.Sprint(i)
		m.RestoreUser(store.ProtocolVNV1, key)
		m.UserStats(store.ProtocolVNV1, key).UpWrite.Add(1)
		m.ForgetUser(store.ProtocolVNV1, key)
		if m.UserStats(store.ProtocolVNV1, key) != nil {
			t.Fatal("deleted user was registered by an old connection")
		}
	}
	if len(m.users) != 0 {
		t.Fatalf("deleted users retained %d registrations", len(m.users))
	}
}

func TestRecreatedUserDoesNotReuseOldCounter(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	m := NewMeter(nil)
	m.RestoreUser(store.ProtocolVNV1, "alice")
	old := m.UserStats(store.ProtocolVNV1, "alice")
	old.UpWrite.Add(10)
	m.ForgetUser(store.ProtocolVNV1, "alice")
	old.UpWrite.Add(20)
	if err := m.Flush(ctx, st); err != nil {
		t.Fatal(err)
	}
	if rows, err := st.ListUserTraffic(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("deleted user traffic = %v, err=%v", rows, err)
	}
	m.RestoreUser(store.ProtocolVNV1, "alice")
	current := m.UserStats(store.ProtocolVNV1, "alice")
	if current == old {
		t.Fatal("recreated user reused an old connection counter")
	}
	m.RestoreUser(store.ProtocolVNV1, "alice")
	if m.UserStats(store.ProtocolVNV1, "alice") != current {
		t.Fatal("updating an existing user replaced its counter")
	}
	current.UpWrite.Add(3)
	old.UpWrite.Add(40)
	if err := m.Flush(ctx, st); err != nil {
		t.Fatal(err)
	}
	assertUserTraffic(t, st, 3, 0)
}

type blockingBatchStore struct {
	store.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingBatchStore) AddTrafficBatch(ctx context.Context, batch store.TrafficBatch) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		return s.Store.AddTrafficBatch(ctx, batch)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestDeleteWaitsForPendingFlushAndRemovesUserTraffic(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			st := testStore(t)
			m := NewMeter(nil)
			m.RestoreUser(store.ProtocolVNV1, "idle")
			m.RestoreUser(store.ProtocolVNV1, "alice")
			old := m.UserStats(store.ProtocolVNV1, "alice")
			client := m.ClientStats("edge")
			old.UpWrite.Add(10)
			client.UpWrite.Add(10)
			entered, release := make(chan struct{}), make(chan struct{})
			var released sync.Once
			unblock := func() { released.Do(func() { close(release) }) }
			defer unblock()
			var writer Writer = &blockingTrafficWriter{Writer: st, entered: entered, release: release}
			if batch {
				writer = &blockingBatchStore{Store: st, entered: entered, release: release}
			}
			flushed := make(chan error, 1)
			go func() { flushed <- m.Flush(ctx, writer) }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("flush did not reach storage")
			}
			deleted := make(chan error, 1)
			go func() {
				m.ForgetUser(store.ProtocolVNV1, "alice")
				_, err := st.DeleteUserTraffic(ctx, store.ProtocolVNV1, "alice")
				deleted <- err
			}()
			select {
			case err := <-deleted:
				t.Fatalf("deletion passed an in-flight flush: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			unblock()
			if err := <-flushed; err != nil {
				t.Fatal(err)
			}
			if err := <-deleted; err != nil {
				t.Fatal(err)
			}
			old.UpWrite.Add(20)
			client.UpWrite.Add(20)
			if m.UserStats(store.ProtocolVNV1, "alice") != nil {
				t.Fatal("old connection recreated deleted user")
			}
			if err := m.Flush(ctx, st); err != nil {
				t.Fatal(err)
			}
			if rows, err := st.ListUserTraffic(ctx); err != nil || len(rows) != 0 {
				t.Fatalf("deleted user traffic = %v, err=%v", rows, err)
			}
			assertClientTraffic(t, st, 30, 0)
		})
	}
}
