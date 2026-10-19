package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	s := DefaultServer()
	if s.Server.Listen != "127.0.0.1:9000" {
		t.Fatalf("unexpected server defaults: %+v", s)
	}
	if !s.Admin.Enabled || s.Admin.Port != 8080 || s.Admin.Addr() != "127.0.0.1:8080" {
		t.Fatalf("unexpected admin defaults: %+v", s.Admin)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("default server must be valid: %v", err)
	}
	c := DefaultClient()
	if c.Server.Addr != "127.0.0.1:9000" {
		t.Fatalf("unexpected client default server: %q", c.Server.Addr)
	}
	if c.Routing.Default != "direct" || len(c.Routing.Outbounds) != 2 || len(c.Routing.Rules) != 2 {
		t.Fatalf("unexpected routing defaults: %+v", c.Routing)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("default client must be valid: %v", err)
	}
}

// 示例配置由测试加载，保证 configs/ 下的样例始终可解析、可通过校验。
func TestLoadExampleConfigs(t *testing.T) {
	root := repoRoot(t)

	var s Server
	if err := Load(filepath.Join(root, "configs", "server.yaml"), &s); err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("example server config invalid: %v", err)
	}

	var c Client
	if err := Load(filepath.Join(root, "configs", "client.yaml"), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("example client config invalid: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if err := Load("does-not-exist.yaml", &Server{}); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	path := writeYAML(t, "server:\n  listen: 127.0.0.1:9000\n  listern: 127.0.0.1:9999\n")
	var s Server
	if err := Load(path, &s); err == nil {
		t.Fatal("unknown key must be rejected")
	}
}

// YAML 中缺省的键保留加载前已填入的默认值；给出的键正常覆盖。
func TestLoadPreservesDefaults(t *testing.T) {
	path := writeYAML(t, "server:\n  listen: 0.0.0.0:8080\n")
	s := DefaultServer()
	if err := Load(path, &s); err != nil {
		t.Fatal(err)
	}
	if s.Server.Listen != "0.0.0.0:8080" {
		t.Fatalf("listen = %q", s.Server.Listen)
	}
	if s.Log.Level != "info" {
		t.Fatalf("log level should keep default, got %q", s.Log.Level)
	}
}

// log.console 加载：显式给出时覆盖默认值 false。
func TestLoadLogConsole(t *testing.T) {
	path := writeYAML(t, "server:\n  listen: 127.0.0.1:9000\nlog:\n  level: info\n  output: log/server.log\n  console: true\n")
	s := DefaultServer()
	if err := Load(path, &s); err != nil {
		t.Fatal(err)
	}
	if !s.Log.Console {
		t.Fatal("log.console should be true when set in yaml")
	}

	// 缺省时保留默认 false。
	path = writeYAML(t, "server:\n  listen: 127.0.0.1:9000\nlog:\n  level: info\n  output: log/server.log\n")
	s = DefaultServer()
	if err := Load(path, &s); err != nil {
		t.Fatal(err)
	}
	if s.Log.Console {
		t.Fatal("log.console should default to false")
	}
}

func TestServerValidationErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{"bad listen", "server:\n  listen: not-an-address\n"},
		{"empty listen host", "server:\n  listen: :9000\n"},
		{"bad auth key", "server:\n  auth:\n    key: short\n"},
		{"bad log level", "log:\n  level: verbose\n  output: stderr\n"},
		{"empty log output", "log:\n  level: info\n  output: ''\n"},
		{"admin enabled bad port", "admin:\n  enabled: true\n  port: 70000\n"},
		{"admin enabled zero port", "admin:\n  enabled: true\n  port: 0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeYAML(t, tc.yaml)
			var s Server
			err := Load(path, &s)
			if err == nil {
				err = s.Validate()
			}
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestRoutingValidationErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{
			"unknown outbound type",
			"routing:\n  default: direct\n  outbounds:\n    - {tag: direct, type: socks}\n",
		},
		{
			"duplicated tag",
			"routing:\n  default: direct\n  outbounds:\n    - {tag: x, type: direct}\n    - {tag: x, type: reject}\n",
		},
		{
			"empty tag",
			"routing:\n  default: direct\n  outbounds:\n    - {tag: '', type: direct}\n",
		},
		{
			"default not registered",
			"routing:\n  default: missing\n  outbounds:\n    - {tag: direct, type: direct}\n",
		},
		{
			"rule outbound not registered",
			"routing:\n  default: direct\n  outbounds:\n    - {tag: direct, type: direct}\n  rules:\n    - {name: r, outbound: nope, match: {port: [80]}}\n",
		},
		{
			"rule empty name",
			"routing:\n  default: direct\n  outbounds:\n    - {tag: direct, type: direct}\n  rules:\n    - {name: '', outbound: direct, match: {port: [80]}}\n",
		},
		{
			"rule empty match",
			"routing:\n  default: direct\n  outbounds:\n    - {tag: direct, type: direct}\n  rules:\n    - {name: r, outbound: direct, match: {}}\n",
		},
		{
			"rule bad port",
			"routing:\n  default: direct\n  outbounds:\n    - {tag: direct, type: direct}\n  rules:\n    - {name: r, outbound: direct, match: {port: [70000]}}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeYAML(t, "server:\n  addr: 127.0.0.1:9000\n"+tc.yaml)
			var c Client
			err := Load(path, &c)
			if err == nil {
				err = c.Validate()
			}
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// 空 outbounds 列表必须报错，即使文件中显式给出。
func TestRoutingRequiresOutbounds(t *testing.T) {
	path := writeYAML(t, "server:\n  addr: 127.0.0.1:9000\nrouting:\n  default: direct\n  outbounds: []\n")
	var c Client
	if err := Load(path, &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err == nil {
		t.Fatal("empty outbounds must be invalid")
	}
}

func TestIPv6AddressAccepted(t *testing.T) {
	path := writeYAML(t, "server:\n  listen: '[::1]:9000'\nlog:\n  level: info\n  output: stderr\n")
	var s Server
	if err := Load(path, &s); err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("IPv6 listen must be valid: %v", err)
	}
}

// writeYAML 把内容写进临时文件并返回路径。
func writeYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// repoRoot 从测试工作目录定位模块根（internal/config → 上两级）。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", dir, err)
	}
	return dir
}

// admin 段整体替换默认值；禁用时端口可以任意，加载保留未给出的默认键。
func TestAdminConfig(t *testing.T) {
	path := writeYAML(t, "server:\n  listen: 127.0.0.1:9000\nadmin:\n  enabled: false\n")
	s := DefaultServer()
	if err := Load(path, &s); err != nil {
		t.Fatal(err)
	}
	if s.Admin.Enabled {
		t.Fatal("admin should be disabled")
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("disabled admin must skip port validation: %v", err)
	}

	path = writeYAML(t, "server:\n  listen: 127.0.0.1:9000\nadmin:\n  enabled: true\n  port: 9090\n")
	s = DefaultServer()
	if err := Load(path, &s); err != nil {
		t.Fatal(err)
	}
	if s.Admin.Addr() != "127.0.0.1:9090" {
		t.Fatalf("admin addr = %q", s.Admin.Addr())
	}
}
