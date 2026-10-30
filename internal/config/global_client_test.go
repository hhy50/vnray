package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestGlobalClientValidate(t *testing.T) {
	for _, tt := range []struct {
		name  string
		cfg   GlobalClientSettings
		valid bool
	}{
		{"empty", GlobalClientSettings{}, true},
		{"single label", GlobalClientSettings{CustomHosts: []CustomHost{{"youyuevpn4", "192.168.60.1"}}}, true},
		{"ipv6 host", GlobalClientSettings{CustomHosts: []CustomHost{{"vpn.route", "::1"}}}, true},
		// 管理台允许带尾部点录入，Validate 自己会把它归一化后再判定。
		{"trailing dot", GlobalClientSettings{CustomHosts: []CustomHost{{"vpn.route.", "192.168.60.1"}}}, true},
		// 单段域名（内网短名）合法，不能要求必须带点。
		{"wildcards", GlobalClientSettings{IPMaps: []IPMap{{"186.12.12.*", "192.168.60.*"}}}, true},
		{"all wildcards", GlobalClientSettings{IPMaps: []IPMap{{"*.*.*.*", "*.*.*.*"}}}, true},
		{"invalid host", GlobalClientSettings{CustomHosts: []CustomHost{{"https://vpn", "127.0.0.1"}}}, false},
		{"host with port", GlobalClientSettings{CustomHosts: []CustomHost{{"vpn:443", "127.0.0.1"}}}, false},
		{"ip literal as host", GlobalClientSettings{CustomHosts: []CustomHost{{"::1", "127.0.0.1"}}}, false},
		{"empty host", GlobalClientSettings{CustomHosts: []CustomHost{{"", "10.0.0.1"}}}, false},
		{"host too long", GlobalClientSettings{CustomHosts: []CustomHost{{strings.Repeat("a", 254), "10.0.0.1"}}}, false},
		{"invalid ip", GlobalClientSettings{CustomHosts: []CustomHost{{"vpn", "999.0.0.1"}}}, false},
		{"ip with port", GlobalClientSettings{CustomHosts: []CustomHost{{"vpn", "192.168.60.1:80"}}}, false},
		{"missing destination", GlobalClientSettings{IPMaps: []IPMap{{"186.12.12.*", ""}}}, false},
		{"invalid segment", GlobalClientSettings{IPMaps: []IPMap{{"186.12.256.*", "192.168.60.*"}}}, false},
		{"too few segments", GlobalClientSettings{IPMaps: []IPMap{{"186.12.*", "192.168.60.*"}}}, false},
		{"too many segments", GlobalClientSettings{IPMaps: []IPMap{{"186.12.12.1.*", "192.168.60.*"}}}, false},
		{"empty pattern", GlobalClientSettings{IPMaps: []IPMap{{"", ""}}}, false},
		{"negative segment", GlobalClientSettings{IPMaps: []IPMap{{"186.12.12.-1", "192.168.60.*"}}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); (err == nil) != tt.valid {
				t.Fatalf("Validate() = %v, valid = %v", err, tt.valid)
			}
		})
	}
}

// TestGlobalClientClone 断言 Clone 是深拷贝：运行态快照不能被管理台编辑改掉，
// 空表克隆后仍为 nil（避免序列化出多余的空数组）。
func TestGlobalClientClone(t *testing.T) {
	src := GlobalClientSettings{
		CustomHosts: []CustomHost{{Domain: "vpn.route", IP: "192.168.60.1"}},
		IPMaps:      []IPMap{{From: "186.12.12.*", To: "192.168.60.*"}},
	}
	cp := src.Clone()
	cp.CustomHosts[0].IP = "127.0.0.1"
	cp.IPMaps[0].To = "127.0.0.*"
	if src.CustomHosts[0].IP != "192.168.60.1" || src.IPMaps[0].To != "192.168.60.*" {
		t.Fatalf("clone shares slices with source: %+v", src)
	}

	empty := GlobalClientSettings{}.Clone()
	if empty.CustomHosts != nil || empty.IPMaps != nil {
		t.Fatalf("empty clone = %+v, want nil slices", empty)
	}
	if !reflect.DeepEqual(empty, GlobalClientSettings{}) {
		t.Fatalf("empty clone = %+v, want zero value", empty)
	}
}
