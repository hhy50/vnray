package access

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/store"
)

// slowUserStore 在每次读库上留出窗口，让"读—改—写"之间的竞态在无锁实现下必然暴露。
type slowUserStore struct {
	store.Store
	mu    sync.Mutex
	row   store.UserBinding
	found bool
	delay time.Duration
}

func (s *slowUserStore) GetUser(context.Context, store.Protocol, string) (store.UserBinding, bool, error) {
	time.Sleep(s.delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.row, s.found, nil
}

func (s *slowUserStore) SetUser(_ context.Context, row store.UserBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.row, s.found = row, true
	return nil
}

func (s *slowUserStore) DeleteUserTraffic(context.Context, store.Protocol, string) (bool, error) {
	return false, nil
}

// TestUpdateUserKeepsConcurrentFieldEdits 断言按字段提交的并发编辑互不覆盖：每个协程
// 只改自己的字段，最终记录必须同时带有全部改动。
func TestUpdateUserKeepsConcurrentFieldEdits(t *testing.T) {
	st := &slowUserStore{
		row: store.UserBinding{Key: "alice", Protocol: store.ProtocolVNV1, Name: "Alice",
			Client: "edge", Status: store.UserEnabled},
		found: true, delay: 2 * time.Millisecond,
	}
	b := NewBindings(st)

	edits := []func(*Binding){
		func(u *Binding) { u.Config.UpLimit = 1 << 20 },
		func(u *Binding) { u.Config.DownLimit = 2 << 20 },
		func(u *Binding) { u.Config.Concurrency = 7 },
		func(u *Binding) { u.Config.LogLevel = "debug" },
		func(u *Binding) { u.Config.Rule = "office" },
		func(u *Binding) { u.Client = "edge-2" },
	}
	var wg sync.WaitGroup
	for _, edit := range edits {
		wg.Add(1)
		go func(edit func(*Binding)) {
			defer wg.Done()
			if _, err := b.UpdateUser(context.Background(), store.ProtocolVNV1, "alice", func(u *Binding) error {
				edit(u)
				return nil
			}); err != nil {
				t.Errorf("update user: %v", err)
			}
		}(edit)
	}
	wg.Wait()

	got, ok := b.Lookup(context.Background(), store.ProtocolVNV1, "alice")
	if !ok {
		t.Fatal("binding gone")
	}
	cfg := got.Config
	if cfg.UpLimit != 1<<20 || cfg.DownLimit != 2<<20 || cfg.Concurrency != 7 ||
		cfg.LogLevel != "debug" || cfg.Rule != "office" || got.Client != "edge-2" {
		t.Fatalf("concurrent edits lost: %+v client=%s", cfg, got.Client)
	}
}

// TestUpdateUserRejectsMutateError 断言回调失败时不写库，返回原错误。
func TestUpdateUserRejectsMutateError(t *testing.T) {
	st := &slowUserStore{
		row: store.UserBinding{Key: "bob", Protocol: store.ProtocolSS, Name: "Bob",
			Client: "edge", Status: store.UserEnabled},
		found: true,
	}
	b := NewBindings(st)
	want := fmt.Errorf("nope")
	if _, err := b.UpdateUser(context.Background(), store.ProtocolSS, "bob", func(*Binding) error { return want }); err != want {
		t.Fatalf("want mutate error, got %v", err)
	}
	if st.row.Name != "Bob" {
		t.Fatalf("failed update must not write: %+v", st.row)
	}
}
