package admin

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVMessLink(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	link := vmessLink("vpn.example.com", 8443, uuid, shareName(uuid))
	if !strings.HasPrefix(link, "vmess://") {
		t.Fatalf("link = %q, want a vmess:// prefix", link)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(link, "vmess://"))
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var got vmessShare
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	want := vmessShare{
		V: "2", PS: "vnray-b831381d", Add: "vpn.example.com", Port: "8443",
		ID: uuid, Aid: "0", Scy: "auto", Net: "tcp", Type: "none",
	}
	if got != want {
		t.Fatalf("payload = %+v, want %+v", got, want)
	}
}

// TestVlessLink 断言 vless:// 链接的形态：uuid@host:port 加固定查询串与节点名。
func TestVlessLink(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	link := vlessLink("vpn.example.com", 8443, uuid, shareName(uuid))
	if !strings.HasPrefix(link, "vless://"+uuid+"@vpn.example.com:8443?") {
		t.Fatalf("link = %q, want a vless://<uuid>@host:port? prefix", link)
	}
	for _, want := range []string{"encryption=none", "type=tcp", "security=none", "#vnray-b831381d"} {
		if !strings.Contains(link, want) {
			t.Fatalf("link %q missing %q", link, want)
		}
	}
}

// TestLinkForPort 断言分享链接按协议选择，且端口为 0（插件未配该协议）时不出链接。
func TestLinkForPort(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	if got := linkForPort(0, "vpn.example.com", uuid, true); got != "" {
		t.Fatalf("port 0 link = %q, want empty", got)
	}
	if got := linkForPort(8443, "vpn.example.com", uuid, true); !strings.HasPrefix(got, "vmess://") {
		t.Fatalf("vmess link = %q, want a vmess:// prefix", got)
	}
	if got := linkForPort(8443, "vpn.example.com", uuid, false); !strings.HasPrefix(got, "vless://") {
		t.Fatalf("vless link = %q, want a vless:// prefix", got)
	}
}

func TestBareHost(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"", ""},
		{"  vpn.example.com  ", "vpn.example.com"},
		{"https://vpn.example.com:8443/path", "vpn.example.com"},
		{"1.2.3.4:9000", "1.2.3.4"},
		{"[::1]:8080", "::1"},
		{"::1", "::1"},
	} {
		if got := bareHost(tt.in); got != tt.want {
			t.Fatalf("bareHost(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestShareHost 覆盖两种来源：配了 public_host 就用它（管理台只监听回环地址，
// 请求 Host 会退化成 127.0.0.1）；没配则退回请求 Host 的主机部分。
func TestShareHost(t *testing.T) {
	r := httptest.NewRequest("GET", "http://127.0.0.1:8080/admin/ops/_users", nil)
	if got := (&AdminService{opts: AdminOptions{PublicHost: "vpn.example.com"}}).shareHost(r); got != "vpn.example.com" {
		t.Fatalf("shareHost with public_host = %q, want vpn.example.com", got)
	}
	if got := (&AdminService{}).shareHost(r); got != "127.0.0.1" {
		t.Fatalf("shareHost without public_host = %q, want 127.0.0.1", got)
	}
}
