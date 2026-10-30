package core

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/rule"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/internal/userclient"
)

// TestLoadRuleSetSeedsDefault 断言首次启动只种 default，且删掉后重启不会复活。
func TestLoadRuleSetSeedsDefault(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	rs := loadRuleSet(ctx, st)
	if !rs.Exists(rule.NameDefault) {
		t.Fatal("default rule should be seeded on an empty table")
	}
	if rs.Exists("all") {
		t.Fatal("only default is preset; all must not be seeded")
	}
	kv, err := st.GetServiceSettings(ctx)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if _, ok := kv[rule.Key(rule.NameDefault)]; !ok {
		t.Fatalf("default rule should be persisted under %q, got %v", rule.Key(rule.NameDefault), kv)
	}

	// 改名单后重新加载：库里的值必须胜过预置值。
	if err := rs.SaveRule(rule.NameDefault, rule.Rule{Reject: []string{"bad.example"}}); err != nil {
		t.Fatalf("save rule: %v", err)
	}
	if got, _ := loadRuleSet(ctx, st).Verdict(rule.NameDefault, "bad.example"); got != rule.Reject {
		t.Fatalf("reloaded verdict = %v, want reject", got)
	}

	// 表里还有别的规则时，删掉的 default 不会被补回来。
	if err := rs.SaveRule("office", rule.Rule{Direct: []string{"*.cn"}}); err != nil {
		t.Fatalf("save office: %v", err)
	}
	if err := rs.DeleteRule(rule.NameDefault); err != nil {
		t.Fatalf("delete default: %v", err)
	}
	reloaded := loadRuleSet(ctx, st)
	if reloaded.Exists(rule.NameDefault) {
		t.Fatal("deleted default must not be re-seeded while other rules exist")
	}
	if !reloaded.Exists("office") {
		t.Fatal("existing rule should survive a reload")
	}

	// 删光之后重启：预置只做一次，不会再冒出 default。
	if err := reloaded.DeleteRule("office"); err != nil {
		t.Fatalf("delete office: %v", err)
	}
	again := loadRuleSet(ctx, st)
	if len(again.List()) != 0 {
		t.Fatalf("rules deleted by the admin must stay deleted, got %v", again.List())
	}
}

// TestLoadRuleSetMigratesLegacyKeys 断言旧前缀 user.rule.* 的行迁到 rule.* 并删掉旧行；
// 新旧同时存在时以新键为准。
func TestLoadRuleSetMigratesLegacyKeys(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if err := st.SetServiceSettings(ctx, map[string]string{
		"user.rule.default": `{"reject":["old.example"],"direct":[]}`,
		"user.rule.office":  `{"reject":[],"direct":["*.legacy"]}`,
		"rule.office":       `{"reject":[],"direct":["*.cn"]}`,
	}); err != nil {
		t.Fatalf("seed legacy keys: %v", err)
	}

	rs := loadRuleSet(ctx, st)
	if got, _ := rs.Verdict(rule.NameDefault, "old.example"); got != rule.Reject {
		t.Fatalf("migrated default verdict = %v, want reject", got)
	}
	// 新旧同名时以新键为准。
	if got, _ := rs.Verdict("office", "www.cn"); got != rule.Direct {
		t.Fatalf("conflicting rule should keep the new key, verdict = %v", got)
	}
	if got, _ := rs.Verdict("office", "www.legacy"); got != rule.Pass {
		t.Fatalf("legacy value must not win, verdict = %v", got)
	}

	kv, err := st.GetServiceSettings(ctx)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	for _, key := range []string{"user.rule.default", "user.rule.office"} {
		if _, ok := kv[key]; ok {
			t.Fatalf("legacy key %q should be removed, got %v", key, kv)
		}
	}
	if _, ok := kv[rule.Key("office")]; !ok {
		t.Fatalf("rule.office should stay, got %v", kv)
	}
}

