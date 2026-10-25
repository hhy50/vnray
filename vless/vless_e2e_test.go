package vless_test

// vless 端到端：vless 模拟客户端 → vless 插件入站 → uuid 路由 → 隧道 → 真实
// 客户端（vnray-client）→ direct 出站 → HTTP 源站。TCP 直达与 mux 主连接两条
// 路径都走生产装配链路；请求头与 mux 帧逐字节构造，与服务端实现互为独立验证。

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/client"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/core"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/vmess"
)

const e2eUUID = "b831381d-6324-4d53-ad4f-8cda48b30811"

// testStore 在临时目录开一个 sqlite 库，测试结束自动关闭。
func testStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestVlessPluginEndToEnd 跑 TCP 直达链路。
func TestVlessPluginEndToEnd(t *testing.T) {
	t.Parallel()
	runVlessPluginEndToEnd(t, false, false)
}

// TestVlessPluginMuxEndToEnd 跑 mux 主连接链路：一条连接复用两条子连接。
func TestVlessPluginMuxEndToEnd(t *testing.T) {
	t.Parallel()
	runVlessPluginEndToEnd(t, true, false)
}

// TestVlessPluginTLSEndToEnd 跑 TLS 链路：自签证书 + 客户端跳过校验，数据面与明文一致。
func TestVlessPluginTLSEndToEnd(t *testing.T) {
	t.Parallel()
	runVlessPluginEndToEnd(t, false, true)
}

// TestVlessPluginTLSMuxEndToEnd 是实际部署最常见的一种：TLS 上的 mux 主连接。
func TestVlessPluginTLSMuxEndToEnd(t *testing.T) {
	t.Parallel()
	runVlessPluginEndToEnd(t, true, true)
}

func runVlessPluginEndToEnd(t *testing.T, mux, useTLS bool) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello-from-origin"))
	}))
	defer origin.Close()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	if err != nil {
		t.Fatalf("parse origin addr: %v", err)
	}
	originPort, _ := strconv.Atoi(portStr)

	cfg := &config.Server{
		Server: config.ServerOptions{
			Listen: "127.0.0.1:0",
			Auth:   config.Auth{Key: "e2e-key"},
		},
	}
	sessions := transport.NewSessionManager()
	svc := core.NewConnectorServiceWith(cfg, sessions, nil, nil, tcp.WithMuxer(tcp.YamuxMuxer()))
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start data plane: %v", err)
	}
	defer func() { _ = svc.Drain(context.Background()) }()

	agent, err := client.Dial(svc.Addr(), directDispatcher(), client.DialOptions{
		Auth:  client.AuthOptions{Key: "e2e-key"},
		Info:  protocol.ClientInfo{Name: "test-edge", Version: "e2e", OS: "test", Arch: "test"},
		Muxer: tcp.YamuxMuxer(),
	})
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer agent.Close()
	go func() { _ = agent.Serve() }()

	deadline := time.Now().Add(5 * time.Second)
	for svc.Sessions().ClientCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("client session never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// v2ray 插件的 vless 入站：启用 + uuid 路由（uuid 用户写进绑定表，与生产装配同一条路径）。
	st := testStore(t)
	bindings := admin.NewBindings(st)
	plugins := plugin.NewManager(st, bindings, svc.Sessions(), nil)
	bindings.Watch(func(c admin.UserChange) {
		p, err := plugins.Get("v2ray")
		if err != nil {
			return
		}
		if syncer, ok := p.(plugin.UserSyncer); ok {
			syncer.SyncUser(c.Protocol, c.Key, c.Client, c.Status, c.Deleted)
		}
	})
	if err := plugins.Start(context.Background()); err != nil {
		t.Fatalf("start plugins: %v", err)
	}
	p, err := plugins.Get("v2ray")
	if err != nil {
		t.Fatalf("get plugin: %v", err)
	}
	vmessPort, vlessPort := freePort(t), freePort(t)
	// TLS 现在是默认开启的，明文链路必须显式关掉。
	enableParams := map[string]string{
		"vmess_port": strconv.Itoa(vmessPort),
		"vless_port": strconv.Itoa(vlessPort),
		"vless_tls":  "off",
	}
	if useTLS {
		enableParams["vless_tls"] = "on"
		enableParams["vless_tls_sni"] = "vpn.example.com"
	}
	if err := p.Enable(context.Background(), enableParams); err != nil {
		t.Fatalf("enable v2ray: %v", err)
	}
	// vless 用户以 protocol=vless 登记：该协议只进 vless 入站的验证器。
	if err := bindings.PutUser(store.UserBinding{
		Key: e2eUUID, Protocol: store.ProtocolVLESS, Name: "e2e-vless",
		Status: store.UserEnabled, Client: "test-edge",
	}); err != nil {
		t.Fatalf("register uuid: %v", err)
	}

	conn := dialVless(t, vlessPort, useTLS)
	defer func() { _ = conn.Close() }()

	if mux {
		runMuxSubConnections(t, conn, host, originPort)
		return
	}
	runSingleConnection(t, conn, host, originPort)
}

