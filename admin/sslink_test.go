package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/store"
)

func TestSSLink(t *testing.T) {
	for _, host := range []string{"vpn.example.com", "192.0.2.1", "2001:db8::1"} {
		t.Run(host, func(t *testing.T) {
			const password = "secret:@/?# 中文"
			const name = "节点 A / #1"
			raw := ssLink(shareTarget{Host: host, Port: 8388}, "chacha20-poly1305", password, name)
			link, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			credentials, err := base64.RawURLEncoding.DecodeString(link.User.Username())
			if err != nil || string(credentials) != "chacha20-ietf-poly1305:"+password {
				t.Fatalf("credentials = %q, %v", credentials, err)
			}
			if link.Scheme != "ss" || link.Hostname() != host || link.Port() != "8388" || link.Fragment != name {
				t.Fatalf("link = %s", raw)
			}
		})
	}
	if got := ssLink(shareTarget{Host: "example.com"}, "aes-256-gcm", "secret", ""); got != "" {
		t.Fatalf("link without port = %q", got)
	}
}

func TestSSUserMethodAndShareLink(t *testing.T) {
	s := newTestAdminService(t)
	s.opts.Bindings = NewBindings(s.opts.Store)
	s.opts.Plugins = plugin.NewManager(s.opts.Store, s.opts.Bindings, nil, nil)
	s.opts.PublicHost = "vpn.example.com"
	p, err := s.opts.Plugins.Get("ss")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.(plugin.PortSetter).SetPort(context.Background(), 8388); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305"} {
		t.Run(method, func(t *testing.T) {
			w, r := apiContext(http.MethodPost, "/admin/ops/api/users", map[string]any{
				"protocol": "ss", "user_key": method + "-password", "name": "SS 用户",
				"client": "edge", "ss_method": method,
			}, nil)
			s.apiUserSave(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("save = %d: %s", w.Code, w.Body.String())
			}
			w, r = apiContext(http.MethodPut, "/admin/ops/api/users/limits?protocol=ss&key="+method+"-password",
				map[string]any{"up_kbps": 12, "concurrency": 3, "log_level": "info"}, nil)
			s.apiUserLimits(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("limits = %d: %s", w.Code, w.Body.String())
			}
			u, ok := NewBindings(s.opts.Store).Lookup(store.ProtocolSS, method+"-password")
			if !ok || u.Config.SSMethod != method || u.Config.UpLimit != 12*1024 {
				t.Fatalf("persisted config = %+v, found = %v", u.Config, ok)
			}
		})
	}
	if err := s.opts.Bindings.PutUser(store.UserBinding{
		Protocol: store.ProtocolSS, Key: "legacy-password", Name: "旧用户", Client: "edge", Status: store.UserEnabled,
	}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.apiUsers(w, httptest.NewRequest(http.MethodGet, "/admin/ops/api/users", nil))
	var response struct {
		Data struct{ Users []apiUserDTO } `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data.Users) != 4 {
		t.Fatalf("users = %s", w.Body.String())
	}
	for _, user := range response.Data.Users {
		if !user.Shareable || !strings.HasPrefix(user.ShareLink, "ss://") || user.ShareLinkLegacy != "" {
			t.Fatalf("share = %+v", user)
		}
		if user.UserKey == "legacy-password" && user.SSMethod != plugin.DefaultSSMethod {
			t.Fatalf("legacy method = %q", user.SSMethod)
		}
		link, _ := url.Parse(user.ShareLink)
		credentials, err := base64.RawURLEncoding.DecodeString(link.User.Username())
		if err != nil || string(credentials) != user.SSMethod+":"+user.UserKey || link.Host != "vpn.example.com:8388" {
			t.Fatalf("share credentials or endpoint mismatch: %s", user.ShareLink)
		}
	}
	for _, method := range []string{"", "rc4-md5", "unknown"} {
		w, r := apiContext(http.MethodPost, "/admin/ops/api/users", map[string]any{
			"protocol": "ss", "user_key": "bad-method", "name": "bad", "client": "edge", "ss_method": method,
		}, nil)
		s.apiUserSave(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("method %q = %d", method, w.Code)
		}
	}
}
