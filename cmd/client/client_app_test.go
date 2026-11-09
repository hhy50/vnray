package main

import (
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
	cfg.Name = "default:default:edge-1"
	cfg.Public = "192.168.31.*"
	cfg.Private = "127.0.0.0/8,10.0.0.0/8"
	cfg.Log = &config.Log{Level: "debug", Output: "stdout", Console: true}
	reconnect := config.DefaultReconnect()
	reconnect.Interval = config.Duration(10 * time.Second)

	info := newClientInfo(cfg, reconnect)

	frame, err := protocol.NewConfigReportFrame(protocol.ConfigReportRequest{Info: info})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := protocol.ReadPayload(frame)
	if err != nil {
		t.Fatal(err)
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

// 未配置 name 时默认名为 default:default:${hostname}-${ip}；拿不到 IPv4 则退化成
// 不带 IP 后缀，任何情况下都不应为空或保留旧的单段格式。
func TestNewClientInfoDefaultName(t *testing.T) {
	cfg := config.DefaultClient()
	hostname, _ := os.Hostname()

	info := newClientInfo(cfg, config.DefaultReconnect())

	if info.Name == "" {
		t.Fatal("default name must not be empty")
	}
	wantPrefix := config.DefaultClientNamespace + ":" + config.DefaultClientGroup + ":" + hostname
	if info.Name != wantPrefix && !strings.HasPrefix(info.Name, wantPrefix+"-") {
		t.Fatalf("default name = %q, want %q or %q-<ip>", info.Name, wantPrefix, wantPrefix)
	}
	if ip := primaryIPv4(); ip != "" && !strings.HasSuffix(info.Name, "-"+ip) {
		t.Errorf("name %q should end with primary IPv4 %q", info.Name, ip)
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
