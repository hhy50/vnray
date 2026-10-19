package cli

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/protocol"
)

// execute 运行命令并捕获标准输出，返回输出与错误。
func execute(t *testing.T, newCmd func() *cobra.Command, args ...string) (string, error) {
	t.Helper()
	cmd := newCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestClientHelp(t *testing.T) {
	out, err := execute(t, NewClientCommand, "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--server", "--config"} {
		if !strings.Contains(out, want) {
			t.Fatalf("client help %q must contain %s", out, want)
		}
	}
	if strings.Contains(out, "--listen") {
		t.Fatal("client help must not list server-only flag --listen")
	}
	if strings.Contains(out, "--log-level") {
		t.Fatal("client help must not list log flags; logs are file-only")
	}
}

func TestRejectPositionalArgs(t *testing.T) {
	if _, err := execute(t, NewClientCommand, "unexpected"); err == nil {
		t.Fatal("client must reject positional arguments")
	}
}

// 日志级别只能在配置文件中给出；非法值在配置校验阶段被拒绝。
func TestInvalidLogLevelFromFile(t *testing.T) {
	path := writeConfig(t, "log:\n  level: verbose\n  output: stderr\n")
	_, err := execute(t, NewClientCommand, "--config", path)
	if err == nil || !strings.Contains(err.Error(), "log.level") {
		t.Fatalf("expected log.level validation error from config, got %v", err)
	}
}

// 关闭重连且服务端未运行时，客户端应快速失败而非进入重连循环。
func TestClientDialFailure(t *testing.T) {
	path := writeConfig(t, "server:\n  reconnect:\n    enabled: false\n")
	_, err := execute(t, NewClientCommand, "--config", path, "--server", "127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "dial server") {
		t.Fatalf("expected dial server error, got %v", err)
	}
}

// 认证信息必须带上生效配置：翻译结果经 Auth 帧编解码往返后逐字段不丢。
func TestNewClientInfoCarriesConfig(t *testing.T) {
	cfg := config.DefaultClient()
	cfg.Name = "default:default:edge-1"
	cfg.Public = "192.168.31.*"
	cfg.Private = "127.0.0.0/8,10.0.0.0/8"
	cfg.Log = &config.Log{Level: "debug", Output: "stdout", Console: true}
	reconnect := config.DefaultReconnect()
	reconnect.Interval = config.Duration(10 * time.Second)

	info := newClientInfo(cfg, reconnect)

	frame, err := protocol.NewAuthFrame(protocol.AuthRequest{Key: "test-key", Info: info})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := protocol.ReadPayload(frame)
	if err != nil {
		t.Fatal(err)
	}
	req, err := protocol.ParseAuthRequest(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.Info, info) {
		t.Fatalf("auth roundtrip info = %+v, want %+v", req.Info, info)
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

func TestVersion(t *testing.T) {
	out, err := execute(t, NewClientCommand, "--version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "client version dev") {
		t.Fatalf("version output = %q", out)
	}
}

// writeConfig 把内容写进临时文件并返回路径。
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
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
