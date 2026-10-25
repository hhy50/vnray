package vless_test

// vless 自签 TLS：证书 PEM 的签发/复用/重签语义，以及 ServeTLS 端到端（真握手 + VLESS 握手）。

import (
	"crypto/tls"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/identity"
	"github.com/hhy50/vnray/vless"
)

// TestLoadCert 首次调用签发证书，拿回来的 PEM 再次 Load 应原样复用。
func TestLoadCert(t *testing.T) {
	first, err := vless.Load("vpn.example.com", nil, nil)
	if err != nil {
		t.Fatalf("generate cert: %v", err)
	}
	if !first.Renewed {
		t.Fatal("first load should sign a new cert")
	}
	if first.ServerName != "vpn.example.com" || first.Fingerprint == "" {
		t.Fatalf("cert metadata incomplete: %+v", first)
	}
	if !strings.Contains(string(first.CertPEM), "BEGIN CERTIFICATE") ||
		!strings.Contains(string(first.KeyPEM), "BEGIN EC PRIVATE KEY") {
		t.Fatal("pem blocks missing")
	}
	if !first.NotAfter.After(time.Now().Add(300 * 24 * time.Hour)) {
		t.Fatalf("not_after = %v, want a long-lived cert", first.NotAfter)
	}

	second, err := vless.Load("vpn.example.com", first.CertPEM, first.KeyPEM)
	if err != nil {
		t.Fatalf("reload cert: %v", err)
	}
	if second.Renewed {
		t.Fatal("reload should reuse the stored cert")
	}
	if second.Fingerprint != first.Fingerprint {
		t.Fatal("reload changed the certificate")
	}
}

// TestLoadCertRenamesOnServerNameChange 换域名要重签，否则证书名与配置不符。
func TestLoadCertRenamesOnServerNameChange(t *testing.T) {
	first, err := vless.Load("a.example.com", nil, nil)
	if err != nil {
		t.Fatalf("generate cert: %v", err)
	}
	second, err := vless.Load("b.example.com", first.CertPEM, first.KeyPEM)
	if err != nil {
		t.Fatalf("regenerate cert: %v", err)
	}
	if !second.Renewed || second.Fingerprint == first.Fingerprint {
		t.Fatal("changing server name should re-sign the cert")
	}
	if len(second.Leaf.DNSNames) == 0 || second.Leaf.DNSNames[0] != "b.example.com" {
		t.Fatalf("dns names = %v, want b.example.com", second.Leaf.DNSNames)
	}
}

// TestLoadCertRepairsBrokenPEM 库里存了坏 PEM 时重签而不是报错，插件才不会起不来。
func TestLoadCertRepairsBrokenPEM(t *testing.T) {
	c, err := vless.Load("vpn.example.com", []byte("not a pem"), []byte("not a key"))
	if err != nil {
		t.Fatalf("load with broken pem: %v", err)
	}
	if !c.Renewed || c.Leaf == nil {
		t.Fatalf("broken pem should be replaced, got %+v", c)
	}
}

// TestServeTLS 在 TLS 监听器上跑一次完整的 VLESS 握手：客户端 tls.Dial（自签故跳过
// 校验）→ 发 VLESS 请求头 → 收到 [0,0] 响应头 → 载荷双向可达。
func TestServeTLS(t *testing.T) {
	cert, err := vless.Load("vpn.example.com", nil, nil)
	if err != nil {
		t.Fatalf("generate cert: %v", err)
	}
	validator := vless.NewValidator()
	if err := validator.Add(e2eUUID, "test-edge"); err != nil {
		t.Fatalf("register uuid: %v", err)
	}
	payload := make(chan string, 1)
	srv := vless.NewServer(validator, func(_ identity.Identity, target vless.Target, network string, conn net.Conn) {
		defer func() { _ = conn.Close() }()
		if target.Host != "example.com" || target.Port != 80 || network != "tcp" {
			payload <- "unexpected target"
			return
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			payload <- "read payload: " + err.Error()
			return
		}
		payload <- string(buf)
		_, _ = conn.Write([]byte("pong"))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.ServeTLS(ln, cert.TLSConfig()) }()

	port := ln.Addr().(*net.TCPAddr).Port
	conn, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         "vpn.example.com",
		NextProtos:         []string{"h2"},
	})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if got := conn.ConnectionState().NegotiatedProtocol; got != "h2" {
		t.Fatalf("alpn = %q, want h2", got)
	}
	if _, err := conn.Write(vlessRequest(0x01, "example.com", 80)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	readResponseHeader(t, conn)
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	select {
	case got := <-payload:
		if got != "ping" {
			t.Fatalf("handler saw %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler never received the payload")
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	reply, err := io.ReadAll(conn)
	if err != nil && len(reply) == 0 {
		t.Fatalf("read reply: %v", err)
	}
	if string(reply) != "pong" {
		t.Fatalf("reply = %q, want pong", string(reply))
	}
}
