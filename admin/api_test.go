package admin

// api_test.go 覆盖 Vue 管理台消费的 JSON 接口与 SPA 静态托管：认证由
// 自有中间件判定，接口只做取数与序列化。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestAPISession(t *testing.T) {
	s := newTestAdminService(t)
	h := s.wrapOps(s.buildOpsMux())

	var got struct {
		Code int `json:"code"`
		Data struct {
			User struct {
				Username string `json:"username"`
				Name     string `json:"name"`
			} `json:"user"`
			CSRF string `json:"csrf"`
		} `json:"data"`
	}

	rec := doRequest(t, h, s.auth, http.MethodGet, "/admin/ops/api/session")
	if rec.Code != http.StatusOK {
		t.Fatalf("session = %d, body: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if got.Code != 0 || got.Data.User.Username != testAdminUser {
		t.Fatalf("unexpected session: %s", rec.Body.String())
	}
	if got.Data.CSRF == "" {
		t.Fatal("session must hand out the CSRF token for writes")
	}
}

func TestRequireAuth(t *testing.T) {
	s := newTestAdminService(t)
	h := s.wrapOps(s.buildOpsMux())

	// 未登录的 JSON 请求返回 401
	if rec := anonRequest(h, http.MethodGet, "/admin/ops/api/clients"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous clients = %d, want 401", rec.Code)
	}
	// 未登录的写请求同样返回 401
	if rec := anonRequest(h, http.MethodPost, "/admin/ops/api/clients/1/kick"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous kick = %d, want 401", rec.Code)
	}

	// 登录后即可访问全部接口；kick 不存在的客户端由 handler 回 404。
	if rec := doRequest(t, h, s.auth, http.MethodGet, "/admin/ops/api/clients"); rec.Code != http.StatusOK {
		t.Fatalf("clients = %d, body: %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, h, s.auth, http.MethodPost, "/admin/ops/api/clients/1/kick"); rec.Code != http.StatusNotFound {
		t.Fatalf("kick of missing client = %d, want 404", rec.Code)
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	s := newTestAdminService(t)

	// 错误口令
	if _, _, err := s.auth.Login(testAdminUser, "wrong-pass"); err == nil {
		t.Fatal("login with wrong password must fail")
	}
	// 不存在的用户名（不暴露与口令错误的区别）
	if _, _, err := s.auth.Login("nobody", testAdminPass); err == nil {
		t.Fatal("login with unknown user must fail")
	}
}

func TestCSRFRejected(t *testing.T) {
	s := newTestAdminService(t)
	h := s.wrapOps(s.buildOpsMux())

	sessToken, _, err := s.auth.Login(testAdminUser, testAdminPass)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/ops/api/clients/1/kick", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessToken})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("write without CSRF token = %d, want 403", rec.Code)
	}
}

func TestSPAHandler(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<!doctype html><title>spa</title>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	h := spaHandler(fsys)

	for _, tt := range []struct {
		name      string
		path      string
		wantCode  int
		wantBody  string
		wantCache string
	}{
		{"root serves index", "/admin/ui/", http.StatusOK, "<title>spa</title>", ""},
		{"deep link falls back to index", "/admin/ui/clients", http.StatusOK, "<title>spa</title>", ""},
		{"unknown nested path is a route", "/admin/ui/clients/42", http.StatusOK, "<title>spa</title>", ""},
		{"static asset is served", "/admin/ui/assets/app.js", http.StatusOK, "console.log(1)", "immutable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != tt.wantCode {
				t.Fatalf("%s = %d, want %d", tt.path, rec.Code, tt.wantCode)
			}
			if body := rec.Body.String(); !strings.Contains(body, tt.wantBody) {
				t.Fatalf("%s body = %q, want to contain %q", tt.path, body, tt.wantBody)
			}
			if tt.wantCache != "" && !strings.Contains(rec.Header().Get("Cache-Control"), tt.wantCache) {
				t.Fatalf("%s cache-control = %q", tt.path, rec.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestMountUIWithoutAssets(t *testing.T) {
	RegisterUI(nil)
	t.Cleanup(func() { RegisterUI(nil) })
	mux := http.NewServeMux()
	mountUI(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/ui/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("without assets the SPA route must stay unregistered, got %d", rec.Code)
	}
}
