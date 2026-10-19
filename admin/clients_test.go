package admin

import (
	"testing"

	"github.com/hhy50/vnray/internal/protocol"
)

func TestIfaceSummary(t *testing.T) {
	for _, tt := range []struct {
		name       string
		in         []protocol.NetInterface
		wantText   string
		wantDetail string
	}{
		{"no interfaces", nil, "—", ""},
		{
			"ipv4 first",
			[]protocol.NetInterface{{Name: "eth0", Addrs: []string{"192.168.1.10/24", "fe80::1/64"}}},
			"eth0 192.168.1.10", "eth0 192.168.1.10/24 fe80::1/64",
		},
		{
			"ipv6 only falls back",
			[]protocol.NetInterface{{Name: "tun0", Addrs: []string{"fd00::2/64"}}},
			"tun0 fd00::2/64", "tun0 fd00::2/64",
		},
		{
			"no address",
			[]protocol.NetInterface{{Name: "docker0"}},
			"docker0", "docker0",
		},
		{
			"picks the first interface carrying ipv4",
			[]protocol.NetInterface{
				{Name: "tun0", Addrs: []string{"fd00::2/64"}},
				{Name: "eth0", Addrs: []string{"10.0.0.5/8"}},
			},
			"eth0 10.0.0.5", "tun0 fd00::2/64 · eth0 10.0.0.5/8",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			text, detail := ifaceSummary(tt.in)
			if text != tt.wantText || detail != tt.wantDetail {
				t.Fatalf("ifaceSummary = (%q, %q), want (%q, %q)", text, detail, tt.wantText, tt.wantDetail)
			}
		})
	}
}

func TestClientRowOfOnline(t *testing.T) {
	row := (&AdminService{}).clientRowOf(ClientInfo{
		ID:     7,
		Remote: "203.0.113.9:51423",
		Client: protocol.ClientInfo{
			Name: "edge-1", Version: "v0.1", OS: "linux", Arch: "amd64",
			Interfaces: []protocol.NetInterface{{Name: "eth0", Addrs: []string{"192.168.1.10/24"}}},
		},
	})
	if !row.Online {
		t.Fatal("online session must render as online")
	}
	if row.Iface != "eth0 192.168.1.10" {
		t.Fatalf("iface = %q, want eth0 192.168.1.10", row.Iface)
	}
	if row.ConfigURL != "/admin/ops/_client-config?name=edge-1" {
		t.Fatalf("config url = %q", row.ConfigURL)
	}
}
