package access

import (
	"context"
	"errors"
	"testing"

	"github.com/hhy50/vnray/internal/store"
)

type contextProbeStore struct {
	store.Store
	seen error
}

func (s *contextProbeStore) GetUser(ctx context.Context, _ store.Protocol, _ string) (store.UserBinding, bool, error) {
	s.seen = ctx.Err()
	return store.UserBinding{}, false, ctx.Err()
}

func TestLookupUserPassesCallerCancellationToStore(t *testing.T) {
	st := &contextProbeStore{}
	b := NewBindings(st)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := b.LookupUser(ctx, store.ProtocolVNV1, "alice"); ok {
		t.Fatal("cancelled lookup returned a user")
	}
	if st.seen != context.Canceled {
		t.Fatalf("store received context error %v, want canceled", st.seen)
	}
}

type listUsersStore struct {
	store.Store
	rows []store.UserBinding
	err  error
	seen error
}

func (s *listUsersStore) ListUsers(ctx context.Context) ([]store.UserBinding, error) {
	s.seen = ctx.Err()
	if s.seen != nil {
		return nil, s.seen
	}
	return s.rows, s.err
}

func TestListUsersPropagatesReadFailureAndCancellation(t *testing.T) {
	wantErr := errors.New("user read failed")
	st := &listUsersStore{err: wantErr}
	b := NewBindings(st)
	if _, err := b.ListUsers(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("read error = %v, want %v", err, wantErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.ListUsers(ctx); !errors.Is(err, context.Canceled) || st.seen != context.Canceled {
		t.Fatalf("canceled read = %v, store context = %v", err, st.seen)
	}
}

type deleteOrderStore struct {
	store.Store
	notified   bool
	removed    bool
	cancel     context.CancelFunc
	cleanupErr error
}

func (s *deleteOrderStore) DeleteUser(context.Context, store.Protocol, string) (bool, error) {
	if s.cancel != nil {
		s.cancel()
	}
	return true, nil
}

func (s *deleteOrderStore) DeleteUserTraffic(ctx context.Context, _ store.Protocol, _ string) (bool, error) {
	s.removed = s.notified
	s.cleanupErr = ctx.Err()
	return true, nil
}

func TestDeleteUserStopsTrafficBeforeRemovingHistory(t *testing.T) {
	st := &deleteOrderStore{}
	b := NewBindings(st)
	b.Watch(func(change UserChange) {
		if change.Deleted {
			st.notified = true
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st.cancel = cancel
	deleted, err := b.DeleteUser(ctx, store.ProtocolVNV1, "alice")
	if err != nil || !deleted || !st.removed || st.cleanupErr != nil {
		t.Fatalf("delete=%v err=%v traffic after notify=%v cleanup err=%v", deleted, err, st.removed, st.cleanupErr)
	}
}
