package store

import (
	"context"
	"testing"
)

// TestServiceSettingsRoundTrip 断言单行运行时配置快照的读写与覆盖。
func TestServiceSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := sqliteStore(t)

	if v, found, err := st.GetServiceSettings(ctx); err != nil || found || v != "" {
		t.Fatalf("empty db: got (%q, %v, %v), want empty and not found", v, found, err)
	}

	const raw = `{"log_level":"warn","public_host":"vpn.example.com"}`
	if err := st.SetServiceSettings(ctx, raw); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, ok, err := st.GetServiceSettings(ctx)
	if err != nil || !ok || got != raw {
		t.Fatalf("get = (%q, %v, %v), want (%q, true, nil)", got, ok, err, raw)
	}

	// 覆盖：固定主键 upsert，仍是一行。
	const next = `{"log_level":"debug"}`
	if err := st.SetServiceSettings(ctx, next); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, ok, err = st.GetServiceSettings(ctx)
	if err != nil || !ok || got != next {
		t.Fatalf("after overwrite = (%q, %v, %v), want (%q, true, nil)", got, ok, err, next)
	}
}
