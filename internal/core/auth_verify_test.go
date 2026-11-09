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
// 条目（配置文件密钥不再生效）；表为空时回退配置文件密钥；两者皆空则拒绝。
func TestVerifyKeyScopedKeys(t *testing.T) {
	newService := func(shared string, keys []authKeySeed) *ConnectorService {
		b := testBindings(t)
		for _, k := range keys {
			if err := b.SetAuthKey(context.Background(), k.key, k.scope); err != nil {
				t.Fatalf("seed auth key %q: %v", k.key, err)
			}
		}
		return &ConnectorService{authKey: shared, sessionCfg: b}
	}

	for _, tt := range []struct {
		name    string
		keys    []authKeySeed
		shared  string
		client  string
		present string
		allow   bool
	}{
		{name: "scope star matches any client", keys: []authKeySeed{{"cvmCM53nGNlSefm13tBl5HTCYplTPi2bfH8xqnz8LsA=", "*"}},
			client: "team1:g:a", present: "cvmCM53nGNlSefm13tBl5HTCYplTPi2bfH8xqnz8LsA=", allow: true},
		{name: "namespace scope", keys: []authKeySeed{{"r2paPcCLxTMRdtXBeKdFpRh95M/DWa7InZFqcPKlm0o=", "team1:*"}},
			client: "team1:g:a", present: "r2paPcCLxTMRdtXBeKdFpRh95M/DWa7InZFqcPKlm0o=", allow: true},
		{name: "namespace scope wrong ns", keys: []authKeySeed{{"r2paPcCLxTMRdtXBeKdFpRh95M/DWa7InZFqcPKlm0o=", "team1:*"}},
			client: "team2:g:a", present: "r2paPcCLxTMRdtXBeKdFpRh95M/DWa7InZFqcPKlm0o=", allow: false},
		{name: "exact client", keys: []authKeySeed{{"iuwkbiGRttr8KBGRESD8TnYQ7pjgF8ObVDp7E5yDckI=", "team2:group:solo"}},
			client: "team2:group:solo", present: "iuwkbiGRttr8KBGRESD8TnYQ7pjgF8ObVDp7E5yDckI=", allow: true},
		{name: "exact client other name", keys: []authKeySeed{{"iuwkbiGRttr8KBGRESD8TnYQ7pjgF8ObVDp7E5yDckI=", "team2:group:solo"}},
			client: "team2:group:other", present: "iuwkbiGRttr8KBGRESD8TnYQ7pjgF8ObVDp7E5yDckI=", allow: false},
		{name: "configured table ignores config key", keys: []authKeySeed{{"iuwkbiGRttr8KBGRESD8TnYQ7pjgF8ObVDp7E5yDckI=", "*"}},
			shared: "6mBcn2BvVg//SqHyz9JJZXctu3JbP42h1WpU1agIMwU=", client: "any:g:a", present: "6mBcn2BvVg//SqHyz9JJZXctu3JbP42h1WpU1agIMwU=", allow: false},
		{name: "empty table falls back to config key", shared: "6mBcn2BvVg//SqHyz9JJZXctu3JbP42h1WpU1agIMwU=",
			client: "any:g:a", present: "6mBcn2BvVg//SqHyz9JJZXctu3JbP42h1WpU1agIMwU=", allow: true},
		{name: "empty table config key mismatch", shared: "6mBcn2BvVg//SqHyz9JJZXctu3JbP42h1WpU1agIMwU=",
			client: "any:g:a", present: "WAhD0D0iFv8aJ10JkbrWbk0a+HEXHZKeneYEt5Wfm8o=", allow: false},
		{name: "nothing configured rejects clients",
			client: "any:g:a", present: "", allow: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := newService(tt.shared, tt.keys)
			err := svc.verifyKey(context.Background(), tt.client, tt.present)
			if tt.allow && err != nil {
				t.Fatalf("verifyKey(%q, %q) = %v, want allow", tt.client, tt.present, err)
			}
			if !tt.allow && !errors.Is(err, errKeyMismatch) {
				t.Fatalf("verifyKey(%q, %q) = %v, want errKeyMismatch", tt.client, tt.present, err)
			}
		})
	}
}
