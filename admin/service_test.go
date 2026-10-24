package admin

// service_test.go 覆盖 API 层里与数据面无关的校验逻辑：日志级别、端口建议、
// 自定义 host / IP 段映射、插件端口参数，以及客户端配置与用户限速的写入校验。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hhy50/vnray/internal/store"
)

// apiContext 构造一个标准 HTTP 请求与响应记录器：API handler 签名为
// func(w http.ResponseWriter, r *http.Request)，直接用 stdlib 类型即可。
func apiContext(method, target string, body any, pathValues map[string]string) (*httptest.ResponseRecorder, *http.Request) {
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, target, reader)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range pathValues {
		r.SetPathValue(k, v)
	}
	w := httptest.NewRecorder()
	return w, r
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

func TestPortsFromParams(t *testing.T) {
	ports, err := portsFromParams(map[string]string{"vmess_port": "11120", "vless_port": "11121"})
	if err != nil {
		t.Fatalf("portsFromParams: %v", err)
	}
	if ports[store.ProtocolVMess] != 11120 || ports[store.ProtocolVLESS] != 11121 {
		t.Fatalf("ports = %v, want vmess=11120 vless=11121", ports)
	}

	if _, err := portsFromParams(map[string]string{"vmess_port": "0"}); err == nil {
		t.Fatal("port 0 should be rejected")
	}
	// 只有 port 字段（单端口插件）不属于 <协议>_port，由调用方单独处理。
	if got, err := portsFromParams(map[string]string{"port": "9001"}); err != nil || len(got) != 0 {
		t.Fatalf("single-port params should yield no protocol ports, got %v, %v", got, err)
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

func TestClientConfigAPI(t *testing.T) {
	bindings := newTestBindings(t)
	service := &AdminService{opts: AdminOptions{Bindings: bindings}}

	save := func(body apiClientConfigIn, name string) *httptest.ResponseRecorder {
		w, r := apiContext(http.MethodPut, "/admin/ops/api/clients/"+name+"/config", body,
			map[string]string{"name": name})
		service.apiClientConfigSave(w, r)
		return w
	}

	if w := save(apiClientConfigIn{Config: "[1,2]"}, "edge-1"); w.Code != http.StatusBadRequest {
		t.Fatalf("non-object JSON: status = %d, want 400", w.Code)
	}
	if w := save(apiClientConfigIn{Config: "{oops"}, "edge-1"); w.Code != http.StatusBadRequest {
		t.Fatalf("broken JSON: status = %d, want 400", w.Code)
	}
	if w := save(apiClientConfigIn{Config: "{}"}, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("empty name: status = %d, want 400", w.Code)
	}

	if w := save(apiClientConfigIn{Config: `{"note":"ops"}`}, "edge-1"); w.Code != http.StatusOK {
		t.Fatalf("save: status = %d, body %s", w.Code, w.Body.String())
	}
	cb, ok := bindings.LookupClient("edge-1")
	if !ok || cb.ServerConfig != `{"note":"ops"}` {
		t.Fatalf("stored config = %+v, want the saved JSON in server_config", cb)
	}

	// 空 config 表示清除；记录仍在（键在、内容空）。
	if w := save(apiClientConfigIn{Config: ""}, "edge-1"); w.Code != http.StatusOK {
		t.Fatalf("clear: status = %d", w.Code)
	}
	if cb, ok := bindings.LookupClient("edge-1"); !ok || cb.ServerConfig != "" {
		t.Fatalf("cleared config = %+v, want an empty config", cb)
	}
}

// TestClientConfigLogLevel 断言日志级别下拉框写进 server_config 的 log_level 字段，
// 且不破坏文本域里的其他键；改回 info 时字段被删掉（默认即 info）。config 列（上报快照）不受影响。
func TestClientConfigLogLevel(t *testing.T) {
	bindings := newTestBindings(t)
	service := &AdminService{opts: AdminOptions{Bindings: bindings}}

	save := func(body apiClientConfigIn) *httptest.ResponseRecorder {
		w, r := apiContext(http.MethodPut, "/admin/ops/api/clients/edge-1/config", body,
			map[string]string{"name": "edge-1"})
		service.apiClientConfigSave(w, r)
		return w
	}

	bindings.SaveClientReportedConfig("edge-1", `{"name":"edge-1","os":"darwin"}`)

	if w := save(apiClientConfigIn{Config: `{"note":"ops"}`, LogLevel: "debug"}); w.Code != http.StatusOK {
		t.Fatalf("save: status = %d", w.Code)
	}
	cb, ok := bindings.LookupClient("edge-1")
	if !ok {
		t.Fatal("client binding not stored")
	}
	if cfg := store.ParseClientConfig(cb.ServerConfig); cfg.LogLevel != "debug" {
		t.Fatalf("stored log_level = %q, want debug (server_config %s)", cfg.LogLevel, cb.ServerConfig)
	}
	if !strings.Contains(cb.ServerConfig, `"note"`) {
		t.Fatalf("other keys were dropped: %s", cb.ServerConfig)
	}
	if cb.Config != `{"name":"edge-1","os":"darwin"}` {
		t.Fatalf("reported snapshot was touched: %q", cb.Config)
	}

	if w := save(apiClientConfigIn{Config: cb.ServerConfig, LogLevel: "info"}); w.Code != http.StatusOK {
		t.Fatalf("reset: status = %d", w.Code)
	}
	cb, _ = bindings.LookupClient("edge-1")
	if cfg := store.ParseClientConfig(cb.ServerConfig); cfg.LogLevel != "" {
		t.Fatalf("stored log_level = %q after resetting to info, want empty", cfg.LogLevel)
	}
}

func TestUserLimitsAPIRejectsInvalidInput(t *testing.T) {
	service := &AdminService{opts: AdminOptions{Bindings: &Bindings{}}}
	for _, body := range []apiUserLimitsIn{
		{UpKB: -1},
		{DownKB: -1},
		{Concurrency: -1},
		{LogLevel: "verbose"},
	} {
		w, r := apiContext(http.MethodPut, "/admin/ops/api/users/limits?protocol=vn-v1&key=alice", body, nil)
		service.apiUserLimits(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %+v: status = %d, want 400", body, w.Code)
		}
	}
}
