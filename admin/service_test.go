package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hhy50/vnray/internal/store"
)

func TestParseLimitKB(t *testing.T) {
	for _, tt := range []struct {
		name    string
		raw     string
		want    int64
		invalid bool
	}{
		{name: "empty"},
		{name: "whitespace", raw: " \t "},
		{name: "zero", raw: "0"},
		{name: "normal", raw: " 1024 ", want: 1024},
		{name: "max", raw: "9007199254740991", want: (1<<63 - 1) / 1024},
		{name: "overflow", raw: "9007199254740992", invalid: true},
		{name: "int overflow", raw: "9223372036854775808", invalid: true},
		{name: "negative", raw: "-1", invalid: true},
		{name: "fraction", raw: "1.5", invalid: true},
		{name: "text", raw: "abc", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLimitKB(tt.raw)
			if (err != nil) != tt.invalid || got != tt.want {
				t.Fatalf("parseLimitKB(%q) = %d, %v", tt.raw, got, err)
			}
		})
	}
}

func TestUserLimitRejectsInvalidInput(t *testing.T) {
	service := &AdminService{opts: AdminOptions{Bindings: &Bindings{}}}
	for _, fields := range []url.Values{
		{"user_key": {"alice"}, "up_kbps": {"-1"}},
		{"user_key": {"alice"}, "down_kbps": {"9007199254740992"}},
		{"user_key": {"alice"}, "up_kbps": {"abc"}},
		{"user_key": {"alice"}, "log_level": {"verbose"}},
	} {
		r := httptest.NewRequest(http.MethodPost, "/admin/ops/_user-config", strings.NewReader(fields.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		service.handleUserConfig(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	}
}

func TestSuggestPorts(t *testing.T) {
	for _, tt := range []struct {
		name       string
		current    int
		serverPort int
		wantStart  int
		wantNext   int
	}{
		{name: "running plugin keeps its port", current: 9002, serverPort: 9000, wantStart: 9002, wantNext: 9003},
		{name: "unset starts next to server port", current: 0, serverPort: 9000, wantStart: 9001, wantNext: 9001},
		{name: "no server port either", current: 0, serverPort: 0, wantStart: 1, wantNext: 1},
		{name: "clamped at 65535", current: 65535, serverPort: 9000, wantStart: 65535, wantNext: 65535},
	} {
		t.Run(tt.name, func(t *testing.T) {
			start, next := suggestPorts(tt.current, tt.serverPort)
			if start != tt.wantStart || next != tt.wantNext {
				t.Fatalf("suggestPorts(%d, %d) = %d, %d; want %d, %d",
					tt.current, tt.serverPort, start, next, tt.wantStart, tt.wantNext)
			}
		})
	}
}

func TestPluginFormPorts(t *testing.T) {
	form := url.Values{
		"plugin":     {"v2ray"},
		"action":     {"set_port"},
		"_token":     {"x"},
		"vmess_port": {"11120"},
		"vless_port": {"11121"},
	}
	r := httptest.NewRequest("POST", "/admin/ops/_plugin", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatalf("parse form: %v", err)
	}
	ports, err := pluginFormPorts(r)
	if err != nil {
		t.Fatalf("pluginFormPorts: %v", err)
	}
	if ports[store.ProtocolVMess] != 11120 || ports[store.ProtocolVLESS] != 11121 {
		t.Fatalf("ports = %v, want vmess=11120 vless=11121", ports)
	}

	// 单个端口非法 → 整体拒绝。
	bad := url.Values{"vmess_port": {"0"}}
	rb := httptest.NewRequest("POST", "/admin/ops/_plugin", strings.NewReader(bad.Encode()))
	rb.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := rb.ParseForm(); err != nil {
		t.Fatalf("parse form: %v", err)
	}
	if _, err := pluginFormPorts(rb); err == nil {
		t.Fatal("port 0 should be rejected")
	}

	// 没有任何 <协议>_port 字段 → 拒绝。
	empty := url.Values{"plugin": {"v2ray"}}
	re := httptest.NewRequest("POST", "/admin/ops/_plugin", strings.NewReader(empty.Encode()))
	re.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := re.ParseForm(); err != nil {
		t.Fatalf("parse form: %v", err)
	}
	if _, err := pluginFormPorts(re); err == nil {
		t.Fatal("missing <protocol>_port should be rejected")
	}
}

func TestHandleClientConfig(t *testing.T) {
	bindings := newTestBindings(t)
	service := &AdminService{opts: AdminOptions{Bindings: bindings}}

	post := func(fields url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/admin/api/clients/config", strings.NewReader(fields.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		service.handleClientConfig(w, r)
		return w
	}

	if w := post(url.Values{"name": {"edge-1"}, "config": {"[1,2]"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("non-object JSON: status = %d, want 400", w.Code)
	}
	if w := post(url.Values{"name": {"edge-1"}, "config": {"{oops"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("broken JSON: status = %d, want 400", w.Code)
	}
	if w := post(url.Values{"config": {"{}"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("empty name: status = %d, want 400", w.Code)
	}

	w := post(url.Values{"name": {"edge-1"}, "config": {`{"note":"ops"}`}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d, want 303", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/admin/ops/_client-config?name=edge-1" {
		t.Fatalf("redirect = %q, want the config page of edge-1", got)
	}
	cb, ok := bindings.LookupClient("edge-1")
	if !ok || cb.ServerConfig != `{"note":"ops"}` {
		t.Fatalf("stored config = %+v, want the saved JSON in server_config", cb)
	}

	// 空 config 表示清除；记录仍在（键在、内容空）。
	if w := post(url.Values{"name": {"edge-1"}, "config": {""}}); w.Code != http.StatusSeeOther {
		t.Fatalf("clear: status = %d, want 303", w.Code)
	}
	if cb, ok := bindings.LookupClient("edge-1"); !ok || cb.ServerConfig != "" {
		t.Fatalf("cleared config = %+v, want an empty config", cb)
	}
}

func TestParseLogLevel(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want string
		bad  bool
	}{
		{in: "", want: ""},
		{in: "info", want: ""},
		{in: " INFO ", want: ""},
		{in: "debug", want: "debug"},
		{in: "Warn", want: "warn"},
		{in: "error", want: "error"},
		{in: "verbose", bad: true},
	} {
		got, err := parseLogLevel(tt.in)
		if tt.bad {
			if err == nil {
				t.Errorf("parseLogLevel(%q) = %q, want an error", tt.in, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("parseLogLevel(%q) = (%q, %v), want (%q, nil)", tt.in, got, err, tt.want)
		}
	}
}

// TestHandleClientConfigLogLevel 断言日志级别下拉框写进 server_config 的 log_level 字段，
// 且不破坏文本域里的其他键；改回 info 时字段被删掉（默认即 info）。config 列（上报快照）不受影响。
func TestHandleClientConfigLogLevel(t *testing.T) {
	bindings := newTestBindings(t)
	service := &AdminService{opts: AdminOptions{Bindings: bindings}}

	post := func(fields url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/admin/ops/_client-config", strings.NewReader(fields.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		service.handleClientConfig(w, r)
		return w
	}

	// 先有一条上报快照，断言管理页保存不会碰它。
	bindings.SaveClientReportedConfig("edge-1", `{"name":"edge-1","os":"darwin"}`)

	if w := post(url.Values{"name": {"edge-1"}, "config": {`{"note":"ops"}`}, "log_level": {"debug"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d, want 303", w.Code)
	}
	cb, ok := bindings.LookupClient("edge-1")
	if !ok {
		t.Fatal("client binding not stored")
	}
	cfg := store.ParseClientConfig(cb.ServerConfig)
	if cfg.LogLevel != "debug" {
		t.Fatalf("stored log_level = %q, want debug (server_config %s)", cfg.LogLevel, cb.ServerConfig)
	}
	if !strings.Contains(cb.ServerConfig, `"note"`) {
		t.Fatalf("other keys were dropped: %s", cb.ServerConfig)
	}
	if cb.Config != `{"name":"edge-1","os":"darwin"}` {
		t.Fatalf("reported snapshot was touched: %q", cb.Config)
	}

	if w := post(url.Values{"name": {"edge-1"}, "config": {cb.ServerConfig}, "log_level": {"info"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("reset: status = %d, want 303", w.Code)
	}
	cb, _ = bindings.LookupClient("edge-1")
	if cfg := store.ParseClientConfig(cb.ServerConfig); cfg.LogLevel != "" {
		t.Fatalf("stored log_level = %q after resetting to info, want empty", cfg.LogLevel)
	}
}

func TestParseIPMaps(t *testing.T) {
	for _, tt := range []struct {
		name      string
		froms     []string
		tos       []string
		wantCount int
		wantFrom  string
		invalid   bool
	}{
		{name: "empty all", froms: []string{"", " "}, tos: []string{"", ""}, wantCount: 0},
		{name: "blank rows skipped", froms: []string{"", "123.123.*.*"}, tos: []string{"", "192.168.*.*"}, wantCount: 1, wantFrom: "123.123.*.*"},
		{name: "missing to", froms: []string{"123.123.*.*"}, tos: []string{""}, invalid: true},
		{name: "missing from", froms: []string{""}, tos: []string{"192.168.*.*"}, invalid: true},
		{name: "bad segment count", froms: []string{"1.2.3"}, tos: []string{"1.2.3.4"}, invalid: true},
		{name: "bad to segment", froms: []string{"1.2.3.4"}, tos: []string{"a.2.*.*"}, invalid: true},
		{name: "bad from value", froms: []string{"1.2.3.999"}, tos: []string{"1.2.3.4"}, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIPMaps(tt.froms, tt.tos)
			if (err != nil) != tt.invalid {
				t.Fatalf("error = %v, want invalid=%v", err, tt.invalid)
			}
			if err != nil {
				return
			}
			if len(got) != tt.wantCount {
				t.Fatalf("ip_maps = %d, want %d", len(got), tt.wantCount)
			}
			if tt.wantCount > 0 && got[0].From != tt.wantFrom {
				t.Fatalf("from[0] = %q, want %q", got[0].From, tt.wantFrom)
			}
		})
	}
}

func TestParseCustomHosts(t *testing.T) {
	for _, tt := range []struct {
		name       string
		domains    []string
		ips        []string
		wantCount  int
		wantDomain string
		invalid    bool
	}{
		{name: "empty all", domains: []string{"", ""}, ips: []string{"", " "}, wantCount: 0},
		{name: "blank rows skipped", domains: []string{"", "a.example"}, ips: []string{"", "10.0.0.1"}, wantCount: 1, wantDomain: "a.example"},
		{name: "normalized lowercase", domains: []string{"Example.COM."}, ips: []string{"10.0.0.1"}, wantCount: 1, wantDomain: "example.com"},
		{name: "missing ip", domains: []string{"a.example"}, ips: []string{""}, invalid: true},
		{name: "missing domain", domains: []string{""}, ips: []string{"10.0.0.1"}, invalid: true},
		{name: "bad ip", domains: []string{"a.example"}, ips: []string{"not-an-ip"}, invalid: true},
		{name: "domain is ip literal", domains: []string{"10.0.0.1"}, ips: []string{"10.0.0.2"}, invalid: true},
		{name: "domain has slash", domains: []string{"a/b.example"}, ips: []string{"10.0.0.1"}, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCustomHosts(tt.domains, tt.ips)
			if (err != nil) != tt.invalid {
				t.Fatalf("error = %v, want invalid=%v", err, tt.invalid)
			}
			if err != nil {
				return
			}
			if len(got) != tt.wantCount {
				t.Fatalf("hosts = %d, want %d", len(got), tt.wantCount)
			}
			if tt.wantCount > 0 && got[0].Domain != tt.wantDomain {
				t.Fatalf("domain[0] = %q, want %q", got[0].Domain, tt.wantDomain)
			}
		})
	}
}
