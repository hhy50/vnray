package config

import (
	"net"
	"testing"
)

func TestParseNetworkPatterns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		want    []string // 解析后期望的 CIDR 字符串
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"whitespace only", " , ", nil, false},
		{"cidr", "192.168.31.0/24", []string{"192.168.31.0/24"}, false},
		{"wildcard /24", "192.168.31.*", []string{"192.168.31.0/24"}, false},
		{"wildcard /16", "192.168.*.*", []string{"192.168.0.0/16"}, false},
		{"wildcard /8", "192.*.*.*", []string{"192.0.0.0/8"}, false},
		{"bare ipv4", "127.0.0.1", []string{"127.0.0.1/32"}, false},
		{"bare ipv6", "::1", []string{"::1/128"}, false},
		{"list", "192.168.31.*, 192.168.32.0/24", []string{"192.168.31.0/24", "192.168.32.0/24"}, false},
		{"bad cidr", "192.168.31.0/99", nil, true},
		{"bad octet", "192.168.31x.*", nil, true},
		{"middle star", "192.*.31.*", nil, true},
		{"not ip", "not-a-pattern", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseNetworkPatterns(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d nets %v, want %d", len(got), got, len(tc.want))
			}
			for i := range got {
				if got[i].String() != tc.want[i] {
					t.Fatalf("net[%d] = %s, want %s", i, got[i].String(), tc.want[i])
				}
			}
		})
	}
}

func TestNetworkContains(t *testing.T) {
	nets, err := ParseNetworkPatterns("192.168.31.*,10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	contains := func(ip string) bool {
		parsed := net.ParseIP(ip)
		for i := range nets {
			if nets[i].Contains(parsed) {
				return true
			}
		}
		return false
	}
	if !contains("192.168.31.5") || !contains("10.2.3.4") {
		t.Fatal("expected membership for in-range IPs")
	}
	if contains("192.168.32.1") || contains("172.16.0.1") {
		t.Fatal("expected non-membership for out-of-range IPs")
	}
}
