package core

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/store"
)

// testBindings 在临时目录开一个 sqlite 库并包成绑定表门面，测试结束自动关闭。
func testBindings(t *testing.T) *admin.Bindings {
	t.Helper()
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "bindings.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return admin.NewBindings(st)
}

// authKeySeed 是 verifyKey 用例的一条种子密钥。
type authKeySeed struct{ key, scope string }

// TestVerifyKeyScopedKeys 覆盖接入密钥的判定优先级：密钥表已配置时只认范围匹配的
// 条目（配置文件密钥不再生效）；表为空时回退配置文件密钥；两者皆空则放行。
func TestVerifyKeyScopedKeys(t *testing.T) {
	newService := func(legacy string, keys []authKeySeed) *ConnectorService {
		b := testBindings(t)
		for _, k := range keys {
			if err := b.SetAuthKey(k.key, k.scope); err != nil {
				t.Fatalf("seed auth key %q: %v", k.key, err)
			}
		}
		return &ConnectorService{authKey: legacy, sessionCfg: b}
	}

	for _, tt := range []struct {
		name    string
		keys    []authKeySeed
		legacy  string
		client  string
		present string
		allow   bool
	}{
		{name: "scope star matches any client", keys: []authKeySeed{{"key-global-1", "*"}},
			client: "team1:g:a", present: "key-global-1", allow: true},
		{name: "namespace scope", keys: []authKeySeed{{"key-team-22", "team1:*"}},
			client: "team1:g:a", present: "key-team-22", allow: true},
		{name: "namespace scope wrong ns", keys: []authKeySeed{{"key-team-22", "team1:*"}},
			client: "team2:g:a", present: "key-team-22", allow: false},
		{name: "exact client", keys: []authKeySeed{{"key-solo-33", "team2:group:solo"}},
			client: "team2:group:solo", present: "key-solo-33", allow: true},
		{name: "exact client other name", keys: []authKeySeed{{"key-solo-33", "team2:group:solo"}},
			client: "team2:group:other", present: "key-solo-33", allow: false},
		{name: "configured table ignores legacy key", keys: []authKeySeed{{"key-solo-33", "*"}},
			legacy: "legacy-key-9", client: "any:g:a", present: "legacy-key-9", allow: false},
		{name: "empty table falls back to legacy key", legacy: "legacy-key-9",
			client: "any:g:a", present: "legacy-key-9", allow: true},
		{name: "empty table legacy mismatch", legacy: "legacy-key-9",
			client: "any:g:a", present: "other-key", allow: false},
		{name: "nothing configured allows all",
			client: "any:g:a", present: "", allow: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := newService(tt.legacy, tt.keys)
			err := svc.verifyKey(tt.client, tt.present)
			if tt.allow && err != nil {
				t.Fatalf("verifyKey(%q, %q) = %v, want allow", tt.client, tt.present, err)
			}
			if !tt.allow && !errors.Is(err, errKeyMismatch) {
				t.Fatalf("verifyKey(%q, %q) = %v, want errKeyMismatch", tt.client, tt.present, err)
			}
		})
	}
}