// TestRuleSetSaveDelete 断言写路径：保存后即时生效、删除后回到放行（default 也能删）。
func TestRuleSetSaveDelete(t *testing.T) {
	ctx := context.Background()
	rs := loadRuleSet(ctx, testStore(t))

	if err := rs.SaveRule("cn", rule.Rule{
		Direct: []string{"*.cn", "10.0.0.0/8"},
		Reject: []string{"ads.cn"},
	}); err != nil {
		t.Fatalf("save rule: %v", err)
	}
	if got, name := rs.Verdict("cn", "www.cn"); got != rule.Direct || name != "cn" {
		t.Fatalf("verdict = %v (%s), want direct (cn)", got, name)
	}
	if got, _ := rs.Verdict("cn", "ads.cn"); got != rule.Reject {
		t.Fatalf("verdict = %v, want reject", got)
	}
	if err := rs.DeleteRule("cn"); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	if rs.Exists("cn") {
		t.Fatal("rule should be gone after delete")
	}
	if got, _ := rs.Verdict("cn", "www.cn"); got != rule.Pass {
		t.Fatalf("missing rule should pass, got %v", got)
	}
	if err := rs.DeleteRule(rule.NameDefault); err != nil {
		t.Fatalf("default rule should be deletable: %v", err)
	}
	if rs.Exists(rule.NameDefault) {
		t.Fatal("default rule should be gone after delete")
	}
	if err := rs.SaveRule("bad name", rule.Rule{}); err == nil {
		t.Fatal("invalid rule name must be rejected")
	}
}

// TestRuleSetEmptyNameUsesDefault 断言没配规则的用户按内置 default 判定（不干预）。
func TestRuleSetEmptyNameUsesDefault(t *testing.T) {
	rs := loadRuleSet(context.Background(), testStore(t))
	if got, name := rs.Verdict("", "example.com"); got != rule.Pass || name != rule.NameDefault {
		t.Fatalf("verdict = %v (%s), want pass (default)", got, name)
	}
}

// TestUserRuleEnforcedOnForward 端到端验证判定发生在服务端：用户绑定到本机出口，
// 命中 reject 名单的目标被拒，未命中的目标照常转发。
func TestUserRuleEnforcedOnForward(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st := testStore(t)
	if err := st.SetUser(ctx, store.UserBinding{
		Key: testUser, Protocol: store.ProtocolVNV1, Name: "alice",
		Status: store.UserEnabled, Client: transport.LocalClientName,
		Config: store.UserConfig{Rule: "office"},
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	rs := loadRuleSet(ctx, st)
	if err := rs.SaveRule("office", rule.Rule{Reject: []string{"blocked.example"}}); err != nil {
		t.Fatalf("save rule: %v", err)
	}

	cfg := config.DefaultServer()
	cfg.Server.Listen = "127.0.0.1:0"
	sessions := transport.NewSessionManager()
	bindings := admin.NewBindings(st)
	local, err := buildLocalExit(config.ServerProxySettings{Routing: config.DefaultServerProxyRouting()})
	if err != nil {
		t.Fatalf("build local exit: %v", err)
	}
	svc := NewConnectorServiceWith(&cfg, sessions, bindings, nil)
	svc.SetSessionConfig(bindings)
	exits := &exitResolver{remote: sessions}
	exits.SetLocal(local)
	svc.SetExitResolver(exits)
	svc.SetRules(rs)
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start connector: %v", err)
	}
	t.Cleanup(func() { _ = svc.Drain(context.Background()) })

	user, err := userclient.Dial(svc.Addr(), protocol.UserAuthRequest{Key: testUser}, nil, nil)
	if err != nil {
		t.Fatalf("dial user: %v", err)
	}
	defer func() { _ = user.Close() }()

	_, err = user.Forward(protocol.Address{Host: "blocked.example", Port: 443})
	if err == nil || !strings.Contains(err.Error(), "rejected by rule office") {
		t.Fatalf("forward to blocked host = %v, want rule rejection", err)
	}

	host, port := startEcho(t)
	conn, err := user.Forward(protocol.Address{Host: host, Port: uint16(port)})
	if err != nil {
		t.Fatalf("forward to echo: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q, want ping", buf)
	}
}
