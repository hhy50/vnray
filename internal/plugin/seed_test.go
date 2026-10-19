package plugin

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hhy50/vnray/internal/store"
)

// stubPlugin 是只记录 Enable 调用的占位插件，用于验证种子语义而不真正监听端口。
type stubPlugin struct {
	name       string
	status     Status
	enabled    map[string]string
	enableCnt  int
	enableFail error
}

func (s *stubPlugin) Name() string            { return s.name }
func (s *stubPlugin) Status() Status          { return s.status }
func (s *stubPlugin) Restore(context.Context) {}
func (s *stubPlugin) Enable(_ context.Context, params map[string]string) error {
	s.enabled = params
	s.enableCnt++
	return s.enableFail
}
func (s *stubPlugin) Disable(context.Context) error { return nil }
func (s *stubPlugin) Stop()                         {}

func seedTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newSeedManager(t *testing.T, plugins map[string]Plugin) *Manager {
	t.Helper()
	return &Manager{store: seedTestStore(t), plugins: plugins}
}

// TestSeedEnablesWhenNoRecord 验证库中无记录时按内置默认端口启用。
func TestSeedEnablesWhenNoRecord(t *testing.T) {
	sp := &stubPlugin{name: "stub", status: Status{
		Ports: []PortKV{{Protocol: store.ProtocolVMess, Port: 11120}, {Protocol: store.ProtocolVLESS, Port: 11121}},
	}}
	m := newSeedManager(t, map[string]Plugin{"stub": sp})
	m.SetSeedEnabled([]string{"stub"})
	if err := m.seed(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if sp.enableCnt != 1 {
		t.Fatalf("Enable called %d times, want 1", sp.enableCnt)
	}
	if sp.enabled["vmess_port"] != "11120" || sp.enabled["vless_port"] != "11121" {
		t.Fatalf("enable params = %v, want default ports", sp.enabled)
	}
}

// TestSeedSkipsWhenRecordExists 验证库中已有记录时（含管理台停用）以库为准，不再种子启用。
func TestSeedSkipsWhenRecordExists(t *testing.T) {
	st := seedTestStore(t)
	if err := st.SetPlugin(context.Background(), store.PluginState{Name: "stub"}); err != nil {
		t.Fatalf("set plugin: %v", err)
	}
	sp := &stubPlugin{name: "stub", status: Status{Ports: []PortKV{{Protocol: store.ProtocolVMess, Port: 11120}}}}
	m := &Manager{store: st, plugins: map[string]Plugin{"stub": sp}}
	m.SetSeedEnabled([]string{"stub"})
	if err := m.seed(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if sp.enableCnt != 0 {
		t.Fatalf("Enable called %d times, want 0", sp.enableCnt)
	}
}

// TestSeedUnknownPluginFails 验证未知插件名视为配置错误，启动即失败。
func TestSeedUnknownPluginFails(t *testing.T) {
	m := newSeedManager(t, map[string]Plugin{})
	m.SetSeedEnabled([]string{"nope"})
	if err := m.seed(context.Background()); err == nil {
		t.Fatal("expected error for unknown plugin name")
	}
}

// TestSeedSkipsWithoutDefaultPort 验证无内置端口的插件（如 ss）跳过并告警，不启用也不报错。
func TestSeedSkipsWithoutDefaultPort(t *testing.T) {
	sp := &stubPlugin{name: "ss-like", status: Status{Ports: []PortKV{{Protocol: store.ProtocolSS, Port: 0}}}}
	m := newSeedManager(t, map[string]Plugin{"ss-like": sp})
	m.SetSeedEnabled([]string{"ss-like"})
	if err := m.seed(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if sp.enableCnt != 0 {
		t.Fatalf("Enable called %d times, want 0", sp.enableCnt)
	}
}
