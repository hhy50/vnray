package dispatch

import (
	"testing"

	"github.com/hhy50/vnray/internal/protocol"
)

func TestHostsUpdateLookup(t *testing.T) {
	h := NewHosts()
	h.Update([]protocol.CustomHost{
		{Domain: "Example.COM.", IP: "10.0.0.1"},
		{Domain: "api.internal", IP: "192.168.1.2"},
	})
	for _, tc := range []struct {
		domain string
		wantIP string
		wantOK bool
	}{
		{"example.com", "10.0.0.1", true},
		{"EXAMPLE.com", "10.0.0.1", true},
		{"example.com.", "10.0.0.1", true},
		{"api.internal", "192.168.1.2", true},
		{"other.com", "", false},
		{"", "", false},
	} {
		got, ok := h.LookupHost(tc.domain)
		if got != tc.wantIP || ok != tc.wantOK {
			t.Fatalf("LookupHost(%q) = %q, %v; want %q, %v", tc.domain, got, ok, tc.wantIP, tc.wantOK)
		}
	}
	// Update(nil) 清空整表：等价于"全部走 DNS"。
	h.Update(nil)
	if ip, ok := h.LookupHost("example.com"); ok {
		t.Fatalf("hosts not cleared: %q -> %q", "example.com", ip)
	}
}

func TestDirectOutboundResolveAddress(t *testing.T) {
	h := NewHosts()
	h.Update([]protocol.CustomHost{{Domain: "mirror.local", IP: "10.0.0.8"}})
	o := NewDirectOutbound("direct")
	o.SetHosts(h)
	for _, tc := range []struct {
		address string
		want    string
	}{
		{"mirror.local:8080", "10.0.0.8:8080"},
		{"example.com:443", "example.com:443"}, // 未命中走 DNS
		{"127.0.0.1:9000", "127.0.0.1:9000"},   // IP 字面量不受影响
	} {
		if got := o.resolveAddress(tc.address); got != tc.want {
			t.Fatalf("resolveAddress(%q) = %q, want %q", tc.address, got, tc.want)
		}
	}
	// SetHosts(nil) 停用查表。
	o.SetHosts(nil)
	if got := o.resolveAddress("mirror.local:8080"); got != "mirror.local:8080" {
		t.Fatalf("hosts not disabled: %q", got)
	}
}
