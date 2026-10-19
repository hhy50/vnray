package portmap_test

// 端口映射端到端：真实服务端（ConnectorService）+ 真实客户端（client.Dial/Serve），
// 验证「外部连接服务端映射端口 → 隧道 → 客户端本地拨号」整条链路的转发与半关闭。

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/client"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/core"
	"github.com/hhy50/vnray/internal/portmap"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
)

// startEchoServer 起一个回显 TCP 服务，返回其地址；测试结束关闭。
func startEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String()
}

// freePort 取一个空闲 TCP 端口（先听后关，测试场景可接受竞态）。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestPortMapEndToEnd 验证映射链路：启用即转发、停用后 Sync 即拒绝。
func TestPortMapEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := store.Open(ctx, "sqlite", filepath.Join(t.TempDir(), "portmap.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	echoAddr := startEchoServer(t)
	_, echoPort, _ := net.SplitHostPort(echoAddr)
	echoPortNum, _ := strconv.Atoi(echoPort)

	const clientName = "test:default:c1"
	remotePort := freePort(t)
	m := store.PortMapping{Client: clientName, RemotePort: remotePort,
		LocalHost: "127.0.0.1", LocalPort: echoPortNum,
		Config:  store.PortConfig{UpLimit: 512 * 1024, DownLimit: 256 * 1024},
		Enabled: true, Remark: "e2e"}
	if err := st.SetPortMapping(ctx, m); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	cfg := config.DefaultServer()
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.Auth.Key = "portmap-e2e-key"

	sessions := transport.NewSessionManager()
	limits := ratelimit.NewRegistry()
	meter := traffic.NewMeter(limits)
	svc := core.NewConnectorService(&cfg, sessions, nil, limits)
	svc.SetTraffic(meter)
	// 关掉心跳：本用例只关心端口映射链路，不需要探测噪声。
	svc.ApplyHeartbeat(0, 0)
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start connector: %v", err)
	}
	defer svc.Drain(context.Background())

	pm := portmap.NewManager(sessions, st, meter, limits)
	if err := pm.Start(ctx); err != nil {
		t.Fatalf("start portmap manager: %v", err)
	}
	if got := pm.ListeningIDs(); len(got) != 1 || !got[1] {
		t.Fatalf("listener not running after Start: %v", got)
	}
	// 映射限速应已灌进注册表（按映射身份隔离）。
	mpc := m.Config
	if up, down := limits.Limits(portmap.Identity(1)); up != mpc.UpLimit || down != mpc.DownLimit {
		t.Fatalf("limits after Start = (%d,%d), want (%d,%d)", up, down, mpc.UpLimit, mpc.DownLimit)
	}

	// 客户端接入：名字与映射的 client 字段一致才被选路。
	dialCtx, stopClient := context.WithCancel(ctx)
	defer stopClient()
	c, err := client.Dial(svc.Addr(), nil, client.DialOptions{
		Auth: client.AuthOptions{Key: cfg.Server.Auth.Key},
		Info: protocol.ClientInfo{Name: clientName},
	})
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	go func() {
		_ = c.Serve()
		<-dialCtx.Done()
	}()
	defer c.Close()

	// 等服务端登记该客户端会话（握手异步完成）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := sessions.SessionByName(clientName); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client session never registered")
		}
		time.Sleep(20 * time.Millisecond)
	}

	roundTrip(t, "127.0.0.1", remotePort, "hello-portmap")

	// 改限速 → Sync 热更新注册表速率（在途桶原地变速，监听器不重建）。
	saved, ok, err := st.GetPortMapping(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("get mapping: ok=%v err=%v", ok, err)
	}
	saved.Config = store.PortConfig{UpLimit: 64 * 1024, DownLimit: 32 * 1024}
	spc := saved.Config
	if err := st.SetPortMapping(ctx, saved); err != nil {
		t.Fatalf("update limits: %v", err)
	}
	pm.Sync(ctx)
	if up, down := limits.Limits(portmap.Identity(1)); up != spc.UpLimit || down != spc.DownLimit {
		t.Fatalf("limits after update = (%d,%d), want (%d,%d)", up, down, spc.UpLimit, spc.DownLimit)
	}
	if got := pm.ListeningIDs(); len(got) != 1 || !got[1] {
		t.Fatalf("listener should survive a limits-only update: %v", got)
	}

	// 停用 → Sync 关监听 → 新连接被拒。（更新须取回 ID：remote_port 全表唯一。）
	saved.Enabled = false
	if err := st.SetPortMapping(ctx, saved); err != nil {
		t.Fatalf("disable mapping: %v", err)
	}
	pm.Sync(ctx)
	if len(pm.ListeningIDs()) != 0 {
		t.Fatalf("listener still running after disable: %v", pm.ListeningIDs())
	}
	if conn, err := net.DialTimeout("tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(remotePort)), time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("connection accepted on disabled mapping")
	}
}

// roundTrip 连接服务端映射端口，写一行、读回显，校验转发链路可达。
func roundTrip(t *testing.T, host string, port int, payload string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		t.Fatalf("dial mapped port: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != payload {
		t.Fatalf("echo mismatch: got %q, want %q", buf, payload)
	}
}
