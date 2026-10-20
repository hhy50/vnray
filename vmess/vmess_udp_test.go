package vmess

// VMess UDP（命令 0x02）的全链路测试：客户端构造请求头 → 入站解析 command=UDP → 载荷
// chunk 按数据报读写往返。

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/identity"
)

// startUDPTestServer 起一个按数据报回显的 VMess 入站（network==udp 时用 ReadPacket/
// WritePacket，单 chunk = 单数据报）。
func startUDPTestServer(t *testing.T) string {
	t.Helper()
	v := NewValidator()
	if err := v.Add(testUUID, "client-a"); err != nil {
		t.Fatalf("add user: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := NewServer(v, func(id identity.Identity, target Target, network string, conn *Conn) {
		if network != "udp" {
			t.Errorf("network = %s, want udp, target=%s", network, target)
		}
		for {
			pkt, rerr := conn.ReadPacket()
			if len(pkt) > 0 {
				if werr := conn.WritePacket(pkt); werr != nil {
					break
				}
			}
			if rerr != nil {
				break
			}
		}
		_ = conn.Close()
	})
	go func() { _ = s.Serve(ln) }()
	return ln.Addr().String()
}

// TestVMessUDPRoundtrip 覆盖多种安全/选项组合的 UDP 单数据报回显。
func TestVMessUDPRoundtrip(t *testing.T) {
	addr := startUDPTestServer(t)
	cases := []struct {
		name     string
		security byte
		option   byte
	}{
		{"gcm+mask", SecurityAES128GCM, OptionChunkStream | OptionChunkMasking},
		{"chacha+mask", SecurityChaCha20Poly1305, OptionChunkStream | OptionChunkMasking},
		{"none+chunk", SecurityNone, OptionChunkStream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()

			sess, err := newClientSession(testUUID, conn,
				Target{Host: "example.com", Port: 53}, RequestCommandUDP, tc.security, tc.option)
			if err != nil {
				t.Fatalf("new udp client session: %v", err)
			}
			if err := sess.ReadResponseHeader(); err != nil {
				t.Fatalf("read response header: %v", err)
			}
			datagram := []byte("dns-packet-udp-vmess")
			if _, err := sess.BodyWriter().Write(datagram); err != nil {
				t.Fatalf("write datagram: %v", err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 64)
			n, err := sess.BodyReader().Read(buf)
			if err != nil {
				t.Fatalf("read echo: %v", err)
			}
			if !bytes.Equal(buf[:n], datagram) {
				t.Fatalf("echo = %q, want %q", buf[:n], datagram)
			}
		})
	}
}
