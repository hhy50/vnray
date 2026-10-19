package admin

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/store"
)

// TestUserCacheTTLAndInvalidation 覆盖凭据点查缓存的两条语义：
// TTL 内绕过 Bindings 的直写不可见（缓存命中）；经 Bindings 的写路径即时失效。
func TestUserCacheTTLAndInvalidation(t *testing.T) {
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "bindings.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	b := NewBindings(st)
	ctx := context.Background()

	if err := b.SetUser(Binding{UserKey: "alice", Protocol: store.ProtocolVMess,
		Name: "Alice", Status: store.UserEnabled, Client: "edge-1"}); err != nil {
		t.Fatalf("set user: %v", err)
	}
	if _, ok := b.LookupUser(store.ProtocolVMess, "alice"); !ok {
		t.Fatal("lookup after set must hit")
	}

	// 绕过 Bindings 直写库：TTL 内缓存仍返回旧值（正是入站热路径要的语义）。
	if err := st.SetUser(ctx, store.UserBinding{Key: "alice", Protocol: store.ProtocolVMess,
		Name: "Alice", Status: store.UserEnabled,
		Client: "edge-2", UpdatedAt: time.Now()}); err != nil {
		t.Fatalf("direct store write: %v", err)
	}
	if u, ok := b.LookupUser(store.ProtocolVMess, "alice"); !ok || u.Client != "edge-1" {
		t.Fatalf("cached lookup = (%+v, %v), want stale edge-1", u, ok)
	}

	// 经 Bindings 覆盖：缓存必须即时失效，读到新值。
	if err := b.SetUser(Binding{UserKey: "alice", Protocol: store.ProtocolVMess,
		Name: "Alice", Status: store.UserEnabled, Client: "edge-9"}); err != nil {
		t.Fatalf("overwrite user: %v", err)
	}
	if u, ok := b.LookupUser(store.ProtocolVMess, "alice"); !ok || u.Client != "edge-9" {
		t.Fatalf("lookup after overwrite = (%+v, %v), want edge-9", u, ok)
	}

	// 删除后：负结果也立即生效。
	if deleted, err := b.DeleteUser(store.ProtocolVMess, "alice"); err != nil || !deleted {
		t.Fatalf("delete = (%v, %v), want (true, nil)", deleted, err)
	}
	if _, ok := b.LookupUser(store.ProtocolVMess, "alice"); ok {
		t.Fatal("lookup after delete must miss")
	}

	// 未注册的 key 也会缓存负结果：再次查询不得再打库（无副作用可断言，至少语义一致）。
	if _, ok := b.LookupUser(store.ProtocolVMess, "alice"); ok {
		t.Fatal("negative lookup must stay negative")
	}
}
