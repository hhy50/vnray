package store

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func testInitialData() InitialData {
	return InitialData{
		AuthKey:  AuthKey{Key: "initial-key", Scope: "*"},
		Settings: map[string]string{"log_level": "info", "heartbeat": "default"},
		Rules:    map[string]string{"rule.default": `{"reject":["blocked.example"]}`},
		Plugins:  []PluginState{{Name: "v2ray", Status: PluginEnabled, Config: PluginConfig{Ports: map[Protocol]int{ProtocolVMess: 11120}}}},
	}
}

func initializeTestStore(t *testing.T, st Store, data InitialData) error {
	t.Helper()
	return (&sqlStore{db: st.(GORMSource).GORM()}).initializeData(context.Background(), data)
}

func TestInitializeRollbackAndRetry(t *testing.T) {
	for _, table := range []string{"vn_auth_keys", "vn_service_settings", "vn_plugin", "vn_initialization"} {
		t.Run(table, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "retry.db")
			st, err := Open(ctx, "sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			db := st.(GORMSource).GORM()
			event := "INSERT"
			if table == "vn_initialization" {
				event = "UPDATE"
			}
			sql := fmt.Sprintf("CREATE TRIGGER reject_initial BEFORE %s ON %s BEGIN SELECT RAISE(ABORT, 'injected failure'); END", event, table)
			if err := db.Exec(sql).Error; err != nil {
				t.Fatal(err)
			}
			_ = st.Close()
			data := testInitialData()
			if failed, err := Open(ctx, "sqlite", path, WithInitialData(data)); err == nil {
				_ = failed.Close()
				t.Fatal("initialization succeeded despite injected failure")
			}
			st, err = Open(ctx, "sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			db = st.(GORMSource).GORM()
			for _, model := range []any{&authKeyModel{}, &serviceSettingsModel{}, &PluginState{}, &initializationModel{}} {
				var count int64
				if err := db.Model(model).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("rollback left %T: count=%d err=%v", model, count, err)
				}
			}
			if err := db.Exec("DROP TRIGGER reject_initial").Error; err != nil {
				t.Fatal(err)
			}
			_ = st.Close()
			st, err = Open(ctx, "sqlite", path, WithInitialData(data))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			assertInitialized(t, st, data)
		})
	}
}

func assertInitialized(t *testing.T, st Store, data InitialData) {
	t.Helper()
	ctx := context.Background()
	keys, err := st.ListAuthKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].Key != data.AuthKey.Key {
		t.Fatalf("keys=%v err=%v", keys, err)
	}
	kv, err := st.GetServiceSettings(ctx)
	expected := map[string]string{}
	for k, v := range data.Settings {
		expected[k] = v
	}
	for k, v := range data.Rules {
		expected[k] = v
	}
	if err != nil || !reflect.DeepEqual(kv, expected) {
		t.Fatalf("settings=%v expected=%v err=%v", kv, expected, err)
	}
	state, found, err := st.GetPlugin(ctx, "v2ray")
	if err != nil || !found || state.Status != PluginEnabled {
		t.Fatalf("plugin=%+v found=%v err=%v", state, found, err)
	}
	var init initializationModel
	if err := st.(GORMSource).GORM().First(&init, 1).Error; err != nil || init.Version != initializationVersion {
		t.Fatalf("version=%+v err=%v", init, err)
	}
}

