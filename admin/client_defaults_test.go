package admin

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/protocol"
)

func TestGlobalClientSeparateLayers(t *testing.T) {
	s := newTestAdminService(t)
	b := NewBindings(s.opts.Store)
	s.opts.Bindings = b
	global := config.GlobalClientSettings{
		CustomHosts: []config.CustomHost{{Domain: "VPN.route.", IP: "192.168.60.1"}, {Domain: "global.route", IP: "192.168.60.2"}},
		IPMaps:      []config.IPMap{{From: "186.12.*.*", To: "192.168.*.*"}},
	}
	b.SetGlobalClientConfig(global)
	global.CustomHosts[0].IP = "127.0.0.99"
	if got := b.PushConfig("never-connected"); len(got.ClientConfig.CustomHosts) != 0 || len(got.GlobalConfig.CustomHosts) != 2 || got.GlobalConfig.CustomHosts[0].IP != "192.168.60.1" {
		t.Fatalf("global config for new client: %+v", got)
	}
	raw := `{"custom_hosts":[{"domain":"vpn.route","ip":"10.0.0.1"}],"ip_maps":[{"from":"186.12.12.*","to":"10.0.0.*"}]}`
	if err := b.SetClient(ClientBinding{ClientKey: "edge", ServerConfig: raw}); err != nil {
		t.Fatal(err)
	}
	local := b.ClientPushConfig("edge")
	got := b.PushConfig("edge")
	if !reflect.DeepEqual(got.ClientConfig, local) || len(got.GlobalConfig.CustomHosts) != 2 || got.GlobalConfig.CustomHosts[0].IP != "192.168.60.1" || len(got.GlobalConfig.IPMaps) != 1 {
		t.Fatalf("server mixed independent layers: %+v", got)
	}
	got.GlobalConfig.CustomHosts[1].IP = "127.0.0.99"
	got.GlobalConfig.IPMaps[0].To = "127.0.0.*"
	if next := b.PushConfig("edge"); next.GlobalConfig.CustomHosts[1].IP != "192.168.60.2" || next.GlobalConfig.IPMaps[0].To != "192.168.*.*" {
		t.Fatalf("snapshot leaked: %+v", next)
	}
	w, r := apiContext(http.MethodGet, "/admin/ops/api/clients/edge", nil, map[string]string{"name": "edge"})
	s.apiClientDetail(w, r)
	var detail struct {
		Data struct {
			Hosts  []protocol.CustomHost `json:"hosts"`
			IPMaps []protocol.IPMap      `json:"ip_maps"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(detail.Data.Hosts, local.CustomHosts) || !reflect.DeepEqual(detail.Data.IPMaps, local.IPMaps) {
		t.Fatalf("detail exposes inherited values: %s", w.Body.String())
	}
	b.SetGlobalClientConfig(config.GlobalClientSettings{})
	if got := b.PushConfig("edge"); !reflect.DeepEqual(got.ClientConfig, local) || len(got.GlobalConfig.CustomHosts) != 0 || len(got.GlobalConfig.IPMaps) != 0 {
		t.Fatalf("clear globals: %+v, want %+v", got, local)
	}
	b.SetGlobalClientConfig(global)
	if err := b.SetClient(ClientBinding{ClientKey: "edge"}); err != nil {
		t.Fatal(err)
	}
	if got := b.PushConfig("edge"); len(got.ClientConfig.IPMaps) != 0 || len(got.ClientConfig.CustomHosts) != 0 || len(got.GlobalConfig.IPMaps) != 1 || got.GlobalConfig.CustomHosts[0].IP != global.CustomHosts[0].IP {
		t.Fatalf("clear local did not restore globals: %+v", got)
	}
}

// TestNormalizeGlobalClient 断言全局配置入库前的归一化口径：去空白、域名小写去尾点、
// 空行忽略、同名行原样保留（去重属于客户端的匹配语义，服务端不改写规则顺序），
// 任一行非法则整份拒绝，不写入半份配置。
func TestNormalizeGlobalClient(t *testing.T) {
	got, err := normalizeGlobalClient(config.GlobalClientSettings{
		CustomHosts: []config.CustomHost{{Domain: " VPN.Route. ", IP: " 192.168.60.1 "}, {}},
		IPMaps:      []config.IPMap{{From: " 186.12.12.* ", To: " 192.168.60.* "}, {}},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := config.GlobalClientSettings{
		CustomHosts: []config.CustomHost{{Domain: "vpn.route", IP: "192.168.60.1"}},
		IPMaps:      []config.IPMap{{From: "186.12.12.*", To: "192.168.60.*"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized = %+v, want %+v", got, want)
	}

	dup, err := normalizeGlobalClient(config.GlobalClientSettings{CustomHosts: []config.CustomHost{
		{Domain: "vpn.route", IP: "10.0.0.1"},
		{Domain: "VPN.route.", IP: "10.0.0.2"},
	}})
	if err != nil {
		t.Fatalf("normalize duplicates: %v", err)
	}
	if len(dup.CustomHosts) != 2 || dup.CustomHosts[1].IP != "10.0.0.2" {
		t.Fatalf("duplicates were merged: %+v", dup.CustomHosts)
	}

	empty, err := normalizeGlobalClient(config.GlobalClientSettings{})
	if err != nil || len(empty.CustomHosts) != 0 || len(empty.IPMaps) != 0 {
		t.Fatalf("empty normalize = %+v, err %v", empty, err)
	}

	for _, bad := range []config.GlobalClientSettings{
		{CustomHosts: []config.CustomHost{{Domain: "vpn", IP: "300.1.1.1"}}},
		{CustomHosts: []config.CustomHost{{Domain: "", IP: "10.0.0.1"}}},
		{IPMaps: []config.IPMap{{From: "186.12.12.1", To: ""}}},
		{IPMaps: []config.IPMap{{From: "186.12.*", To: "192.168.60.*"}}},
	} {
		if _, err := normalizeGlobalClient(bad); err == nil {
			t.Fatalf("accepted invalid global config: %+v", bad)
		}
	}
}

// TestPushConfigWithoutGlobal 断言尚未注入全局配置时的分层快照：独立层照常下发，
// 全局层为空而不是被独立层顶替或触发空指针。
func TestPushConfigWithoutGlobal(t *testing.T) {
	s := newTestAdminService(t)
	b := NewBindings(s.opts.Store)
	if err := b.SetClient(ClientBinding{ClientKey: "edge",
		ServerConfig: `{"custom_hosts":[{"domain":"local.route","ip":"10.0.0.1"}]}`}); err != nil {
		t.Fatal(err)
	}
	got := b.PushConfig("edge")
	if len(got.GlobalConfig.CustomHosts) != 0 || len(got.GlobalConfig.IPMaps) != 0 {
		t.Fatalf("global layer must stay empty: %+v", got.GlobalConfig)
	}
	if len(got.ClientConfig.CustomHosts) != 1 || got.ClientConfig.CustomHosts[0].Domain != "local.route" {
		t.Fatalf("independent layer = %+v", got.ClientConfig)
	}
	if un := b.PushConfig("never-connected"); len(un.ClientConfig.CustomHosts) != 0 || len(un.GlobalConfig.IPMaps) != 0 {
		t.Fatalf("unknown client = %+v", un)
	}
}

func TestAPIClientConfig(t *testing.T) {
	fake := &fakeSettings{cur: config.DefaultServiceSettings()}
	s := &AdminService{opts: AdminOptions{Settings: fake}}
	get := func() config.GlobalClientSettings {
		t.Helper()
		w, r := apiContext(http.MethodGet, "/admin/ops/api/client-config", nil, nil)
		s.apiGlobalClientConfig(w, r)
		var out struct {
			Data struct {
				GlobalClient config.GlobalClientSettings `json:"global_client"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Data.GlobalClient.CustomHosts == nil || out.Data.GlobalClient.IPMaps == nil {
			t.Fatalf("empty form must contain arrays: %+v", out.Data.GlobalClient)
		}
		return out.Data.GlobalClient
	}
	get()
	save := func(global config.GlobalClientSettings, status int) {
		t.Helper()
		body := map[string]any{"global_client": apiGlobalClient{
			CustomHosts: global.CustomHosts,
			IPMaps:      global.IPMaps,
		}}
		w, r := apiContext(http.MethodPut, "/admin/ops/api/client-config", body, nil)
		s.apiGlobalClientConfigSave(w, r)
		if w.Code != status {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}
	save(config.GlobalClientSettings{
		CustomHosts: []config.CustomHost{{Domain: " YouYueVPN4. ", IP: " 192.168.60.1 "}, {}},
		IPMaps:      []config.IPMap{{From: " 186.12.12.* ", To: "192.168.60.*"}, {}},
	}, http.StatusOK)
	want := config.GlobalClientSettings{CustomHosts: []config.CustomHost{{Domain: "youyuevpn4", IP: "192.168.60.1"}}, IPMaps: []config.IPMap{{From: "186.12.12.*", To: "192.168.60.*"}}}
	if got := get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("normalization: %+v", got)
	}
	fake.applied = false
	save(config.GlobalClientSettings{IPMaps: []config.IPMap{{From: "186.12.12.*", To: "300.1.1.*"}}}, http.StatusBadRequest)
	if fake.applied || !reflect.DeepEqual(fake.cur.GlobalClient, want) {
		t.Fatal("invalid input changed settings")
	}
	save(config.GlobalClientSettings{}, http.StatusOK)
	if got := get(); len(got.CustomHosts) != 0 || len(got.IPMaps) != 0 {
		t.Fatalf("clear failed: %+v", got)
	}
}

// TestServiceConfigKeepsGlobalClient 断言服务配置全量保存不会动全局客户端配置。
func TestServiceConfigKeepsGlobalClient(t *testing.T) {
	want := config.GlobalClientSettings{CustomHosts: []config.CustomHost{{Domain: "vpn.route", IP: "10.0.0.1"}}}
	cur := config.DefaultServiceSettings()
	cur.GlobalClient = want
	fake := &fakeSettings{cur: cur}
	s := &AdminService{opts: AdminOptions{Settings: fake}}

	w, r := apiContext(http.MethodPut, "/admin/ops/api/service-config", apiDurationFields{LogLevel: "warn"}, nil)
	s.apiServiceConfigSave(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(fake.cur.GlobalClient, want) {
		t.Fatalf("service config save touched global client: %+v", fake.cur.GlobalClient)
	}
}
