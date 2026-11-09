//go:build !vnray_small

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
	path := writeConfig(t, "server:\n  auth_key: AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA=\n  reconnect:\n    enabled: false\n")
	_, err := execute(t, NewClientCommand, "--config", path, "--server", "127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "dial server") {
		t.Fatalf("expected dial server error, got %v", err)
	}
}

func TestVersion(t *testing.T) {
	out, err := execute(t, NewClientCommand, "--version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "client version "+version) {
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