func TestInitializeConcurrent(t *testing.T) {
	ctx := context.Background()
	st := sqliteStore(t)
	data := testInitialData()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := initializeTestStore(t, st, data); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	assertInitialized(t, st, data)
	if _, err := st.DeleteAuthKey(ctx, data.AuthKey.Key); err != nil {
		t.Fatal(err)
	}
	if err := st.SetServiceSettings(ctx, map[string]string{"rule.default": "", "log_level": "warn"}); err != nil {
		t.Fatal(err)
	}
	if err := st.(GORMSource).GORM().Delete(&PluginState{}, "name = ?", "v2ray").Error; err != nil {
		t.Fatal(err)
	}
	if err := initializeTestStore(t, st, data); err != nil {
		t.Fatal(err)
	}
	if keys, err := st.ListAuthKeys(ctx); err != nil || len(keys) != 0 {
		t.Fatalf("deleted key restored: %v", err)
	}
	kv, err := st.GetServiceSettings(ctx)
	if err != nil || kv["rule.default"] != "" || kv["log_level"] != "warn" {
		t.Fatalf("reset settings: %v, %v", kv, err)
	}
	if _, found, err := st.GetPlugin(ctx, "v2ray"); err != nil || found {
		t.Fatalf("restored deleted plugin: %v", err)
	}
}

func TestInitializeLegacyAndPartial(t *testing.T) {
	cases := []struct {
		name          string
		kv            map[string]string
		wantLevel     string
		wantRule      string
		wantHeartbeat bool
	}{
		{"partial", map[string]string{"log_level": "warn"}, "warn", testInitialData().Rules["rule.default"], true},
		{"completed", map[string]string{"initialized": "true", "rules.seeded": "true", "auth_key_initialized": "true", "log_level": "warn"}, "warn", "", false},
		{"legacy rules", map[string]string{"user.rule.default": "legacy", "user.rule.office": "legacy-office", "rule.office": "edited-office"}, "info", "legacy", true},
		{"auth only", map[string]string{"auth_key_initialized": "true"}, "info", testInitialData().Rules["rule.default"], true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := sqliteStore(t)
			if err := st.SetServiceSettings(ctx, tc.kv); err != nil {
				t.Fatal(err)
			}
			if err := st.SetPlugin(ctx, PluginState{Name: "v2ray", Status: PluginDisabled}); err != nil {
				t.Fatal(err)
			}
			if err := initializeTestStore(t, st, testInitialData()); err != nil {
				t.Fatal(err)
			}
			kv, err := st.GetServiceSettings(ctx)
			if err != nil || kv["log_level"] != tc.wantLevel || kv["rule.default"] != tc.wantRule || (kv["heartbeat"] != "") != tc.wantHeartbeat {
				t.Fatalf("settings=%v err=%v", kv, err)
			}
			for _, k := range []string{"initialized", "rules.seeded", "auth_key_initialized", "user.rule.default", "user.rule.office"} {
				if _, exists := kv[k]; exists {
					t.Fatalf("old key remains: %s", k)
				}
			}
			if tc.name == "legacy rules" && kv["rule.office"] != "edited-office" {
				t.Fatal("legacy value overwrote edited rule")
			}
			if keys, err := st.ListAuthKeys(ctx); err != nil || len(keys) != 0 {
				t.Fatalf("legacy database received default key: %v", err)
			}
			p, _, err := st.GetPlugin(ctx, "v2ray")
			if err != nil || p.Status != PluginDisabled {
				t.Fatalf("disabled plugin enabled: %v", err)
			}
		})
	}
}

func TestInitializeRejectsFutureVersion(t *testing.T) {
	st := sqliteStore(t)
	if err := st.(GORMSource).GORM().Create(&initializationModel{ID: 1, Version: initializationVersion + 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := initializeTestStore(t, st, testInitialData()); err == nil {
		t.Fatal("future version accepted")
	}
}

func TestInitializeAcrossConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent.db")
	stores := make([]Store, 2)
	for i := range stores {
		var err error
		stores[i], err = Open(ctx, "sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer stores[i].Close()
	}
	var wg sync.WaitGroup
	for _, st := range stores {
		wg.Add(1)
		go func(st Store) {
			defer wg.Done()
			if err := initializeTestStore(t, st, testInitialData()); err != nil {
				t.Error(err)
			}
		}(st)
	}
	wg.Wait()
	assertInitialized(t, stores[0], testInitialData())
}
