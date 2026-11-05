package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hhy50/vnray/internal/rule"
	"github.com/hhy50/vnray/internal/store"
)

// fakeRules 是规则读写口的内存实现：校验口径与 core.ruleSet 一致（都走 rule.Compile）。
type fakeRules struct {
	mu    sync.Mutex
	items map[string]rule.Rule
}

func newFakeRules() *fakeRules {
	return &fakeRules{items: map[string]rule.Rule{}}
}

func (f *fakeRules) List() []rule.Named {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]rule.Named, 0, len(f.items))
	for name, r := range f.items {
		out = append(out, rule.Named{Name: name, Rule: r})
	}
	rule.SortNamed(out)
	return out
}

func (f *fakeRules) Exists(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.items[name]
	return ok
}

func (f *fakeRules) SaveRule(name string, r rule.Rule) error {
	if _, err := rule.Compile(name, r); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[name] = r
	return nil
}

func (f *fakeRules) DeleteRule(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, name)
	return nil
}

// newRulesAdmin 构造接入了内存规则表的管理面服务。
func newRulesAdmin(t *testing.T) (*AdminService, *fakeRules) {
	t.Helper()
	s := newTestAdminService(t)
	rules := newFakeRules()
	if err := rules.SaveRule(rule.NameDefault, rule.Rule{}); err != nil {
		t.Fatalf("seed default rule: %v", err)
	}
	s.opts.Rules = rules
	return s, rules
}

// doJSON 以登录会话发一个带 JSON 体的写请求（doRequest 不带体，这里补一个）。
func doJSON(t *testing.T, h http.Handler, auth *AuthService, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	sessToken, csrf, err := auth.Login(testAdminUser, testAdminPass)
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessToken})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAPIRulesCRUD 断言规则接口：列表、保存、删除（预置的 default 同样可删）。
func TestAPIRulesCRUD(t *testing.T) {
	s, rules := newRulesAdmin(t)
	h := s.wrapOps(s.buildOpsMux())

	var list struct {
		Data struct {
			Rules []apiRuleDTO `json:"rules"`
		} `json:"data"`
	}
	rec := doRequest(t, h, s.auth, http.MethodGet, "/admin/ops/api/rules")
	if rec.Code != http.StatusOK {
		t.Fatalf("list rules = %d, body: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode rules: %v", err)
	}
	if len(list.Data.Rules) != 1 || list.Data.Rules[0].Name != rule.NameDefault {
		t.Fatalf("unexpected rules: %s", rec.Body.String())
	}

	rec = doJSON(t, h, s.auth, http.MethodPut, "/admin/ops/api/rules/office",
		`{"reject":["ads.example"],"direct":["*.cn"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save rule = %d, body: %s", rec.Code, rec.Body.String())
	}
	if got := rules.items["office"]; len(got.Reject) != 1 || len(got.Direct) != 1 {
		t.Fatalf("saved rule = %+v", got)
	}

	// 非法规则名与非法 CIDR 都必须在写路径上被拒。
	if rec = doJSON(t, h, s.auth, http.MethodPut, "/admin/ops/api/rules/bad%20name", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid name = %d, want 400", rec.Code)
	}
	if rec = doJSON(t, h, s.auth, http.MethodPut, "/admin/ops/api/rules/x", `{"reject":["10.0.0.0/33"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid cidr = %d, want 400", rec.Code)
	}

	if rec = doJSON(t, h, s.auth, http.MethodDelete, "/admin/ops/api/rules/office", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete rule = %d, body: %s", rec.Code, rec.Body.String())
	}
	if rules.Exists("office") {
		t.Fatal("rule should be deleted")
	}

	// 预置的 default 同样可删。
	if rec = doJSON(t, h, s.auth, http.MethodDelete, "/admin/ops/api/rules/"+rule.NameDefault, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete default = %d, body: %s", rec.Code, rec.Body.String())
	}
	if rules.Exists(rule.NameDefault) {
		t.Fatal("default rule should be deletable")
	}
}

// TestAPIUserRuleField 断言用户配置读写带上出站规则名，未定义的规则名被拒。
func TestAPIUserRuleField(t *testing.T) {
	s, rules := newRulesAdmin(t)
	if err := rules.SaveRule("office", rule.Rule{Direct: []string{"*.cn"}}); err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "bindings.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s.opts.Bindings = NewBindings(st)
	if err := s.opts.Bindings.SetUser(Binding{
		UserKey: "alice", Protocol: store.ProtocolVNV1, Name: "alice",
		Status: store.UserEnabled, Client: "edge-1",
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	h := s.wrapOps(s.buildOpsMux())

	body := `{"up_kbps":0,"down_kbps":0,"concurrency":0,"log_level":"info","rule":"office"}`
	if rec := doJSON(t, h, s.auth, http.MethodPut, "/admin/ops/api/users/limits?protocol=vn-v1&key=alice", body); rec.Code != http.StatusOK {
		t.Fatalf("save user rule = %d, body: %s", rec.Code, rec.Body.String())
	}
	if got, ok := s.opts.Bindings.Lookup(store.ProtocolVNV1, "alice"); !ok || got.Config.Rule != "office" {
		t.Fatalf("user rule = %q, want office", got.Config.Rule)
	}

	body = `{"up_kbps":0,"down_kbps":0,"concurrency":0,"log_level":"info","rule":"ghost"}`
	if rec := doJSON(t, h, s.auth, http.MethodPut, "/admin/ops/api/users/limits?protocol=vn-v1&key=alice", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown rule = %d, want 400", rec.Code)
	}
}

// TestAPIUserSaveRule 断言新增用户时可以直接选规则：带了就落库，不带就保持原值。
func TestAPIUserSaveRule(t *testing.T) {
	s, rules := newRulesAdmin(t)
	if err := rules.SaveRule("office", rule.Rule{Direct: []string{"*.cn"}}); err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "bindings.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s.opts.Bindings = NewBindings(st)
	h := s.wrapOps(s.buildOpsMux())

	body := `{"protocol":"vn-v1","user_key":"bob","name":"bob","client":"edge-1","enabled":true,"rule":"office"}`
	if rec := doJSON(t, h, s.auth, http.MethodPost, "/admin/ops/api/users", body); rec.Code != http.StatusOK {
		t.Fatalf("create user with rule = %d, body: %s", rec.Code, rec.Body.String())
	}
	if got, ok := s.opts.Bindings.Lookup(store.ProtocolVNV1, "bob"); !ok || got.Config.Rule != "office" {
		t.Fatalf("user rule = %q, want office", got.Config.Rule)
	}

	// 不带 rule 的再次保存（改名）不得把已有规则冲掉。
	body = `{"protocol":"vn-v1","user_key":"bob","name":"bob-2","client":"edge-1"}`
	if rec := doJSON(t, h, s.auth, http.MethodPost, "/admin/ops/api/users", body); rec.Code != http.StatusOK {
		t.Fatalf("resave user = %d, body: %s", rec.Code, rec.Body.String())
	}
	if got, _ := s.opts.Bindings.Lookup(store.ProtocolVNV1, "bob"); got.Config.Rule != "office" || got.Name != "bob-2" {
		t.Fatalf("rule lost on resave: %+v", got)
	}

	body = `{"protocol":"vn-v1","user_key":"carol","name":"carol","client":"edge-1","rule":"ghost"}`
	if rec := doJSON(t, h, s.auth, http.MethodPost, "/admin/ops/api/users", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown rule = %d, want 400", rec.Code)
	}
}
