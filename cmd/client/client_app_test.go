package main

import (
	"encoding/json"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/protocol"
)

// 上报信息必须带上生效配置：翻译结果经 ConfigReport 帧编解码往返后逐字段不丢。
func TestNewClientInfoCarriesConfig(t *testing.T) {
	cfg := config.DefaultClient()
	cfg.Group = "office"
	cfg.Name = "edge-1"
	cfg.Public = "192.168.31.*"
	cfg.Private = "127.0.0.0/8,10.0.0.0/8"
	cfg.Log = &config.Log{Level: "debug", Output: "stdout", Console: true}
	reconnect := config.DefaultReconnect()
	reconnect.Interval = config.Duration(10 * time.Second)

	info := newClientInfo(cfg, reconnect)
	if info.Group != "office" || info.Name != "edge-1" {
		t.Fatalf("identity = %q/%q, want office/edge-1", info.Group, info.Name)
	}

	frame, err := protocol.NewConfigReportFrame(protocol.ConfigReportRequest{Info: info})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := protocol.ReadPayload(frame)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Client map[string]json.RawMessage `json:"client"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire.Client["group"]) != `"office"` || string(wire.Client["name"]) != `"edge-1"` || wire.Client["key"] != nil {
		t.Fatalf("reported identity fields = %s", payload)
	}
	req, err := protocol.ParseConfigReportRequest(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.Info, info) {
		t.Fatalf("report roundtrip info = %+v, want %+v", req.Info, info)
	}
	if req.Info.Routing == nil || req.Info.Routing.Default != "direct" || len(req.Info.Routing.Rules) == 0 {
		t.Fatalf("routing config not reported: %+v", req.Info.Routing)
	}
	if req.Info.Log == nil || req.Info.Log.Level != "debug" {
		t.Fatalf("log config not reported: %+v", req.Info.Log)
	}
	if req.Info.Reconnect == nil || !req.Info.Reconnect.Enabled || req.Info.Reconnect.Interval != "10s" {
		t.Fatalf("reconnect config not reported: %+v", req.Info.Reconnect)
	}
}

func TestMissingTransportKeyRejected(t *testing.T) {
	if _, err := newClientApp(config.DefaultClient()); err == nil {
		t.Fatal("client without transport key accepted")
	}
}

func TestNewClientInfoDefaultName(t *testing.T) {
	hostname, _ := os.Hostname()
	for _, tc := range []struct {
		group     string
		wantGroup string
	}{
		{"default", "default"},
		{"", "default"},
		{"office", "office"},
	} {
		t.Run(tc.group, func(t *testing.T) {
			cfg := config.DefaultClient()
			cfg.Group = tc.group
			info := newClientInfo(cfg, config.DefaultReconnect())
			if err := config.ValidateClientIdentity(info.Group, info.Name); err != nil {
				t.Fatalf("default name is invalid: %v", err)
			}
			if info.Group != tc.wantGroup {
				t.Fatalf("group = %q, want %q", info.Group, tc.wantGroup)
			}
			wantPrefix := hostname
			if info.Name != wantPrefix && !strings.HasPrefix(info.Name, wantPrefix+"-") {
				t.Fatalf("default name = %q, want %q or %q-<ip>", info.Name, wantPrefix, wantPrefix)
			}
			if ip := primaryIPv4(); ip != "" && !strings.HasSuffix(info.Name, "-"+ip) {
				t.Errorf("name %q should end with primary IPv4 %q", info.Name, ip)
			}
		})
	}
}

func TestNewClientInfoExplicitName(t *testing.T) {
	for _, tc := range []struct {
		group string
		want  string
	}{
		{"default", "default"},
		{"", "default"},
		{"office", "office"},
	} {
		t.Run(tc.group, func(t *testing.T) {
			cfg := config.DefaultClient()
			cfg.Group = tc.group
			cfg.Name = "edge-1"
			if got := newClientInfo(cfg, config.DefaultReconnect()); got.Group != tc.want || got.Name != "edge-1" {
				t.Fatalf("identity = %q/%q, want %s/edge-1", got.Group, got.Name, tc.want)
			}
		})
	}
}

// primaryIPv4 返回的地址必须是合法 IPv4 且不是回环地址。
func TestPrimaryIPv4(t *testing.T) {
	ip := primaryIPv4()
	if ip == "" {
		t.Skip("no non-loopback IPv4 on this host")
	}
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil || parsed.IsLoopback() {
		t.Fatalf("primaryIPv4 = %q, want a non-loopback IPv4", ip)
	}
}