// dialVless 连接 vless 入站；useTLS 时先完成 TLS 握手（自签证书故跳过校验，
// 与客户端 allowInsecure 等价）。
func dialVless(t *testing.T, port int, useTLS bool) net.Conn {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if !useTLS {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial vless: %v", err)
		}
		return conn
	}
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         "vpn.example.com",
		NextProtos:         []string{"h2", "http/1.1"},
	})
	if err != nil {
		t.Fatalf("tls dial vless: %v", err)
	}
	return conn
}

// vlessRequest 按 VLESS 头布局拼请求头；command 为 Mux 时不带地址段。
func vlessRequest(command byte, host string, port int) []byte {
	raw, _ := vmess.ParseUUID(e2eUUID)
	buf := []byte{0x00}
	buf = append(buf, raw[:]...)
	buf = append(buf, 0x00, command)
	if command == 0x01 {
		buf = append(buf, byte(port>>8), byte(port), 0x02, byte(len(host)))
		buf = append(buf, host...)
	}
	return buf
}

// readResponseHeader 读 2B 响应头并校验 [0,0]。
func readResponseHeader(t *testing.T, conn net.Conn) {
	t.Helper()
	var resp [2]byte
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		t.Fatalf("read response header: %v", err)
	}
	if resp != [2]byte{0x00, 0x00} {
		t.Fatalf("response header = %v, want [0 0]", resp)
	}
	_ = conn.SetReadDeadline(time.Time{})
}

// runSingleConnection TCP 直达：发 HTTP 请求，读响应。
func runSingleConnection(t *testing.T, conn net.Conn, host string, port int) {
	t.Helper()
	if _, err := conn.Write(vlessRequest(0x01, host, port)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	readResponseHeader(t, conn)

	request := "GET / HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(string(got), "hello-from-origin") {
		t.Fatalf("response = %q, want origin payload", string(got))
	}
}

// mux 帧状态与选项（客户端侧独立副本，与服务端常量互为对照）。
const (
	statusNew      = 0x01
	statusKeep     = 0x02
	statusEnd      = 0x03
	optionData     = 0x01
	networkTCP     = 0x01
	addrTypeDomain = 0x02
)

// muxNewFrame 拼一条 New 帧（首数据 = 应用的第一个报文）。
func muxNewFrame(id uint16, host string, port int, first []byte) []byte {
	meta := []byte{byte(id >> 8), byte(id), statusNew, optionData, networkTCP,
		byte(port >> 8), byte(port), addrTypeDomain, byte(len(host))}
	meta = append(meta, host...)
	buf := make([]byte, 2, 2+len(meta)+2+len(first))
	binary.BigEndian.PutUint16(buf, uint16(len(meta)))
	buf = append(buf, meta...)
	buf = append(buf, byte(len(first)>>8), byte(len(first)))
	return append(buf, first...)
}

// runMuxSubConnections 在一条主连接上先后开两条子连接，各拿一次源站响应
// （v2ray 客户端的典型用法）。
func runMuxSubConnections(t *testing.T, conn net.Conn, host string, port int) {
	t.Helper()
	if _, err := conn.Write(vlessRequest(0x03, "", 0)); err != nil {
		t.Fatalf("write mux request: %v", err)
	}
	readResponseHeader(t, conn)

	for i, marker := range []string{"first", "second"} {
		id := uint16(i + 1)
		req := "GET / HTTP/1.1\r\nHost: " + host + "\r\nX-Marker: " + marker + "\r\nConnection: close\r\n\r\n"
		if _, err := conn.Write(muxNewFrame(id, host, port, []byte(req))); err != nil {
			t.Fatalf("sub-connection %d: write: %v", i, err)
		}
		got := readSubConnection(t, conn, id)
		if !strings.Contains(got, "hello-from-origin") {
			t.Fatalf("sub-connection %d response = %q, want origin payload", i, got)
		}
	}
}

// readSubConnection 读一个子连接的响应：Keep 帧数据直到该会话的 End 帧。
func readSubConnection(t *testing.T, conn net.Conn, id uint16) string {
	t.Helper()
	var got []byte
	for {
		var lenBuf [2]byte
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			t.Fatalf("read frame: %v", err)
		}
		meta := make([]byte, binary.BigEndian.Uint16(lenBuf[:]))
		if _, err := io.ReadFull(conn, meta); err != nil {
			t.Fatalf("read meta: %v", err)
		}
		if len(meta) < 4 {
			t.Fatal("short metadata")
		}
		sid, status, option := binary.BigEndian.Uint16(meta[0:2]), meta[2], meta[3]
		var data []byte
		if option&optionData != 0 {
			var dl [2]byte
			if _, err := io.ReadFull(conn, dl[:]); err != nil {
				t.Fatalf("read data len: %v", err)
			}
			data = make([]byte, binary.BigEndian.Uint16(dl[:]))
			if _, err := io.ReadFull(conn, data); err != nil {
				t.Fatalf("read data: %v", err)
			}
		}
		switch status {
		case statusKeep:
			if sid == id {
				got = append(got, data...)
			}
		case statusEnd:
			if sid == id {
				return string(got)
			}
		}
	}
}

// directDispatcher 直连出站的默认分发器。
func directDispatcher() dispatch.Dispatcher {
	return dispatch.NewDefaultDispatcher(nil, "direct", dispatch.NewDirectOutbound("direct"))
}

// freePort 占一个随机端口再释放，拿一个大概率空闲的端口号（仓库既有的竞态规避写法）。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}
