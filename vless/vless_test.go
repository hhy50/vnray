package vless

// 单元测试：按协议规范逐字节构造请求头，与服务端解析互为独立验证；
// 覆盖 TCP 直达、mux 目标合成，以及版本/凭据/addons/命令四类握手拒绝。

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/hhy50/vnray/vmess"
)

const testUUID = "b831381d-6324-4d53-ad4f-8cda48b30811"

// encodeAddr 按规范编码地址段（IPv4 字面量 / 域名）。
func encodeAddr(host string) []byte {
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return append([]byte{addrTypeIPv4}, v4...)
		}
		return append([]byte{addrTypeIPv6}, ip.To16()...)
	}
	return append([]byte{addrTypeDomain, byte(len(host))}, host...)
}

// buildRequest 按协议拼一个请求头；command 为 Mux 时不带地址段。
func buildRequest(raw [16]byte, command byte, host string, port int, addonsLen byte) []byte {
	buf := []byte{0x00}
	buf = append(buf, raw[:]...)
	buf = append(buf, addonsLen, command)
	if command == CommandTCP || command == CommandUDP {
		buf = append(buf, byte(port>>8), byte(port))
		buf = append(buf, encodeAddr(host)...)
	}
	return buf
}

// startTestServer 起一个注册了 testUUID 的服务，handler 收到的目标与载荷经
// channel 交还给测试。
func startTestServer(t *testing.T, handler Handler) string {
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
	s := NewServer(v, handler)
	go func() { _ = s.Serve(ln) }()
	return ln.Addr().String()
}

// dialRequest 发送请求头并等待响应头，返回连接（含已读走的 2B 响应头报告）。
func dialRequest(t *testing.T, addr string, payload []byte) (net.Conn, [2]byte, error) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var resp [2]byte
	_, err = io.ReadFull(conn, resp[:])
	return conn, resp, err
}

// TestServerTCPRoundtrip 走 TCP 命令全路径：目标解析、响应头、载荷透传回显。
func TestServerTCPRoundtrip(t *testing.T) {
	requests := make(chan Target, 1)
	addr := startTestServer(t, func(user, client string, target Target, conn net.Conn) {
		if user != testUUID || client != "client-a" {
			t.Errorf("auth = %s/%s, want %s/client-a", user, client, testUUID)
		}
		requests <- target
		_, _ = io.Copy(conn, conn)
		_ = conn.Close()
	})

	raw, _ := vmess.ParseUUID(testUUID)
	conn, resp, err := dialRequest(t, addr, append(buildRequest(raw, CommandTCP, "example.com", 443, 0), []byte("ping")...))
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if resp != [2]byte{0x00, 0x00} {
		t.Fatalf("response header = %v, want [0 0]", resp)
	}

	select {
	case got := <-requests:
		if got.Host != "example.com" || got.Port != 443 {
			t.Fatalf("target = %s, want example.com:443", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never invoked")
	}

	// 载荷透传：先发出去的 ping 应被回显（响应头之后是明文载荷）。
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q, want ping", got)
	}
}

// TestServerMuxTarget 合成 mux 标记目标：命令 3 不带地址段，目标为 v1.mux.cool。
func TestServerMuxTarget(t *testing.T) {
	requests := make(chan Target, 1)
	addr := startTestServer(t, func(user, client string, target Target, conn net.Conn) {
		requests <- target
		_ = conn.Close()
	})

	raw, _ := vmess.ParseUUID(testUUID)
	if _, _, err := dialRequest(t, addr, buildRequest(raw, CommandMux, "", 0, 0)); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	select {
	case got := <-requests:
		if got.Host != "v1.mux.cool" || got.Port != 0 {
			t.Fatalf("target = %s, want v1.mux.cool:0", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never invoked")
	}
}

// TestServerRejects 覆盖四类握手拒绝：未知 uuid、坏版本、非空 addons、UDP 命令。
// 拒绝的表现一致：不写响应头、连接关闭。
func TestServerRejects(t *testing.T) {
	addr := startTestServer(t, func(user, client string, target Target, conn net.Conn) {
		t.Error("handler should not be invoked for rejected requests")
		_ = conn.Close()
	})

	raw, _ := vmess.ParseUUID(testUUID)
	unknown := raw
	unknown[0] ^= 0xFF

	cases := []struct {
		name string
		req  []byte
	}{
		{"unknown uuid", buildRequest(unknown, CommandTCP, "example.com", 443, 0)},
		{"bad version", append([]byte{0x01}, buildRequest(raw, CommandTCP, "example.com", 443, 0)[1:]...)},
		{"flow addons", buildRequest(raw, CommandTCP, "example.com", 443, 1)},
		{"udp command", buildRequest(raw, CommandUDP, "example.com", 443, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, resp, err := dialRequest(t, addr, tc.req)
			if err == nil && resp != [2]byte{0x00, 0x00} {
				t.Fatalf("rejected request got response header %v", resp)
			}
			_ = conn
		})
	}
}
