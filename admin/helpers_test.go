package admin

// helpers_test.go 提供管理台测试的公共装配：临时库 + AdminService、
// 以及带会话 cookie 的 HTTP 请求封装。登录凭证来自测试装配的播种管理员。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
)

// testAdminCreds 是 newTestAdminService 播种的管理员凭证。
const (
	testAdminUser = "admin"
	testAdminPass = "admin123"
)

// newTestAdminService 打开一个临时 sqlite 业务库并装配 AdminService。
func newTestAdminService(t *testing.T) *AdminService {
	t.Helper()
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "bindings.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	gs := st.(store.GORMSource)
	s, err := NewAdminService("127.0.0.1:0", transport.NewSessionManager(), AdminOptions{
		DB:       gs.GORM(),
		Store:    st,
		Username: testAdminUser,
		Password: testAdminPass,
	})
	if err != nil {
		t.Fatalf("NewAdminService: %v", err)
	}
	t.Cleanup(func() { _ = s.Drain(context.Background()) })
	return s
}

// buildOpsMux 构造业务路由 mux，供测试用（直接复用 registerOpsRoutes）。
func (s *AdminService) buildOpsMux() *http.ServeMux {
	mux := http.NewServeMux()
	s.registerOpsRoutes(mux)
	return mux
}

// doRequest 以播种管理员登录后带会话 cookie 发请求；
// 非 GET 请求自动带上 CSRF token。
func doRequest(t *testing.T, h http.Handler, auth *AuthService, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	sessToken, csrf, err := auth.Login(testAdminUser, testAdminPass)
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(&http.Cookie{
		Name:  sessionCookieName,
		Value: sessToken,
	})
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// anonRequest 不带任何会话凭证发请求（未登录场景）。
func anonRequest(h http.Handler, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
