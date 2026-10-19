package vmess_test

// v2ray 插件端到端：vmess 模拟客户端 → v2ray 入站 → uuid 路由 → 隧道 →
// 真实客户端（vnray-client）→ direct 出站 → HTTP 源站。

import (
	"context"
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

// testStore 在临时目录开一个 sqlite 库，测试结束自动关闭；同包测试共用。
func testStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestV2RayPluginEndToEnd 跑完整链路：vmess 模拟客户端 → v2ray 入站 → uuid 路由 →
// 隧道 → 真实客户端（vnray-client）→ direct 出站 → HTTP 源站。后端经 Muxer 注入，
// 与生产装配同一条路径。
func TestV2RayPluginEndToEnd(t *testing.T) {
	t.Parallel()
	runV2RayPluginEndToEnd(t, tcp.YamuxMuxer())
}

func runV2RayPluginEndToEnd(t *testing.T, muxer tcp.Muxer) {
	// HTTP 源站。
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello-from-origin"))
	}))
	defer origin.Close()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	if err != nil {
		t.Fatalf("parse origin addr: %v", err)
	}
	originPort, _ := strconv.Atoi(portStr)

	// 数据面服务 + 真实客户端接入。
	cfg := &config.Server{
		Server: config.ServerOptions{
			Listen: "127.0.0.1:0",
			Auth:   config.Auth{Key: "e2e-key"},
		},
	}
	sessions := transport.NewSessionManager()
	svc := core.NewConnectorServiceWith(cfg, sessions, nil, nil, tcp.WithMuxer(muxer))
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start data plane: %v", err)
	}
	defer func() { _ = svc.Drain(context.Background()) }()

	agent, err := client.Dial(svc.Addr(), directDispatcher(), client.DialOptions{
		Auth:  client.AuthOptions{Key: "e2e-key"},
		Info:  protocol.ClientInfo{Name: "test-edge", Version: "e2e", OS: "test", Arch: "test"},
		Muxer: muxer,
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

	// v2ray 插件：启用 + uuid 路由（uuid 用户写进绑定表，与生产装配同一条路径）。
	st := testStore(t)
	bindings := admin.NewBindings(st)
	plugins := plugin.NewManager(st, bindings, svc.Sessions(), nil)
	// 装配层在真实进程里用这条观察者把用户表变更推给验证器；这里照做以复现同一链路。
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
	v2ray, err := plugins.Get("v2ray")
	if err != nil {
		t.Fatalf("get plugin: %v", err)
	}
	vmessPort, vlessPort := freePort(t), freePort(t)
	if err := v2ray.Enable(context.Background(), map[string]string{
		"vmess_port": strconv.Itoa(vmessPort),
		"vless_port": strconv.Itoa(vlessPort),
	}); err != nil {
		t.Fatalf("enable v2ray: %v", err)
	}
	// 与生产一致：uuid 用户经用户表登记，插件靠观察者跟随。
	if err := bindings.PutUser(store.UserBinding{
		Key: e2eUUID, Protocol: store.ProtocolVMess, Name: "e2e-vmess",
		Status: store.UserEnabled, Client: "test-edge",
	}); err != nil {
		t.Fatalf("register uuid: %v", err)
	}

	// vmess 模拟客户端走全链路。
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(vmessPort))
	if err != nil {
		t.Fatalf("dial v2ray: %v", err)
	}
	defer conn.Close()
	sess, err := vmess.NewClientSession(e2eUUID, conn,
		vmess.Target{Host: host, Port: originPort},
		vmess.SecurityAES128GCM, vmess.OptionChunkStream|vmess.OptionChunkMasking)
	if err != nil {
		t.Fatalf("vmess handshake: %v", err)
	}
	if err := sess.ReadResponseHeader(); err != nil {
		t.Fatalf("read response header: %v", err)
	}

	w := sess.BodyWriter()
	request := "GET / HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n"
	if _, err := w.Write([]byte(request)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = w.Close()

	got, err := io.ReadAll(sess.BodyReader())
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(string(got), "hello-from-origin") {
		t.Fatalf("response = %q, want origin payload", string(got))
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
