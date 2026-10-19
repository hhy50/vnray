package admin

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
)

// TestPanelVerify 在启动期跑 steward 的整面板校验：列引用、图标名、过滤器
// 都在这里检查，拼错要到运行期才炸的问题一启动就暴露。同时验证缺 DB 的
// 装配会被拒绝——面板数据与业务绑定表必须同库。
func TestPanelVerify(t *testing.T) {
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "bindings.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	s := &AdminService{clients: transport.NewSessionManager()}

	p, err := newPanel(AdminOptions{DB: st.(store.GORMSource).GORM()}, s)
	if err != nil {
		t.Fatalf("newPanel(shared db): %v", err)
	}
	if err := p.app.Verify(); err != nil {
		t.Fatalf("panel verify (shared db): %v", err)
	}
	p.close()

	if _, err := newPanel(AdminOptions{}, s); err == nil {
		t.Fatal("newPanel without db must fail")
	}
}
