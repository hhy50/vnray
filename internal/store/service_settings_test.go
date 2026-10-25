package store

import (
	"context"
	"testing"
)

// TestServiceSettingsRoundTrip 断言运行时配置按段存取：单段覆盖不动其它段，空值删除该段。
func TestServiceSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := sqliteStore(t)

	if kv, err := st.GetServiceSettings(ctx); err != nil || len(kv) != 0 {
		t.Fatalf("empty db: got (%v, %v), want empty", kv, err)
	}

	if err := st.SetServiceSettings(ctx, map[string]string{
		"public_host": "vpn.example.com",
		"log_level":   "warn",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	kv, err := st.GetServiceSettings(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(kv) != 2 || kv["public_host"] != "vpn.example.com" || kv["log_level"] != "warn" {
		t.Fatalf("get = %v, want two sections", kv)
	}

	// 只写一段：其它段原样保留（插件写 vless 段时不能顺手清掉别人的配置）。
	if err := st.SetServiceSettings(ctx, map[string]string{"vless": `{"tls":{"enabled":true}}`}); err != nil {
		t.Fatalf("set one section: %v", err)
	}
	kv, _ = st.GetServiceSettings(ctx)
	if len(kv) != 3 || kv["public_host"] != "vpn.example.com" {
		t.Fatalf("after single-section write = %v, want the others kept", kv)
	}

	// 覆盖同段。
	if err := st.SetServiceSettings(ctx, map[string]string{"log_level": "debug"}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if kv, _ = st.GetServiceSettings(ctx); kv["log_level"] != "debug" {
		t.Fatalf("log_level = %q, want debug", kv["log_level"])
	}

	// 空值删除该段。
	if err := st.SetServiceSettings(ctx, map[string]string{"vless": ""}); err != nil {
		t.Fatalf("delete section: %v", err)
	}
	kv, _ = st.GetServiceSettings(ctx)
	if _, ok := kv["vless"]; ok {
		t.Fatalf("vless should be deleted, got %v", kv)
	}
}
