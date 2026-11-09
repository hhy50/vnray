package inbound

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hhy50/vnray/internal/store"
)

func seedTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestInitialStates(t *testing.T) {
	states, err := InitialStates([]string{"v2ray", "ss", "vnray-user"})
	if err != nil {
		t.Fatal(err)
	}
	// 只有 v2ray 需要初始记录：ss 与 vnray-user 的默认值由各自插件给出，sqlite-web 已独立成包。
	if len(states) != 1 {
		t.Fatalf("states: %+v", states)
	}
	if states[0].Status != store.PluginEnabled || states[0].Config.Ports[store.ProtocolVMess] != defaultVMessPort || states[0].Config.Ports[store.ProtocolVLESS] != defaultVLESSPort {
		t.Fatalf("v2ray: %+v", states[0])
	}
	if _, err := InitialStates([]string{"unknown"}); err == nil {
		t.Fatal("unknown plugin accepted")
	}
}
