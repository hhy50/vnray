package store

import (
	"context"
	"testing"
)

// TestAuthKeyConformance 是三种驱动共用的密钥表行为契约：按 key upsert（保留 id 与
// created_at）、覆盖换 scope、List 按 key 升序、Delete 幂等、非法记录被拒。
func TestAuthKeyConformance(t *testing.T) {
	for name, open := range drivers(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := open(t)

			mustSetAuthKey(t, st, AuthKey{Key: "key-alpha-01", Scope: "*"})
			mustSetAuthKey(t, st, AuthKey{Key: "key-beta-02", Scope: "team1:*"})

			got, ok, err := st.GetAuthKey(ctx, "key-alpha-01")
			if err != nil || !ok {
				t.Fatalf("get key-alpha-01 = (%+v, %v, %v), want found", got, ok, err)
			}
			if got.Scope != "*" || got.ID == 0 || got.CreatedAt.IsZero() {
				t.Errorf("key-alpha-01 = %+v, want scope=* with id/created_at", got)
			}

			// 覆盖写：同一密钥换 scope，id 与 created_at 保留。
			first := got
			mustSetAuthKey(t, st, AuthKey{Key: "key-alpha-01", Scope: "team2:*"})
			second, _, _ := st.GetAuthKey(ctx, "key-alpha-01")
			if second.Scope != "team2:*" {
				t.Errorf("after overwrite scope = %q, want team2:*", second.Scope)
			}
			if second.ID != first.ID || !second.CreatedAt.Equal(first.CreatedAt) {
				t.Errorf("overwrite changed identity: id %d→%d created %v→%v",
					first.ID, second.ID, first.CreatedAt, second.CreatedAt)
			}

			items, err := st.ListAuthKeys(ctx)
			if err != nil {
				t.Fatalf("list auth keys: %v", err)
			}
			if len(items) != 2 || items[0].Key != "key-alpha-01" || items[1].Key != "key-beta-02" {
				t.Fatalf("list auth keys = %+v, want keys in asc order", items)
			}

			deleted, err := st.DeleteAuthKey(ctx, "key-beta-02")
			if err != nil || !deleted {
				t.Fatalf("delete key-beta-02 = (%v, %v), want (true, nil)", deleted, err)
			}
			deleted, err = st.DeleteAuthKey(ctx, "key-beta-02")
			if err != nil || deleted {
				t.Fatalf("delete key-beta-02 again = (%v, %v), want (false, nil)", deleted, err)
			}

			// 非法记录：空 key 与非法范围一律拒绝。
			for _, bad := range []AuthKey{
				{Key: "", Scope: "*"},
				{Key: "key-bad-scope", Scope: "a*b:*"},
				{Key: "key-bad-scope", Scope: ":*"},
				{Key: "key-bad-scope", Scope: "team*"},
				{Key: "key-bad-scope", Scope: ""},
			} {
				if err := st.SetAuthKey(ctx, bad); err == nil {
					t.Errorf("set auth key %+v: want error", bad)
				}
			}
		})
	}
}

// TestValidateAuthScope 覆盖范围 key 三种合法形态与典型非法输入。
func TestValidateAuthScope(t *testing.T) {
	for _, tt := range []struct {
		scope string
		valid bool
	}{
		{"*", true},
		{"team1:*", true},
		{"default:default:host-192.168.0.2", true},
		{"", false},
		{":*", false},
		{"a:b:*", false},
		{"te*am:*", false},
		{"team*", false},
	} {
		err := ValidateAuthScope(tt.scope)
		if (err == nil) != tt.valid {
			t.Errorf("ValidateAuthScope(%q) = %v, want valid=%v", tt.scope, err, tt.valid)
		}
	}
}

// TestScopeMatches 覆盖范围匹配的三种形态，包括名字缺 namespace 段的边界。
func TestScopeMatches(t *testing.T) {
	for _, tt := range []struct {
		scope, name string
		want        bool
	}{
		{"*", "anything", true},
		{"*", "", true},
		{"team1:*", "team1:group:name", true},
		{"team1:*", "team2:group:name", false},
		{"team1:*", "team1", false},
		{"team1:*", "team1x:group:name", false},
		{"team1:group:name", "team1:group:name", true},
		{"team1:group:name", "team1:group:other", false},
		{"plain", "plain", true},
		{"plain", "plain:x", false},
	} {
		if got := ScopeMatches(tt.scope, tt.name); got != tt.want {
			t.Errorf("ScopeMatches(%q, %q) = %v, want %v", tt.scope, tt.name, got, tt.want)
		}
	}
}

func mustSetAuthKey(t *testing.T, st Store, k AuthKey) {
	t.Helper()
	if err := st.SetAuthKey(context.Background(), k); err != nil {
		t.Fatalf("set auth key %+v: %v", k, err)
	}
}
