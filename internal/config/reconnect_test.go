package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultClientReconnects(t *testing.T) {
	cfg := DefaultClient()
	if cfg.Server.Reconnect == nil || !cfg.Server.Reconnect.Enabled {
		t.Fatal("默认应开启重连：客户端需要长期在线")
	}
	if got := cfg.Server.Reconnect.Interval.Std(); got != DefaultReconnectInterval {
		t.Fatalf("interval = %v, want %v", got, DefaultReconnectInterval)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("默认配置应合法: %v", err)
	}
}

// 部分 YAML 只覆盖部分字段：未出现的键必须保留默认值，而不是被清零。
func TestPartialConfigKeepsReconnectDefaults(t *testing.T) {
	cfg := DefaultClient()
	loadString(t, "server:\n  addr: 127.0.0.1:9000\n", &cfg)
	if !cfg.Server.Reconnect.Enabled || cfg.Server.Reconnect.Interval.Std() != DefaultReconnectInterval {
		t.Fatalf("reconnect = %+v, want 默认开启且间隔 %v", cfg.Server.Reconnect, DefaultReconnectInterval)
	}
}

func TestReconnectConfigFromYAML(t *testing.T) {
	cfg := DefaultClient()
	loadString(t, "server:\n  reconnect:\n    enabled: false\n", &cfg)
	if cfg.Server.Reconnect.Enabled {
		t.Fatal("enabled: false 应能关闭重连")
	}

	cfg = DefaultClient()
	loadString(t, "server:\n  reconnect:\n    interval: 30s\n", &cfg)
	if !cfg.Server.Reconnect.Enabled || cfg.Server.Reconnect.Interval.Std() != 30*time.Second {
		t.Fatalf("reconnect = %+v, want 开启且间隔 30s", cfg.Server.Reconnect)
	}
}

// 开启重连却把间隔写成 0 会退化成忙等重连，必须在配置校验阶段就拒绝。
func TestReconnectIntervalMustBePositive(t *testing.T) {
	cfg := DefaultClient()
	cfg.Server.Reconnect = &Reconnect{Enabled: true, Interval: 0}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "reconnect.interval") {
		t.Fatalf("err = %v, want reconnect.interval 校验错误", err)
	}

	// 关闭重连时间隔无意义，不做要求。
	cfg.Server.Reconnect = &Reconnect{Enabled: false}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("关闭重连时应放行: %v", err)
	}
}

// 时长文本在解码时就校验，配置写错不会拖到运行期。
func TestReconnectRejectsBadDuration(t *testing.T) {
	cfg := DefaultClient()
	if err := Load(writeTemp(t, "server:\n  reconnect:\n    interval: 5seconds\n"), &cfg); err == nil {
		t.Fatal("非法时长文本应在加载阶段报错")
	}
}

func loadString(t *testing.T, content string, cfg *Client) {
	t.Helper()
	if err := Load(writeTemp(t, content), cfg); err != nil {
		t.Fatal(err)
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
