package access

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/store"
)

type indexedKeyStore struct {
	store.Store
	mu            sync.Mutex
	rows          map[string]store.AuthKey
	lists         atomic.Int64
	listErr       error
	writeErr      error
	commitOnError bool
}

func (s *indexedKeyStore) ListAuthKeys(ctx context.Context) ([]store.AuthKey, error) {
	s.lists.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]store.AuthKey, 0, len(s.rows))
	for _, row := range s.rows {
		out = append(out, row)
	}
	return out, nil
}

func (s *indexedKeyStore) SetAuthKey(_ context.Context, key store.AuthKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr == nil || s.commitOnError {
		s.rows[key.Key] = key
	}
	return s.writeErr
}

func (s *indexedKeyStore) DeleteAuthKey(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, found := s.rows[key]
	if s.writeErr == nil || s.commitOnError {
		delete(s.rows, key)
	}
	return found, s.writeErr
}

func newIndexedKeyStore() *indexedKeyStore {
	return &indexedKeyStore{rows: make(map[string]store.AuthKey)}
}

func mustParsedKey(t testing.TB, value string) authkey.Key {
	t.Helper()
	key, err := authkey.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestAuthKeyIndexAvoidsStorageOnHitsAndMisses(t *testing.T) {
	s := newIndexedKeyStore()
	var value string
	for range 10000 {
		value = authkey.Generate()
		s.rows[value] = store.AuthKey{Key: value, Scope: "team:*"}
	}
	b := NewBindings(s)
	key, missing := mustParsedKey(t, value), mustParsedKey(t, authkey.Generate())
	if err := b.ReloadAuthKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.listErr = errors.New("database unavailable after initialization")
	for range 100 {
		got, configured, err := b.LookupTransportKey(context.Background(), key.ID())
		if err != nil || !configured || got != key {
			t.Fatalf("key lookup: %v", err)
		}
		if configured, ok := b.MatchClientAuthKey(context.Background(), "team:edge", value); !configured || !ok {
			t.Fatal("scope lookup failed")
		}
		if _, configured, err := b.LookupTransportKey(context.Background(), missing.ID()); !configured || !errors.Is(err, errTransportKeyNotRegistered) {
			t.Fatal("missing key accepted")
		}
		if configured, ok := b.MatchClientAuthKey(context.Background(), "other:edge", value); !configured || ok {
			t.Fatal("wrong scope accepted")
		}
	}
	if got := s.lists.Load(); got != 1 {
		t.Fatalf("performed %d full-table reads, want 1", got)
	}
}

func TestAuthKeyIndexWriteThrough(t *testing.T) {
	s := newIndexedKeyStore()
	b := NewBindings(s)
	value := authkey.Generate()
	key := mustParsedKey(t, value)
	if err := b.SetAuthKey(context.Background(), value, "a:*"); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.MatchClientAuthKey(context.Background(), "a:edge", value); !ok {
		t.Fatal("insert not visible")
	}
	if err := b.SetAuthKey(context.Background(), value, "b:*"); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.MatchClientAuthKey(context.Background(), "a:edge", value); ok {
		t.Fatal("old scope retained")
	}
	if _, ok := b.MatchClientAuthKey(context.Background(), "b:edge", value); !ok {
		t.Fatal("new scope not visible")
	}
	if deleted, err := b.DeleteAuthKey(context.Background(), value); err != nil || !deleted {
		t.Fatalf("delete: %v", err)
	}
	if _, configured, err := b.LookupTransportKey(context.Background(), key.ID()); err != nil || configured {
		t.Fatal("last deletion did not restore empty-table state")
	}
	if configured, ok := b.MatchClientAuthKey(context.Background(), "b:edge", value); configured || ok {
		t.Fatal("deleted key retained")
	}
	if deleted, err := b.DeleteAuthKey(context.Background(), value); err != nil || deleted {
		t.Fatalf("repeated delete: %v", err)
	}
	if s.lists.Load() != 1 {
		t.Fatal("write-through rescanned storage")
	}
}

func TestAuthKeyIndexLegacyRowsDisableFallback(t *testing.T) {
	s := newIndexedKeyStore()
	s.rows["old-password"] = store.AuthKey{Key: "old-password", Scope: "*"}
	b := NewBindings(s)
	missing := mustParsedKey(t, authkey.Generate())
	if _, configured, err := b.LookupTransportKey(context.Background(), missing.ID()); !configured || err == nil {
		t.Fatal("legacy row permitted fallback")
	}
	if configured, ok := b.MatchClientAuthKey(context.Background(), "team:edge", "old-password"); !configured || ok {
		t.Fatal("legacy key accepted")
	}
	if _, err := b.DeleteAuthKey(context.Background(), "old-password"); err != nil {
		t.Fatal(err)
	}
	if _, configured, err := b.LookupTransportKey(context.Background(), missing.ID()); configured || err != nil {
		t.Fatal("legacy deletion did not restore fallback")
	}
}

func TestAuthKeyIndexUncertainWritesFailClosed(t *testing.T) {
	for _, operation := range []string{"set", "delete"} {
		t.Run(operation, func(t *testing.T) {
			s := newIndexedKeyStore()
			b := NewBindings(s)
			value := authkey.Generate()
			key := mustParsedKey(t, value)
			if err := b.SetAuthKey(context.Background(), value, "a:*"); err != nil {
				t.Fatal(err)
			}
			s.writeErr = errors.New("commit response lost")
			s.commitOnError = true
			var err error
			if operation == "set" {
				err = b.SetAuthKey(context.Background(), value, "b:*")
			} else {
				_, err = b.DeleteAuthKey(context.Background(), value)
			}
			if err == nil {
				t.Fatal("write error lost")
			}
			s.listErr = errors.New("database unavailable")
			if _, configured, err := b.LookupTransportKey(context.Background(), key.ID()); !configured || err == nil {
				t.Fatal("uncertain write retained cached access")
			}
			if configured, ok := b.MatchClientAuthKey(context.Background(), "a:edge", value); !configured || ok {
				t.Fatal("reload failure allowed authentication")
			}
			s.listErr, s.writeErr = nil, nil
			if _, ok := b.MatchClientAuthKey(context.Background(), "a:edge", value); ok {
				t.Fatal("old permission restored after recovery")
			}
			if _, ok := b.MatchClientAuthKey(context.Background(), "b:edge", value); ok != (operation == "set") {
				t.Fatal("committed change not recovered")
			}
		})
	}
}

func TestAuthKeyIndexReloadAndRecovery(t *testing.T) {
	s := newIndexedKeyStore()
	b := NewBindings(s)
	value := authkey.Generate()
	if err := b.SetAuthKey(context.Background(), value, "a:*"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAuthKey(context.Background(), store.AuthKey{Key: value, Scope: "b:*"}); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadAuthKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.MatchClientAuthKey(context.Background(), "a:edge", value); ok {
		t.Fatal("reload retained old scope")
	}
	s.listErr = errors.New("database unavailable")
	if err := b.ReloadAuthKeys(context.Background()); err == nil {
		t.Fatal("reload failure hidden")
	}
	if configured, ok := b.MatchClientAuthKey(context.Background(), "b:edge", value); !configured || ok {
		t.Fatal("failed reload left stale access enabled")
	}
	s.listErr = nil
	if _, ok := b.MatchClientAuthKey(context.Background(), "b:edge", value); !ok {
		t.Fatal("index did not recover")
	}
}

func TestAuthKeyIndexConcurrentReadWrite(t *testing.T) {
	s := newIndexedKeyStore()
	b := NewBindings(s)
	value := authkey.Generate()
	key := mustParsedKey(t, value)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				_, _, _ = b.LookupTransportKey(context.Background(), key.ID())
				b.MatchClientAuthKey(context.Background(), "team:edge", value)
			}
		}()
	}
	for range 100 {
		if err := b.SetAuthKey(context.Background(), value, "team:*"); err != nil {
			t.Fatal(err)
		}
		if _, ok := b.MatchClientAuthKey(context.Background(), "team:edge", value); !ok {
			t.Fatal("completed insert not visible")
		}
		if _, err := b.DeleteAuthKey(context.Background(), value); err != nil {
			t.Fatal(err)
		}
		if _, ok := b.MatchClientAuthKey(context.Background(), "team:edge", value); ok {
			t.Fatal("completed delete not visible")
		}
	}
	wg.Wait()
	if s.lists.Load() != 1 {
		t.Fatal("concurrent cold readers repeated the full load")
	}
}

func BenchmarkAuthKeyIndex(b *testing.B) {
	for _, size := range []int{1, 1000, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			s := newIndexedKeyStore()
			var value string
			for range size {
				value = authkey.Generate()
				s.rows[value] = store.AuthKey{Key: value, Scope: "team:*"}
			}
			bindings := NewBindings(s)
			if err := bindings.ReloadAuthKeys(context.Background()); err != nil {
				b.Fatal(err)
			}
			id := mustParsedKey(b, value).ID()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_, _, _ = bindings.LookupTransportKey(context.Background(), id)
				bindings.MatchClientAuthKey(context.Background(), "team:edge", value)
			}
			b.StopTimer()
			if s.lists.Load() != 1 {
				b.Fatal("lookup queried storage")
			}
		})
	}
}
