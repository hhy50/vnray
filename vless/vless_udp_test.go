package vless

// VLESS UDP（命令 0x02）的全链路测试：头之后是 [2B 长度][数据报]* 的明文封帧，按数据报
// 回显往返。

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/identity"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/vmess"
)

// startUDPTestServer 起一个按数据报回显的 VLESS 入站（network==udp 时用长度前缀封帧）。
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
	s := NewServer(v, func(id identity.Identity, target Target, network string, conn net.Conn) {
		if network != "udp" {
			t.Errorf("network = %s, want udp, target=%s", network, target)
		}
		dc := protocol.NewDatagramConn(conn)
		for {
			pkt, rerr := dc.ReadPacket()
			if len(pkt) > 0 {
				if werr := dc.WritePacket(pkt); werr != nil {
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

// buildUDPRequest 拼 UDP 请求头：命令 0x02 + 端口 + 地址。
func buildUDPRequest(raw [16]byte, host string, port int) []byte {
	return buildRequest(raw, CommandUDP, host, port, 0)
}

// TestVLESSCommandUDPRoundtrip 多个数据报经 [2B len] 封帧往返回显。
func TestVLESSCommandUDPRoundtrip(t *testing.T) {
	addr := startUDPTestServer(t)
	raw, _ := vmess.ParseUUID(testUUID)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(buildUDPRequest(raw, "example.com", 53)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var resp [2]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		t.Fatalf("read response header: %v", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	dc := protocol.NewDatagramConn(conn)

	payloads := [][]byte{[]byte("first"), []byte("second datagram"), []byte("third")}
	for _, p := range payloads {
		if err := dc.WritePacket(p); err != nil {
			t.Fatalf("write datagram: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		got, err := dc.ReadPacket()
		if err != nil {
			t.Fatalf("read echo for %q: %v", p, err)
		}
		if string(got) != string(p) {
			t.Fatalf("echo = %q, want %q", got, p)
		}
	}
}
