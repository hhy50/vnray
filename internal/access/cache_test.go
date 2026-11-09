package access

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/store"
)

type delayedUserStore struct {
	store.Store
	mu      sync.Mutex
	row     store.UserBinding
	found   bool
	read    chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *delayedUserStore) GetUser(context.Context, store.Protocol, string) (store.UserBinding, bool, error) {
	s.mu.Lock()
	row, found := s.row, s.found
	s.mu.Unlock()
	s.once.Do(func() {
		if s.read != nil {
			close(s.read)
			<-s.release
		}
	})
	return row, found, nil
}

func (s *delayedUserStore) SetUser(_ context.Context, row store.UserBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.row, s.found = row, true
	return nil
}

func (s *delayedUserStore) DeleteUser(context.Context, store.Protocol, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := s.found
	s.found = false
	return found, nil
}

func (s *delayedUserStore) DeleteUserTraffic(context.Context, store.Protocol, string) (bool, error) {
	return false, nil
}

func TestUserCacheDoesNotRestoreInvalidatedAccess(t *testing.T) {
	for _, deletion := range []bool{false, true} {
		t.Run(fmt.Sprint(deletion), func(t *testing.T) {
			st := &delayedUserStore{
				row:   store.UserBinding{Key: "alice", Protocol: store.ProtocolVNV1, Name: "Alice", Client: "edge", Status: store.UserEnabled},
				found: true, read: make(chan struct{}), release: make(chan struct{}),
			}
			b := NewBindings(st)
			done := make(chan bool, 1)
			go func() {
				user, ok := b.UserFor(context.Background(), "alice")
				done <- ok && user.Active
			}()
			select {
			case <-st.read:
			case <-time.After(2 * time.Second):
				t.Fatal("lookup did not reach storage")
			}
			if deletion {
				if _, err := b.DeleteUser(context.Background(), store.ProtocolVNV1, "alice"); err != nil {
					t.Fatal(err)
				}
			} else if err := b.SetUser(context.Background(), Binding{UserKey: "alice", Protocol: store.ProtocolVNV1, Name: "Alice", Client: "edge", Status: store.UserDisabled}); err != nil {
				t.Fatal(err)
			}
			close(st.release)
			select {
			case active := <-done:
				if active {
					t.Fatal("in-flight read restored revoked access")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("lookup did not finish")
			}
			if user, ok := b.UserFor(context.Background(), "alice"); ok && user.Active {
				t.Fatal("stale access remained cached")
			}
		})
	}
}

func TestUserCacheBoundsAndExpiration(t *testing.T) {
	b := NewBindings(&delayedUserStore{})
	for i := range userCacheCapacity + 100 {
		b.LookupUser(context.Background(), store.ProtocolVNV1, fmt.Sprint(i))
	}
	if len(b.userCache) != userCacheCapacity || b.userLRU.Len() != userCacheCapacity {
		t.Fatalf("cache grew past capacity: %d/%d", len(b.userCache), b.userLRU.Len())
	}
	if _, ok := b.userCache[cacheKey(store.ProtocolVNV1, "0")]; ok {
		t.Fatal("oldest entry was not evicted")
	}
	key := fmt.Sprint(userCacheCapacity + 99)
	e := b.userCache[cacheKey(store.ProtocolVNV1, key)]
	value := e.Value.(userCacheEntry)
	value.expires = time.Now().Add(-time.Second)
	e.Value = value
	if _, ok, _ := b.cachedUser(store.ProtocolVNV1, key); ok {
		t.Fatal("expired entry was reused")
	}
	if len(b.userCache) != userCacheCapacity-1 {
		t.Fatal("expired entry was not removed")
	}
}
