package plugin

// vless 用户把出口绑到保留名 server:self 时，流量要经服务端本机出口直接出站，
// 而不是走远程客户端会话。这条路径此前没有任何覆盖：插件只依赖 ExitResolver
// 接口，本机出口未启用时接口一律未命中，于是连接被静默关掉——现象就是
// "选了本机、连不上"，而日志还在报客户端离线。

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/vmess"
)

// stubExitResolver 复刻 exitResolver 对保留名的语义：server:self 只归本机出口，
// 未启用时未命中且不回落到远程会话。
type stubExitResolver struct {
	local  transport.Exit
	remote map[string]transport.Exit
}

func (r *stubExitResolver) Resolve(name string) (transport.Exit, bool) {
	if name == transport.LocalClientName {
		if r.local == nil {
			return nil, false
		}
		return r.local, true
	}
	exit, ok := r.remote[name]
	return exit, ok
}

// recordingExit 是本机出口的替身：记下 OpenTunnel 的目标，并把隧道对端交给测试，
// 便于断言数据真的搬过去了（而不是只验证"函数被调用"）。
type recordingExit struct {
	mu    sync.Mutex
	dests []dispatch.Destination
	peers chan net.Conn
}

func newRecordingExit() *recordingExit {
	return &recordingExit{peers: make(chan net.Conn, 4)}
}

func (e *recordingExit) OpenTunnel(dest dispatch.Destination, _ protocol.TunnelRequest) (net.Conn, error) {
	e.mu.Lock()
	e.dests = append(e.dests, dest)
	e.mu.Unlock()
	local, peer := net.Pipe()
	e.peers <- peer
	return local, nil
}

func (e *recordingExit) destsSnapshot() []dispatch.Destination {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]dispatch.Destination(nil), e.dests...)
}

// freePluginPort 占一个随机端口再释放，拿一个大概率空闲的端口号。
func freePluginPort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	return port
}

// startVLESSInbound 起一个只监听回环的 v2ray 插件，返回插件与 vless 端口。
// 两个端口都必须是合法值，否则 Enable 直接拒绝。
func startVLESSInbound(t *testing.T, exits transport.ExitResolver) (*V2RayPlugin, int) {
	t.Helper()
	p := newV2RayPlugin(seedTestStore(t), nil, exits, nil)
	vmessPort, vlessPort := freePluginPort(t), freePluginPort(t)
	if err := p.Enable(context.Background(), map[string]string{
		"vmess_port": strconv.Itoa(vmessPort),
		"vless_port": strconv.Itoa(vlessPort),
	}); err != nil {
		t.Fatalf("enable v2ray plugin: %v", err)
	}
	t.Cleanup(p.Stop)
	waitListening(t, vlessPort)
	return p, vlessPort
}

// waitListening 在 2 秒内轮询到端口可连；插件在 Enable 里同步起监听，
// 但这里仍留余量以避开慢 CI。
func waitListening(t *testing.T, port int) {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("vless port %d is not listening: %v", port, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// vlessRequestFrame 拼一条明文 VLESS 请求头：version + uuid + addonsLen(0) +
// cmd(TCP) + 目标端口 + 域名地址。addonsLen 必须为 0——本入站不支持 flow。
func vlessRequestFrame(t *testing.T, uuid, host string, port int) []byte {
	t.Helper()
	raw, err := vmess.ParseUUID(uuid)
	if err != nil {
		t.Fatalf("parse uuid: %v", err)
	}
	frame := make([]byte, 0, 64)
	frame = append(frame, 0x00)
	frame = append(frame, raw[:]...)
	frame = append(frame, 0x00)
	frame = append(frame, 0x01)
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(port))
	frame = append(frame, p[:]...)
	frame = append(frame, 0x02, byte(len(host)))
	frame = append(frame, host...)
	return frame
}

// dialVLESS 连上 vless 入站、发请求头并吃掉 2 字节响应头，返回可承载数据的连接。
func dialVLESS(t *testing.T, port int, uuid, host string, targetPort int) net.Conn {
	t.Helper()
	// 插件首次启用默认签发自签证书，测试用跳过校验的 TLS 客户端接入。
	conn, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		&tls.Config{InsecureSkipVerify: true, ServerName: "vnray.local"})
	if err != nil {
		t.Fatalf("tls dial vless: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write(vlessRequestFrame(t, uuid, host, targetPort)); err != nil {
		t.Fatalf("write vless request: %v", err)
	}
	var resp [2]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		t.Fatalf("read vless response header: %v", err)
	}
	return conn
}

// TestVLESSLocalExitRelay 验证绑了 server:self 的 vless 用户在数据面完整可用：
// 请求头解析后经本机出口建隧道，双向字节都能搬过去。
func TestVLESSLocalExitRelay(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	local := newRecordingExit()
	p, port := startVLESSInbound(t, &stubExitResolver{local: local})
	p.SyncUser(store.ProtocolVLESS, uuid, transport.LocalClientName, store.UserEnabled, false)

	conn := dialVLESS(t, port, uuid, "example.com", 443)
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	var peer net.Conn
	select {
	case peer = <-local.peers:
	case <-time.After(2 * time.Second):
		t.Fatal("本机出口没有被调用：vless 流量没有走到 server:self")
	}
	defer func() { _ = peer.Close() }()

	dests := local.destsSnapshot()
	if len(dests) != 1 || dests[0].Host != "example.com" || dests[0].Port != 443 {
		t.Fatalf("本机出口收到的目标 = %+v, want example.com:443", dests)
	}
	if dests[0].Network != "tcp" {
		t.Fatalf("本机出口收到的网络 = %q, want tcp", dests[0].Network)
	}

	// 上行：客户端写进隧道，出口侧读到同样的字节。
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write upstream: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(peer, got); err != nil {
		t.Fatalf("read upstream at exit: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("上行字节 = %q, want ping", got)
	}

	// 下行：出口侧写回，客户端读到同样的字节。
	if _, err := peer.Write([]byte("pong")); err != nil {
		t.Fatalf("write downstream: %v", err)
	}
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read downstream at client: %v", err)
	}
	if string(got) != "pong" {
		t.Fatalf("下行字节 = %q, want pong", got)
	}
}

// TestVLESSLocalExitDisabledClosesConn 验证 server_proxy 未启用时的行为：连接
// 必须在认证之后被关掉，而不是挂住等待。日志文案另由 TestBoundClientMessage 覆盖。
func TestVLESSLocalExitDisabledClosesConn(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	p, port := startVLESSInbound(t, &stubExitResolver{})
	p.SyncUser(store.ProtocolVLESS, uuid, transport.LocalClientName, store.UserEnabled, false)

	conn := dialVLESS(t, port, uuid, "example.com", 443)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var buf [1]byte
	if _, err := conn.Read(buf[:]); err == nil {
		t.Fatal("本机出口未启用时应关闭连接，实际仍可读")
	}
}

// TestBoundClientMessage 断言出口未命中时区分两种原因：绑了本机出口但没启用是配置
// 问题，报"客户端离线"会把排查方向带偏（ss 入站早已按这个口径区分）。
func TestBoundClientMessage(t *testing.T) {
	if got := boundClientMessage("vless", transport.LocalClientName); got != "vless: server:self bound but server_proxy is disabled, closing inbound conn" {
		t.Fatalf("本机出口未启用的文案 = %q", got)
	}
	if got := boundClientMessage("vless", "edge-1"); got != "vless: bound client offline, closing inbound conn" {
		t.Fatalf("客户端离线的文案 = %q", got)
	}
}
