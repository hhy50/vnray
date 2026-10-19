package config

import (
	"reflect"
	"testing"
	"time"
)

// TestServiceSettingsRoundTrip 断言整份运行时配置经 JSON 编解码后不丢字段。
func TestServiceSettingsRoundTrip(t *testing.T) {
	in := DefaultServiceSettings()
	in.PublicHost = "vpn.example.com"
	in.Heartbeat = Heartbeat{Enabled: false, Interval: Duration(9 * time.Second), Timeout: Duration(2 * time.Second)}
	in.LogLevel = "debug"
	in.TrafficFlushInterval = Duration(3 * time.Second)
	in.Session.KeepAliveInterval = Duration(20 * time.Second)
	in.Session.KeepAliveTimeout = Duration(25 * time.Second)
	in.Session.MaxStreamWindow = 512 * 1024
	in.Session.AcceptBacklog = 128
	in.Session.StreamOpenTimeout = Duration(60 * time.Second)
	in.LogRotation = LogRotation{MaxSizeMB: 100, MaxBackups: 5, MaxAgeDays: 7}

	raw, err := EncodeServiceSettings(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeServiceSettings(raw, DefaultServiceSettings())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

// TestDecodeServiceSettingsKeepsDefaults 断言缺键保留基线：后续给值对象加字段时，
// 库里旧记录不会把新字段清零。
func TestDecodeServiceSettingsKeepsDefaults(t *testing.T) {
	base := DefaultServiceSettings()
	out, err := DecodeServiceSettings(`{"log_level":"warn"}`, base)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.LogLevel != "warn" {
		t.Fatalf("log level = %q, want warn", out.LogLevel)
	}
	if out.Heartbeat != base.Heartbeat || out.Session != base.Session || out.LogRotation != base.LogRotation ||
		!reflect.DeepEqual(out.ServerProxy, base.ServerProxy) {
		t.Fatalf("missing keys were not kept: %+v", out)
	}

	// 空串直接用基线。
	if got, err := DecodeServiceSettings("  ", base); err != nil || !reflect.DeepEqual(got, base) {
		t.Fatalf("empty raw = %+v, %v; want the base unchanged", got, err)
	}
}

// TestServiceSettingsValidate 覆盖各字段的非法取值。
func TestServiceSettingsValidate(t *testing.T) {
	if err := DefaultServiceSettings().Validate(); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
	for _, tt := range []struct {
		name string
		mut  func(*ServiceSettings)
	}{
		{"bad level", func(s *ServiceSettings) { s.LogLevel = "verbose" }},
		{"public host with scheme", func(s *ServiceSettings) { s.PublicHost = "https://x" }},
		{"zero flush interval", func(s *ServiceSettings) { s.TrafficFlushInterval = 0 }},
		{"zero heartbeat interval when enabled", func(s *ServiceSettings) { s.Heartbeat.Interval = 0 }},
		{"small stream window", func(s *ServiceSettings) { s.Session.MaxStreamWindow = 1024 }},
		{"negative keepalive", func(s *ServiceSettings) { s.Session.KeepAliveInterval = -1 }},
		{"zero rotate size", func(s *ServiceSettings) { s.LogRotation.MaxSizeMB = 0 }},
		{"negative backups", func(s *ServiceSettings) { s.LogRotation.MaxBackups = -1 }},
		{"empty routing", func(s *ServiceSettings) { s.ServerProxy.Routing = Routing{} }},
		{"bad public net", func(s *ServiceSettings) { s.ServerProxy.Public = "not-a-network" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := DefaultServiceSettings()
			tt.mut(&s)
			if err := s.Validate(); err == nil {
				t.Fatalf("Validate accepted invalid settings: %+v", s)
			}
		})
	}
}

// TestRoutingTextRoundTrip 断言 routing 文本编解码往返；未知键被拒而非静默忽略，空文本回落默认。
func TestRoutingTextRoundTrip(t *testing.T) {
	rc := DefaultServerProxyRouting()
	rc.Rules = []Rule{{Name: "block", Outbound: "reject", Match: Match{Host: []string{"blocked.example"}}}}

	text, err := EncodeRoutingText(rc)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := DecodeRoutingText(text)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(back, rc) {
		t.Fatalf("round trip = %+v, want %+v", back, rc)
	}

	if _, err := DecodeRoutingText("bogus_key: 1"); err == nil {
		t.Fatal("unknown key must be rejected")
	}
	if got, err := DecodeRoutingText("  "); err != nil || !reflect.DeepEqual(got, DefaultServerProxyRouting()) {
		t.Fatalf("empty text should fall back to default, got %+v err %v", got, err)
	}
}
