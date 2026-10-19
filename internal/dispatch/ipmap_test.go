package dispatch

import (
	"testing"

	"github.com/hhy50/vnray/internal/protocol"
)

func TestIPMapsRewrite(t *testing.T) {
	m := NewIPMaps()
	m.Update([]protocol.IPMap{
		{From: "123.123.*.*", To: "192.168.*.*"},
		{From: "10.1.1.1", To: "10.9.9.9"},
	})
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"123.123.10.20", "192.168.10.20", true}, // 通配段取源 IP
		{"123.123.0.255", "192.168.0.255", true},
		{"10.1.1.1", "10.9.9.9", true}, // 全字面量精确命中
		{"10.1.1.2", "", false},        // 不命中
		{"123.124.10.20", "", false},
		{"example.com", "", false}, // 域名不是 IP
		{"::1", "", false},         // IPv6 不支持
	} {
		got, ok := m.Rewrite(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("Rewrite(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	// Update(nil) 清空整表。
	m.Update(nil)
	if ip, ok := m.Rewrite("123.123.10.20"); ok {
		t.Fatalf("ipmaps not cleared: %q -> %q", "123.123.10.20", ip)
	}
}

func TestValidIPMap(t *testing.T) {
	for _, tt := range []struct {
		from, to string
		invalid  bool
	}{
		{"123.123.*.*", "192.168.*.*", false},
		{"10.1.1.1", "10.9.9.9", false},
		{"1.2.3", "1.2.3.4", true},     // 段数不足
		{"1.2.3.4.5", "1.2.3.4", true}, // 段数过多
		{"1.2.3.999", "1.2.3.4", true}, // 越界
		{"1.2.3.abc", "1.2.3.4", true}, // 非数字
		{"1.2.3.*", "1.2.3.4", false},  // 合法的 from
		{"1.2.3.4", "a.2.*.*", true},   // 非法 to
	} {
		err := ValidIPMap(tt.from, tt.to)
		if (err != nil) != tt.invalid {
			t.Fatalf("ValidIPMap(%q, %q) invalid=%v, err=%v", tt.from, tt.to, tt.invalid, err)
		}
	}
}

func TestDirectOutboundIPMapRewrite(t *testing.T) {
	m := NewIPMaps()
	m.Update([]protocol.IPMap{{From: "123.123.*.*", To: "192.168.*.*"}})
	o := NewDirectOutbound("direct")
	o.SetIPMaps(m)
	for _, tc := range []struct {
		address string
		want    string
	}{
		{"123.123.1.2:80", "192.168.1.2:80"},
		{"example.com:443", "example.com:443"}, // IP 映射不碰域名，交给 host/DNS
	} {
		if got := o.resolveAddress(tc.address); got != tc.want {
			t.Fatalf("resolveAddress(%q) = %q, want %q", tc.address, got, tc.want)
		}
	}
	// SetIPMaps(nil) 停用。
	o.SetIPMaps(nil)
	if got := o.resolveAddress("123.123.1.2:80"); got != "123.123.1.2:80" {
		t.Fatalf("ipmaps not disabled: %q", got)
	}
}
