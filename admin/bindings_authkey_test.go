package admin

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hhy50/vnray/internal/store"
)

// newTestBindings 打开一套走临时 sqlite 的绑定表门面。
func newTestBindings(t *testing.T) *Bindings {
	t.Helper()
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "bindings.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewBindings(st)
}

// mustAuthKey 写入一条密钥，失败即终止用例。
func mustAuthKey(t *testing.T, b *Bindings, key, scope string) {
	t.Helper()
	if err := b.SetAuthKey(key, scope); err != nil {
		t.Fatalf("set auth key %q scope %q: %v", key, scope, err)
	}
}

// TestBindingsAuthKeys 覆盖密钥镜像的增删改查与校验：upsert 按密钥、删除幂等、
// 快照有序、短密钥与坏范围被拒。
func TestBindingsAuthKeys(t *testing.T) {
	b := newTestBindings(t)

	if err := b.SetAuthKey("key-alpha-01", "*"); err != nil {
		t.Fatalf("set auth key: %v", err)
	}
	// 同密钥换范围即覆盖，不产生第二条记录。
	if err := b.SetAuthKey("key-alpha-01", "team1:*"); err != nil {
		t.Fatalf("overwrite auth key: %v", err)
	}
	if err := b.SetAuthKey("key-beta-02", "team1:group:name"); err != nil {
		t.Fatalf("set second auth key: %v", err)
	}

	snap := b.AuthKeySnapshot()
	if len(snap) != 2 || snap[0].Key != "key-alpha-01" || snap[1].Key != "key-beta-02" {
		t.Fatalf("snapshot = %+v, want 2 keys in asc order", snap)
	}
	if snap[0].Scope != "team1:*" {
		t.Errorf("scope = %q, want team1:*", snap[0].Scope)
	}
	if snap[0].ID == 0 || snap[0].CreatedAt.IsZero() {
		t.Errorf("mirrored key must carry id and created_at, got %+v", snap[0])
	}

	deleted, err := b.DeleteAuthKey("key-beta-02")
	if err != nil || !deleted {
		t.Fatalf("delete = (%v, %v), want (true, nil)", deleted, err)
	}
	if deleted, _ = b.DeleteAuthKey("key-beta-02"); deleted {
		t.Error("delete again must report false")
	}
	if got := len(b.AuthKeySnapshot()); got != 1 {
		t.Fatalf("snapshot after delete has %d keys, want 1", got)
	}

	for _, bad := range []struct{ key, scope string }{
		{"short", "*"},
		{"", "*"},
		{"key-ok-999", "a:b:*"},
		{"key-ok-999", ""},
	} {
		if err := b.SetAuthKey(bad.key, bad.scope); err == nil {
			t.Errorf("SetAuthKey(%q, %q): want error", bad.key, bad.scope)
		}
	}
}

// TestMatchClientAuthKey 覆盖范围匹配语义：全量、namespace、精确名与未配置。
func TestMatchClientAuthKey(t *testing.T) {
	b := newTestBindings(t)

	// 表为空：configured=false，调用方回退旧校验路径。
	if configured, ok := b.MatchClientAuthKey("any:name:here", "whatever"); configured || ok {
		t.Fatalf("empty table = (%v, %v), want (false, false)", configured, ok)
	}

	mustAuthKey(t, b, "key-global-1", "*")
	mustAuthKey(t, b, "key-team-22", "team1:*")
	mustAuthKey(t, b, "key-solo-33", "team2:group:solo")

	cases := []struct {
		name, key string
		want      bool
	}{
		{"team1:g:a", "key-team-22", true},
		{"team1:g:b", "key-global-1", true},
		{"team2:group:solo", "key-solo-33", true},
		{"team2:group:solo", "key-team-22", false},
		{"team2:group:solo", "key-global-1", true},
		{"team1:g:a", "key-solo-33", false},
		{"team1:g:a", "wrong-key", false},
	}
	for _, tt := range cases {
		configured, ok := b.MatchClientAuthKey(tt.name, tt.key)
		if !configured {
			t.Fatalf("configured must stay true once keys exist")
		}
		if ok != tt.want {
			t.Errorf("MatchClientAuthKey(%q, %q) = %v, want %v", tt.name, tt.key, ok, tt.want)
		}
	}
}
