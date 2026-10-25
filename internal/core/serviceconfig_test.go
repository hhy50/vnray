package core

import (
	"context"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
)

// TestSessionConfigFromDefaultsMatchesBackend 断言配置值对象的会话默认值与 tcp 后端默认值
// 一一对应——两处默认值必须保持一致，否则改配置的语义会漂移。
func TestSessionConfigFromDefaultsMatchesBackend(t *testing.T) {
	got := sessionConfigFromSettings(config.DefaultSessionSettings())
	want := tcp.DefaultSessionConfig()
	if got.KeepAliveInterval != want.KeepAliveInterval || got.KeepAliveTimeout != want.KeepAliveTimeout ||
		got.MaxStreamWindow != want.MaxStreamWindow || got.AcceptBacklog != want.AcceptBacklog ||
		got.StreamOpenTimeout != want.StreamOpenTimeout {
		t.Fatalf("session config = %+v, want %+v", got, want)
	}
}

// TestLoadServiceSettings 断言启动合并：基线取内置默认值与配置文件日志级别，首次启动
// 整份落库，库里有快照则覆盖，坏数据回退基线。
func TestLoadServiceSettings(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cfg := config.DefaultServer()
	cfg.Log.Level = "debug"

	base := loadServiceSettings(ctx, st, &cfg)
	if base.LogLevel != "debug" {
		t.Fatalf("baseline log level = %q, want the config value", base.LogLevel)
	}
	if base.PublicHost != "" || base.Heartbeat != config.DefaultHeartbeat() {
		t.Fatalf("heartbeat/public_host should come from built-in defaults: %+v", base)
	}

	// 首次启动把默认值整份写进库并写初始化标记；空值段（public_host 默认空）不占行。
	seeded, err := st.GetServiceSettings(ctx)
	if err != nil {
		t.Fatalf("read seeded settings: %v", err)
	}
	if _, ok := seeded[store.ServiceSettingsInitializedKey]; !ok {
		t.Fatalf("first start should write the initialized marker, got %v", seeded)
	}
	for _, key := range []string{
		config.SettingLogLevel, config.SettingTrafficFlush, config.SettingHeartbeat,
		config.SettingSession, config.SettingLogRotation, config.SettingServerProxy, config.SettingVLESS,
	} {
		if _, ok := seeded[key]; !ok {
			t.Fatalf("first start should seed %q, got %v", key, seeded)
		}
	}
	if seeded[config.SettingLogLevel] != "debug" {
		t.Fatalf("seeded log level = %q, want the config value", seeded[config.SettingLogLevel])
	}

	if err := st.SetServiceSettings(ctx, map[string]string{
		config.SettingPublicHost:   "db.example.com",
		config.SettingTrafficFlush: "3s",
	}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	got := loadServiceSettings(ctx, st, &cfg)
	if got.PublicHost != "db.example.com" || got.TrafficFlushInterval.Std() != 3*time.Second {
		t.Fatalf("overlay = %+v, want public_host=db.example.com flush=3s", got)
	}

	if err := st.SetServiceSettings(ctx, map[string]string{config.SettingHeartbeat: "{oops"}); err != nil {
		t.Fatalf("seed bad settings: %v", err)
	}
	if got := loadServiceSettings(ctx, st, &cfg); got.LogLevel != "debug" {
		t.Fatalf("bad section should fall back to baseline, got %q", got.LogLevel)
	}
}

// TestLoadServiceSettingsSeedsOnce 断言初始化只做一次：已初始化后重启不重刷默认值，
// 否则用户改过的配置每次启动都会被重置。
func TestLoadServiceSettingsSeedsOnce(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cfg := config.DefaultServer()

	loadServiceSettings(ctx, st, &cfg)
	if err := st.SetServiceSettings(ctx, map[string]string{config.SettingLogLevel: "warn"}); err != nil {
		t.Fatalf("set log level: %v", err)
	}
	if got := loadServiceSettings(ctx, st, &cfg); got.LogLevel != "warn" {
		t.Fatalf("second start = %q, want the stored warn (defaults must not be re-seeded)", got.LogLevel)
	}
}

// TestServiceConfigApplyPersistsAndDispatches 断言 Apply 先落库再热分发，且快照同步更新。
func TestServiceConfigApplyPersistsAndDispatches(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	limits := ratelimit.NewRegistry()
	meter := traffic.NewMeter(limits)
	cfg := config.DefaultServer()
	svc := NewConnectorService(&cfg, transport.NewSessionManager(), nil, limits)
	initial := config.DefaultServiceSettings()
	sc := newServiceConfig(st, svc, meter, nil, nil, initial)

	next := initial
	next.Heartbeat = config.Heartbeat{Enabled: true, Interval: config.Duration(2 * time.Second), Timeout: config.Duration(time.Second)}
	next.TrafficFlushInterval = config.Duration(3 * time.Second)
	next.Session.KeepAliveInterval = config.Duration(20 * time.Second)

	if err := sc.Apply(next); err != nil {
		t.Fatalf("apply: %v", err)
	}

	kv, err := st.GetServiceSettings(ctx)
	if err != nil || len(kv) == 0 {
		t.Fatalf("settings not persisted: (%v, %v)", kv, err)
	}
	back, err := config.DecodeServiceSettings(kv, initial)
	if err != nil {
		t.Fatalf("decode persisted: %v", err)
	}
	if back.Heartbeat != next.Heartbeat || back.TrafficFlushInterval != next.TrafficFlushInterval {
		t.Fatalf("persisted = %+v, want %+v", back, next)
	}

	if got := svc.currentHeartbeat(); got != (HeartbeatOptions{Interval: 2 * time.Second, Timeout: time.Second}) {
		t.Fatalf("heartbeat not dispatched: %+v", got)
	}
	if sc.Current().Session.KeepAliveInterval.Std() != 20*time.Second {
		t.Fatalf("current snapshot not updated: %+v", sc.Current())
	}
}

// TestServiceConfigVLESSTLS 断言 vless TLS 段单独落库、同步快照，且不碰其它字段。
func TestServiceConfigVLESSTLS(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	limits := ratelimit.NewRegistry()
	cfg := config.DefaultServer()
	svc := NewConnectorService(&cfg, transport.NewSessionManager(), nil, limits)
	initial := config.DefaultServiceSettings()
	initial.PublicHost = "vpn.example.com"
	sc := newServiceConfig(st, svc, traffic.NewMeter(limits), nil, nil, initial)

	if got := sc.VLESSTLS(); got != nil {
		t.Fatalf("VLESSTLS() = %+v, want nil before configuration", got)
	}
	want := config.VLESSTLS{Enabled: true, ServerName: "node.example.com", CertPEM: "CERT", KeyPEM: "KEY"}
	if err := sc.SetVLESSTLS(want); err != nil {
		t.Fatalf("set vless tls: %v", err)
	}
	if got := sc.VLESSTLS(); got == nil || *got != want {
		t.Fatalf("VLESSTLS() = %+v, want %+v", got, want)
	}

	kv, err := st.GetServiceSettings(ctx)
	if err != nil || len(kv) == 0 {
		t.Fatalf("settings not persisted: (%v, %v)", kv, err)
	}
	back, err := config.DecodeServiceSettings(kv, initial)
	if err != nil {
		t.Fatalf("decode persisted: %v", err)
	}
	if back.VLESS.TLS == nil || *back.VLESS.TLS != want {
		t.Fatalf("persisted vless tls = %+v, want %+v", back.VLESS.TLS, want)
	}
	if back.PublicHost != initial.PublicHost {
		t.Fatalf("public_host = %q, want %q", back.PublicHost, initial.PublicHost)
	}

	// 管理台保存服务配置是以快照为基线覆盖的：Apply 之后 vless 段必须原样保留，
	// 否则改一次日志级别就把证书冲掉了。
	next := sc.Current()
	next.LogLevel = "debug"
	if err := sc.Apply(next); err != nil {
		t.Fatalf("apply service settings: %v", err)
	}
	if got := sc.VLESSTLS(); got == nil || *got != want {
		t.Fatalf("VLESSTLS() after apply = %+v, want %+v", got, want)
	}
	kv, _ = st.GetServiceSettings(ctx)
	if back, err := config.DecodeServiceSettings(kv, initial); err != nil || back.VLESS.TLS == nil {
		t.Fatalf("vless tls lost after apply: (%+v, %v)", back.VLESS.TLS, err)
	}
}

// TestServiceConfigApplyRejectsInvalid 断言非法配置既不落库也不改运行态。
func TestServiceConfigApplyRejectsInvalid(t *testing.T) {
	st := testStore(t)
	limits := ratelimit.NewRegistry()
	meter := traffic.NewMeter(limits)
	cfg := config.DefaultServer()
	svc := NewConnectorService(&cfg, transport.NewSessionManager(), nil, limits)
	initial := config.DefaultServiceSettings()
	sc := newServiceConfig(st, svc, meter, nil, nil, initial)

	bad := initial
	bad.LogLevel = "verbose"
	if err := sc.Apply(bad); err == nil {
		t.Fatal("Apply accepted an invalid log level")
	}
	if kv, _ := st.GetServiceSettings(context.Background()); len(kv) != 0 {
		t.Fatalf("invalid settings must not be persisted: %v", kv)
	}
}
