package config

import (
	"reflect"
	"testing"
	"time"
)

// TestServiceSettingsRoundTrip 断言整份运行时配置按段摊成 KV 后能原样解回来。
func TestServiceSettingsRoundTrip(t *testing.T) {
	in := DefaultServiceSettings()
	in.PublicHost = "vpn.example.com"
	in.GlobalClient = GlobalClientSettings{
		CustomHosts: []CustomHost{{Domain: "youyuevpn4.route", IP: "192.168.60.1"}},
		IPMaps:      []IPMap{{From: "186.12.12.*", To: "192.168.60.*"}},
	}
	in.Heartbeat = Heartbeat{Enabled: false, Interval: Duration(9 * time.Second), Timeout: Duration(2 * time.Second)}
	in.LogLevel = "debug"
	in.TrafficFlushInterval = Duration(3 * time.Second)
	in.Session.KeepAliveInterval = Duration(20 * time.Second)
	in.Session.KeepAliveTimeout = Duration(25 * time.Second)
	in.Session.MaxStreamWindow = 512 * 1024
	in.Session.AcceptBacklog = 128
	in.Session.StreamOpenTimeout = Duration(60 * time.Second)
	in.LogRotation = LogRotation{MaxSizeMB: 100, MaxBackups: 5, MaxAgeDays: 7}
	in.VLESS.TLS = &VLESSTLS{Enabled: true, ServerName: "node.example.com", CertPEM: "CERT", KeyPEM: "KEY"}

	kv, err := EncodeServiceSettings(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(kv) != 9 {
		t.Fatalf("kv has %d sections, want 9: %v", len(kv), kv)
	}
	if kv[SettingPublicHost] != "vpn.example.com" || kv[SettingTrafficFlush] != "3s" {
		t.Fatalf("scalar sections should be stored as plain text: %v", kv)
	}
	out, err := DecodeServiceSettings(kv, DefaultServiceSettings())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

// TestDecodeServiceSettingsKeepsDefaults 断言缺段保留基线：后续给值对象加字段时，
// 库里旧记录不会把新字段清零；未知段忽略，坏值报错。
func TestDecodeServiceSettingsKeepsDefaults(t *testing.T) {
	base := DefaultServiceSettings()
	out, err := DecodeServiceSettings(map[string]string{SettingLogLevel: "warn"}, base)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.LogLevel != "warn" {
		t.Fatalf("log level = %q, want warn", out.LogLevel)
	}
	if out.Heartbeat != base.Heartbeat || out.Session != base.Session || out.LogRotation != base.LogRotation ||
		!reflect.DeepEqual(out.ServerProxy, base.ServerProxy) {
		t.Fatalf("missing sections were not kept: %+v", out)
	}

	if got, err := DecodeServiceSettings(nil, base); err != nil || !reflect.DeepEqual(got, base) {
		t.Fatalf("empty kv = %+v, %v; want the base unchanged", got, err)
	}
	if got, err := DecodeServiceSettings(map[string]string{"future_section": "x"}, base); err != nil || !reflect.DeepEqual(got, base) {
		t.Fatalf("unknown section should be ignored, got %+v err %v", got, err)
	}
	if _, err := DecodeServiceSettings(map[string]string{SettingHeartbeat: "{oops"}, base); err == nil {
		t.Fatal("broken section must be rejected")
	}
	if _, err := DecodeServiceSettings(map[string]string{SettingTrafficFlush: "soon"}, base); err == nil {
		t.Fatal("broken duration must be rejected")
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
