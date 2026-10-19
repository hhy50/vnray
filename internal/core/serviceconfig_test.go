package core

import (
	"context"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/ratelimit"
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

// TestLoadServiceSettings 断言启动合并：基线取内置默认值与配置文件日志级别，库里有快照
// 则覆盖，坏数据回退基线。
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

	if err := st.SetServiceSettings(ctx, `{"public_host":"db.example.com","traffic_flush_interval":"3s"}`); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	got := loadServiceSettings(ctx, st, &cfg)
	if got.PublicHost != "db.example.com" || got.TrafficFlushInterval.Std() != 3*time.Second {
		t.Fatalf("overlay = %+v, want public_host=db.example.com flush=3s", got)
	}

	if err := st.SetServiceSettings(ctx, "{oops"); err != nil {
		t.Fatalf("seed bad settings: %v", err)
	}
	if got := loadServiceSettings(ctx, st, &cfg); got.LogLevel != "debug" {
		t.Fatalf("bad json should fall back to baseline, got %q", got.LogLevel)
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

	raw, ok, err := st.GetServiceSettings(ctx)
	if err != nil || !ok || raw == "" {
		t.Fatalf("settings not persisted: (%q, %v, %v)", raw, ok, err)
	}
	back, err := config.DecodeServiceSettings(raw, initial)
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
	if _, ok, _ := st.GetServiceSettings(context.Background()); ok {
		t.Fatal("invalid settings must not be persisted")
	}
}
