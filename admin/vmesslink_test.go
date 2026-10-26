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
	link := vmessLink(shareTarget{Host: "vpn.example.com", Port: 8443}, uuid, shareName(uuid))
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
	link := vlessLink(shareTarget{Host: "vpn.example.com", Port: 8443}, uuid, shareName(uuid))
	if !strings.HasPrefix(link, "vless://"+uuid+"@vpn.example.com:8443?") {
		t.Fatalf("link = %q, want a vless://<uuid>@host:port? prefix", link)
	}
	for _, want := range []string{"encryption=none", "type=tcp", "security=none", "#vnray-b831381d"} {
		if !strings.Contains(link, want) {
			t.Fatalf("link %q missing %q", link, want)
		}
	}
}

// TestVlessLinkTLS 断言 TLS 链接带齐客户端要用的参数：自签证书必须带证书指纹，
// 且不能再出现 allowInsecure——新版 Xray 见到这个字段会直接拒绝启动。
func TestVlessLinkTLS(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	const fp = "e8e2d387fdbffeb38e9c9065cf30a97ee23c0e3d32ee6f78ffae40966befccc9"
	target := shareTarget{
		Host: "vpn.example.com", Port: 8443,
		TLS: &shareTLS{ServerName: "node.example.com", SelfSigned: true, Fingerprint: fp},
	}
	link := vlessLink(target, uuid, shareName(uuid))
	for _, want := range []string{
		"security=tls", "sni=node.example.com", "fp=chrome",
		"alpn=h2%2Chttp%2F1.1", "pcs=" + fp, "encryption=none",
	} {
		if !strings.Contains(link, want) {
			t.Fatalf("link %q missing %q", link, want)
		}
	}
	if strings.Contains(link, "allowInsecure") {
		t.Fatalf("tls link %q still carries the removed allowInsecure", link)
	}
	if strings.Contains(link, "security=none") {
		t.Fatalf("tls link %q still carries security=none", link)
	}
}

// TestVlessLinkTLSFingerprintCase 断言指纹统一成小写：Xray 大小写不敏感，但链接
// 需要稳定，避免同一张证书生成两种链接。
func TestVlessLinkTLSFingerprintCase(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	link := vlessLink(shareTarget{
		Host: "vpn.example.com", Port: 8443,
		TLS: &shareTLS{SelfSigned: true, Fingerprint: "E8E2D387FDBFFEB38E9C9065CF30A97EE23C0E3D32EE6F78FFAE40966BEFCCC9"},
	}, uuid, shareName(uuid))
	if !strings.Contains(link, "pcs=e8e2d387fdbffeb38e9c9065cf30a97ee23c0e3d32ee6f78ffae40966befccc9") {
		t.Fatalf("link %q did not lowercase the fingerprint", link)
	}
}

// TestVlessLinkVariantsAreMutuallyExclusive 断言两代链接各自只带一种校验参数。
// 这是硬约束：为了"兼容"把 allowInsecure 和 pcs 塞进同一条链接，会让新内核拒绝
// 启动整个进程，而客户端所有节点共用一份配置，等于全部节点一起失效。
func TestVlessLinkVariantsAreMutuallyExclusive(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	const fp = "e8e2d387fdbffeb38e9c9065cf30a97ee23c0e3d32ee6f78ffae40966befccc9"
	target := shareTarget{
		Host: "vpn.example.com", Port: 8443,
		TLS: &shareTLS{ServerName: "node.example.com", SelfSigned: true, Fingerprint: fp},
	}
	for _, tt := range []struct {
		name     string
		link     string
		want     string
		unwanted string
	}{
		{"新版", vlessLink(target, uuid, shareName(uuid)), "pcs=" + fp, "allowInsecure"},
		{"旧版", vlessLinkLegacy(target, uuid, shareName(uuid)), "allowInsecure=1", "pcs="},
	} {
		if !strings.Contains(tt.link, tt.want) {
			t.Fatalf("%s链接 %q 缺少 %q", tt.name, tt.link, tt.want)
		}
		if strings.Contains(tt.link, tt.unwanted) {
			t.Fatalf("%s链接 %q 不应包含 %q", tt.name, tt.link, tt.unwanted)
		}
	}
}

// TestVlessLinkLegacyKeepsTransportParams 断言旧版链接除校验参数外与新版一致：
// sni/fp/alpn 都不能少，否则旧客户端同样连不上。
func TestVlessLinkLegacyKeepsTransportParams(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	const fp = "e8e2d387fdbffeb38e9c9065cf30a97ee23c0e3d32ee6f78ffae40966befccc9"
	link := vlessLinkLegacy(shareTarget{
		Host: "vpn.example.com", Port: 8443,
		TLS: &shareTLS{ServerName: "node.example.com", SelfSigned: true, Fingerprint: fp},
	}, uuid, shareName(uuid))
	for _, want := range []string{
		"security=tls", "sni=node.example.com", "fp=chrome",
		"alpn=h2%2Chttp%2F1.1", "encryption=none",
	} {
		if !strings.Contains(link, want) {
			t.Fatalf("旧版链接 %q 缺少 %q", link, want)
		}
	}
}

// TestVlessLinkTLSNoFingerprint 断言拿不到指纹时不写任何校验参数：写 allowInsecure
// 会让内核起不来，比握手失败更难排查。
func TestVlessLinkTLSNoFingerprint(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	link := vlessLink(shareTarget{
		Host: "vpn.example.com", Port: 8443, TLS: &shareTLS{SelfSigned: true},
	}, uuid, shareName(uuid))
	for _, unwanted := range []string{"allowInsecure", "pcs=", "insecure="} {
		if strings.Contains(link, unwanted) {
			t.Fatalf("link %q should not contain %q", link, unwanted)
		}
	}
}

// TestVlessLinkTLSWithoutServerName 没配域名时 sni 退回分享主机，避免空 sni。
func TestVlessLinkTLSWithoutServerName(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	link := vlessLink(shareTarget{
		Host: "1.2.3.4", Port: 8443, TLS: &shareTLS{SelfSigned: true},
	}, uuid, shareName(uuid))
	if !strings.Contains(link, "sni=1.2.3.4") {
		t.Fatalf("link %q missing sni fallback", link)
	}
}

// TestLinkForPort 断言分享链接按协议选择，且端口为 0（插件未配该协议）时不出链接。
func TestLinkForPort(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	if got := linkForPort(shareTarget{Host: "vpn.example.com"}, uuid, true); got != "" {
		t.Fatalf("port 0 link = %q, want empty", got)
	}
	if got := linkForPort(shareTarget{Host: "vpn.example.com", Port: 8443}, uuid, true); !strings.HasPrefix(got, "vmess://") {
		t.Fatalf("vmess link = %q, want a vmess:// prefix", got)
	}
	if got := linkForPort(shareTarget{Host: "vpn.example.com", Port: 8443}, uuid, false); !strings.HasPrefix(got, "vless://") {
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
