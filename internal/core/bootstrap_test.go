package core

import (
	"context"
	"path/filepath"
	"reflect"

	"testing"

	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/rule"
	"github.com/hhy50/vnray/internal/store"
)

func TestInitialClientAuthKey(t *testing.T) {
	ctx := context.Background()
	cfg := config.DefaultServer()
	cfg.Storage.Driver = "sqlite"
	cfg.Storage.DSN = filepath.Join(t.TempDir(), "bindings.db")
	open := func() store.Store {
		t.Helper()
		st, err := newBindingStore(ctx, &cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	}
	st := open()
	keys, err := st.ListAuthKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].Key != initialClientAuthKey || keys[0].Scope != "*" {
		t.Fatalf("initial key missing or incorrect: count=%d, err=%v", len(keys), err)
	}
	if _, err := authkey.Parse(keys[0].Key); err != nil {
		t.Fatalf("initial key is invalid: %v", err)
	}
	b := newBindings(st)
	if err := b.ReloadAuthKeys(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"default:default:edge", "other:office:edge"} {
		if configured, allowed := b.MatchClientAuthKey(context.Background(), name, initialClientAuthKey); !configured || !allowed {
			t.Fatalf("initial key does not authorize %q", name)
		}
	}
	loadServiceSettings(ctx, st, &cfg)
	settings, err := st.GetServiceSettings(ctx)
	if err != nil || settings[config.SettingHeartbeat] == "" {
		t.Fatalf("service settings were not initialized: %v", err)
	}
	_ = st.Close()
	st = open()
	current, ok, err := st.GetAuthKey(ctx, initialClientAuthKey)
	if err != nil || !ok || current.ID != keys[0].ID || !current.UpdatedAt.Equal(keys[0].UpdatedAt) {
		t.Fatalf("restart rewrote initial key: %v", err)
	}
	current.Scope = "team:*"
	if err := st.SetAuthKey(ctx, current); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st = open()
	current, ok, err = st.GetAuthKey(ctx, initialClientAuthKey)
	if err != nil || !ok || current.Scope != "team:*" {
		t.Fatalf("restart reset edited scope: %v", err)
	}
	if _, err := st.DeleteAuthKey(ctx, initialClientAuthKey); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st = open()
	if keys, err := st.ListAuthKeys(ctx); err != nil || len(keys) != 0 {
		t.Fatalf("restart restored deleted key: count=%d, err=%v", len(keys), err)
	}
}

func TestInitialClientAuthKeyPreservesExistingDatabase(t *testing.T) {
	for _, existing := range []string{"initialized", "settings", "key"} {
		t.Run(existing, func(t *testing.T) {
			ctx := context.Background()
			cfg := config.DefaultServer()
			cfg.Storage.Driver = "sqlite"
			cfg.Storage.DSN = filepath.Join(t.TempDir(), "existing.db")
			st, err := store.Open(ctx, "sqlite", cfg.Storage.DSN)
			if err != nil {
				t.Fatal(err)
			}
			switch existing {
			case "initialized":
				err = st.SetServiceSettings(ctx, map[string]string{store.ServiceSettingsInitializedKey: "true"})
			case "settings":
				err = st.SetServiceSettings(ctx, map[string]string{config.SettingLogLevel: "warn"})
			case "key":
				err = st.SetAuthKey(ctx, store.AuthKey{Key: authkey.Generate(), Scope: "team:*"})
			}
			_ = st.Close()
			if err != nil {
				t.Fatal(err)
			}
			st, err = newBindingStore(ctx, &cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if _, found, err := st.GetAuthKey(ctx, initialClientAuthKey); err != nil || found {
				t.Fatalf("existing database received default key: found=%v err=%v", found, err)
			}
		})
	}
}

func initializedTestStore(t *testing.T, cfg *config.Server) store.Store {
	t.Helper()
	cfg.Storage.Driver = "sqlite"
	cfg.Storage.DSN = filepath.Join(t.TempDir(), "initialized.db")
	st, err := newBindingStore(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestDatabaseBootstrapAndReadOnlyLoaders(t *testing.T) {
	ctx := context.Background()
	cfg := config.DefaultServer()
	cfg.Log.Level = "debug"
	cfg.Plugin.Enabled = []string{"v2ray"}
	st := initializedTestStore(t, &cfg)
	before, err := st.GetServiceSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before[config.SettingLogLevel] != "debug" || before[rule.Key(rule.NameDefault)] == "" {
		t.Fatalf("incomplete defaults: %v", before)
	}
	p, found, err := st.GetPlugin(ctx, "v2ray")
	if err != nil || !found || p.Status != store.PluginEnabled || len(p.Config.Ports) != 2 {
		t.Fatalf("plugin defaults: %+v, %v", p, err)
	}
	loadServiceSettings(ctx, st, &cfg)
	loadRuleSet(ctx, st)
	after, err := st.GetServiceSettings(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("read changed database: %v", err)
	}
	if _, err := st.DeleteAuthKey(ctx, initialClientAuthKey); err != nil {
		t.Fatal(err)
	}
	if err := st.SetServiceSettings(ctx, map[string]string{rule.Key(rule.NameDefault): "", config.SettingLogLevel: "warn"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPlugin(ctx, store.PluginState{Name: "v2ray", Status: store.PluginDisabled}); err != nil {
		t.Fatal(err)
	}
	cfg.Plugin.Enabled = []string{"v2ray", "sqlite-web"}
	restarted, err := newBindingStore(ctx, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if keys, err := restarted.ListAuthKeys(ctx); err != nil || len(keys) != 0 {
		t.Fatalf("restored deleted key: %v", err)
	}
	kv, err := restarted.GetServiceSettings(ctx)
	if err != nil || kv[rule.Key(rule.NameDefault)] != "" || kv[config.SettingLogLevel] != "warn" {
		t.Fatalf("reset data: %v err=%v", kv, err)
	}
	if _, found, err := restarted.GetPlugin(ctx, "sqlite-web"); err != nil || found {
		t.Fatalf("seeded plugin after initialization: %v", err)
	}
	p, _, err = restarted.GetPlugin(ctx, "v2ray")
	if err != nil || p.Status != store.PluginDisabled {
		t.Fatal("disabled plugin reset")
	}
}

func TestConfigAndRuleLoadersDoNotInitialize(t *testing.T) {
	st := testStore(t)
	cfg := config.DefaultServer()
	loadServiceSettings(context.Background(), st, &cfg)
	loadRuleSet(context.Background(), st)
	kv, err := st.GetServiceSettings(context.Background())
	if err != nil || len(kv) != 0 {
		t.Fatalf("loaders seeded database: %v err=%v", kv, err)
	}
}
