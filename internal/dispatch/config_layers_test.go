package dispatch

import (
	"sync"
	"testing"

	"github.com/hhy50/vnray/internal/protocol"
)

func TestIPMapLayers(t *testing.T) {
	global := []protocol.IPMap{{From: "192.169.30.*", To: "10.60.0.*"}}
	for _, tt := range []struct {
		name        string
		local       []protocol.IPMap
		global      []protocol.IPMap
		input, want string
	}{
		{"local exact beats global wildcard", []protocol.IPMap{{From: "192.169.30.12", To: "10.30.0.12"}}, global, "192.169.30.12", "10.30.0.12"},
		{"local miss falls back", []protocol.IPMap{{From: "192.169.30.12", To: "10.30.0.12"}}, global, "192.169.30.13", "10.60.0.13"},
		{"local wildcard beats global exact", []protocol.IPMap{{From: "192.169.30.*", To: "10.30.0.*"}}, []protocol.IPMap{{From: "192.169.30.12", To: "10.60.0.12"}}, "192.169.30.12", "10.30.0.12"},
		{"identity match stops fallback", []protocol.IPMap{{From: "192.169.30.12", To: "192.169.30.12"}}, global, "192.169.30.12", "192.169.30.12"},
		{"no second rewrite", []protocol.IPMap{{From: "192.169.30.12", To: "192.169.30.13"}}, global, "192.169.30.12", "192.169.30.13"},
		{"empty local", nil, global, "192.169.30.12", "10.60.0.12"},
		{"first rule in layer", []protocol.IPMap{{From: "192.169.30.*", To: "10.30.0.*"}, {From: "192.169.30.12", To: "10.99.0.12"}}, global, "192.169.30.12", "10.30.0.12"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := NewIPMaps()
			m.UpdateLayers(tt.local, tt.global)
			if got, ok := m.Rewrite(tt.input); !ok || got != tt.want {
				t.Fatalf("Rewrite(%s) = %s, %v; want %s", tt.input, got, ok, tt.want)
			}
			o := NewDirectOutbound("direct")
			o.SetIPMaps(m)
			if got := o.resolveAddress(tt.input + ":443"); got != tt.want+":443" {
				t.Fatalf("dial target = %s", got)
			}
			m.UpdateLayers(nil, nil)
			if _, ok := m.Rewrite(tt.input); ok {
				t.Fatal("cleared rule still matches")
			}
		})
	}
}

func TestHostLayers(t *testing.T) {
	h := NewHosts()
	local := []protocol.CustomHost{{Domain: "VPN.route.", IP: "10.30.0.12"}}
	global := []protocol.CustomHost{{Domain: "vpn.route", IP: "10.60.0.12"}, {Domain: "global.route", IP: "10.60.0.13"}}
	h.UpdateLayers(local, global)
	for _, tt := range []struct{ domain, want string }{{"vpn.ROUTE", "10.30.0.12"}, {"global.route", "10.60.0.13"}} {
		if got, ok := h.LookupHost(tt.domain); !ok || got != tt.want {
			t.Fatalf("LookupHost(%s) = %s, %v", tt.domain, got, ok)
		}
	}
	h.UpdateLayers(nil, global)
	if got, _ := h.LookupHost("vpn.route"); got != "10.60.0.12" {
		t.Fatalf("local clear did not restore fallback: %s", got)
	}
	h.UpdateLayers(local, nil)
	if got, _ := h.LookupHost("vpn.route"); got != "10.30.0.12" {
		t.Fatalf("global clear changed local: %s", got)
	}
	if _, ok := h.LookupHost("global.route"); ok {
		t.Fatal("global clear retained fallback")
	}
	h.UpdateLayers(nil, nil)
	if _, ok := h.LookupHost("vpn.route"); ok {
		t.Fatal("clear retained host")
	}
}

// TestLayersCrossTableNoSecondRewrite 锁定跨表语义：域名表命中的结果（哪怕是同网段的
// 内网地址）不会再被 IP 段映射改写一次，避免出现 A→B→C 的链式改写。
func TestLayersCrossTableNoSecondRewrite(t *testing.T) {
	h, m := NewHosts(), NewIPMaps()
	h.UpdateLayers(nil, []protocol.CustomHost{{Domain: "vpn.route", IP: "192.169.30.12"}})
	m.UpdateLayers(nil, []protocol.IPMap{{From: "192.169.30.*", To: "10.60.0.*"}})

	o := NewDirectOutbound("direct")
	o.SetHosts(h)
	o.SetIPMaps(m)
	if got := o.resolveAddress("vpn.route:443"); got != "192.169.30.12:443" {
		t.Fatalf("dial target = %s, want the host table result", got)
	}
	// 同一个 IP 作为字面量目标时才走段映射：分流按目标类型，不按规则表优先级。
	if got, ok := m.Rewrite("192.169.30.12"); !ok || got != "10.60.0.12" {
		t.Fatalf("Rewrite = %s, %v", got, ok)
	}
}

// TestLayersIgnoreNonIPv4 断言段映射只对 IPv4 字面量生效：域名与 IPv6 目标原样保留。
func TestLayersIgnoreNonIPv4(t *testing.T) {
	m := NewIPMaps()
	m.UpdateLayers(nil, []protocol.IPMap{{From: "*.*.*.*", To: "10.*.*.*"}})
	for _, target := range []string{"vpn.route", "::1", "2001:db8::1"} {
		if got, ok := m.Rewrite(target); ok {
			t.Fatalf("Rewrite(%s) = %s, want no match", target, got)
		}
	}
	o := NewDirectOutbound("direct")
	o.SetIPMaps(m)
	for _, target := range []string{"vpn.route:443", "[::1]:443"} {
		if got := o.resolveAddress(target); got != target {
			t.Fatalf("dial target = %s, want %s", got, target)
		}
	}
}

// TestConfigLayersConcurrentReplace 断言热更新时两张表各自的「层内原子替换」：
// 读者要么看到完整旧规则，要么看到完整新规则，不会看到"层被清空"的中间态。
func TestConfigLayersConcurrentReplace(t *testing.T) {
	h, m := NewHosts(), NewIPMaps()
	localHosts := []protocol.CustomHost{{Domain: "vpn.route", IP: "10.30.0.12"}}
	globalHosts := []protocol.CustomHost{{Domain: "vpn.route", IP: "10.60.0.12"}}
	localMaps := []protocol.IPMap{{From: "192.169.30.12", To: "10.30.0.12"}}
	globalMaps := []protocol.IPMap{{From: "192.169.30.*", To: "10.60.0.*"}}
	h.UpdateLayers(localHosts, nil)
	m.UpdateLayers(localMaps, nil)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 1000 {
			h.UpdateLayers(nil, globalHosts)
			m.UpdateLayers(nil, globalMaps)
			h.UpdateLayers(localHosts, nil)
			m.UpdateLayers(localMaps, nil)
		}
	}()
	for range 1000 {
		if got, ok := h.LookupHost("vpn.route"); !ok || (got != "10.30.0.12" && got != "10.60.0.12") {
			t.Errorf("partial host snapshot: %s, %v", got, ok)
			break
		}
		if got, ok := m.Rewrite("192.169.30.12"); !ok || (got != "10.30.0.12" && got != "10.60.0.12") {
			t.Errorf("partial mapping snapshot: %s, %v", got, ok)
			break
		}
	}
	wg.Wait()
}
