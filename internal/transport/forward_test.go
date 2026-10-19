package transport_test

// 四端链路测试：用户 A 与客户端 D 连同一个主端口（多路复用会话靠握手首帧的
// Auth/UserAuth 区分角色），A 以 UserAuth 出示 user_key 认证（密钥须已在注册表
// 登记），之后的每条 Forward 流经其绑定的客户端去访问只有该客户端够得到的目标 B。
// 链路：A → 主端口 → 用户会话 → Forward 流 → 服务端C → Tunnel 流 → 客户端D → B。

import (
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/acl"
	"github.com/hhy50/vnray/internal/client"
	"github.com/hhy50/vnray/internal/core"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/transport"
)

// 隧道长连接测试参数：沿用代理长连接测试的压缩 keepalive 与轮询间隔。
const (
	forwardTestKey = "user-alice-key-001"
	forwardClient  = "test-edge"
	tunnelHold     = 6 * time.Second
)

// testRegistry 是测试用的 user_key → 接入信息 注册表（实现 transport.UserRegistry）。
// 表里的用户一律视为启用。
type testRegistry map[string]string

func (r testRegistry) UserFor(key string) (transport.UserAccess, bool) {
	client, ok := r[key]
	return transport.UserAccess{Client: client, Name: key, Active: true}, ok
}

// startEchoServer 起测试服务 B：把收到的字节原样写回。
func startEchoServer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo server: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String()
}

// startForwardLink 组装服务端 C 与客户端 D：C 接受用户接入并注册 user_key → clientName，
// D 以 clientName 接入；返回服务端句柄。cfg/muxer 为 nil 时用默认值，两端一致。
func startForwardLink(t *testing.T, cfg *tcp.SessionConfig, muxer tcp.Muxer, registry transport.UserRegistry, clientName string) *core.ConnectorService {
	t.Helper()

	plane := startPlane(t, planeOptions{
		authKey:       testAuthKey,
		heartbeat:     core.HeartbeatOptions{Interval: longConnHeartbeatInterval, Timeout: time.Second},
		sessionConfig: cfg,
		muxer:         muxer,
	}, registry)

	if clientName == "" {
		return plane // 不接入客户端，用于验证"客户端离线"路径
	}

	client, err := client.Dial(plane.Addr(), directDispatcher(), client.DialOptions{
		Auth:          client.AuthOptions{Key: testAuthKey},
		Info:          protocol.ClientInfo{Name: clientName, Version: "v-test", OS: "testos", Arch: "testarch"},
		SessionConfig: cfg,
		Muxer:         muxer,
	})
	if err != nil {
		t.Fatalf("dial data plane: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- client.Serve() }()
	t.Cleanup(func() {
		_ = client.Close()
		select {
		case err := <-serveDone:
			if err != nil {
				t.Errorf("client serve: %v", err)
			}
		case <-time.After(sessionTimeout):
			t.Error("client serve did not return after close")
		}
	})
	return plane
}

// testUserInfo 是测试用户端随 UserAuth 上报的自信息。
func testUserInfo() protocol.UserInfo {
	return protocol.UserInfo{
		Name: "test-user", Version: "v-test", OS: "testos", Arch: "testarch",
		Hostname: "test-host", PID: 4242,
	}
}

// dialUser 以用户身份连主端口并完成 UserAuth：成功返回用户多路复用会话与服务端应答
// （用户路径的应答带绑定客户端的配置）；被拒时会话已关闭、应答 ok=false。
// cfg/muxer 为 nil 时用默认值。
func dialUser(t *testing.T, plane *core.ConnectorService, key string, cfg *tcp.SessionConfig, muxer tcp.Muxer) (tcp.Session, protocol.Reply) {
	t.Helper()

	conn, err := net.Dial("tcp", plane.Addr())
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}
	if muxer == nil {
		muxer = tcp.DefaultMuxer()
	}
	if cfg == nil {
		cfg = tcp.DefaultSessionConfig()
	}
	user, err := muxer.Client(conn, cfg)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("create %s client: %v", muxer.Name(), err)
	}

	stream, err := user.OpenStream()
	if err != nil {
		t.Fatalf("open user auth stream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	_ = stream.SetDeadline(time.Now().Add(transport.TunnelTimeout))

	frame, err := protocol.NewUserAuthFrame(protocol.UserAuthRequest{Key: key, Info: testUserInfo()})
	if err != nil {
		t.Fatalf("build user auth frame: %v", err)
	}
	if err := protocol.NewEncoder(stream).WriteFrame(frame); err != nil {
		t.Fatalf("write user auth frame: %v", err)
	}
	resp, err := protocol.NewDecoder(stream).ReadFrame()
	if err != nil {
		t.Fatalf("read user auth result: %v", err)
	}
	if resp.Type != protocol.AuthResult {
		_ = resp.CloseBody()
		t.Fatalf("frame type = %d, want AuthResult", resp.Type)
	}
	payload, err := protocol.ReadPayload(resp)
	if err != nil {
		t.Fatalf("read user auth payload: %v", err)
	}
	res, err := protocol.ParseReply(payload)
	if err != nil {
		_ = user.Close()
		t.Fatalf("parse user auth reply: %v", err)
	}
	if !res.OK {
		_ = user.Close()
		return nil, res
	}

	t.Cleanup(func() { _ = user.Close() })
	return user, res
}

// openForward 在用户会话上开一条 Forward 流完成协商，返回可承载裸字节的流。
func openForward(t *testing.T, user tcp.Session, dest dispatch.Destination) tcp.Stream {
	t.Helper()

	stream, err := user.OpenStream()
	if err != nil {
		t.Fatalf("open forward stream: %v", err)
	}
	_ = stream.SetDeadline(time.Now().Add(transport.TunnelTimeout))

	frame, err := protocol.NewForwardFrame(protocol.Address{Host: dest.Host, Port: uint16(dest.Port)})
	if err != nil {
		t.Fatalf("build forward frame: %v", err)
	}
	if err := protocol.NewEncoder(stream).WriteFrame(frame); err != nil {
		t.Fatalf("write forward frame: %v", err)
	}
	resp, err := protocol.NewDecoder(stream).ReadFrame()
	if err != nil {
		t.Fatalf("read forward result: %v", err)
	}
	if resp.Type != protocol.ForwardResult {
		_ = resp.CloseBody()
		t.Fatalf("frame type = %d, want ForwardResult", resp.Type)
	}
	payload, err := protocol.ReadPayload(resp)
	if err != nil {
		t.Fatalf("read forward payload: %v", err)
	}
	res, err := protocol.ParseReply(payload)
	if err != nil {
		t.Fatalf("parse forward reply: %v", err)
	}
	if !res.OK {
		_ = stream.Close()
		t.Fatalf("forward rejected: %s", res.Reason)
	}
	_ = stream.SetDeadline(time.Time{})
	return stream
}

// openForwardResult 同 openForward，但被拒时不 Fatal，返回拒绝原因供断言。
func openForwardResult(t *testing.T, user tcp.Session, dest dispatch.Destination) (ok bool, reason string) {
	t.Helper()

	stream, err := user.OpenStream()
	if err != nil {
		t.Fatalf("open forward stream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	_ = stream.SetDeadline(time.Now().Add(transport.TunnelTimeout))

	frame, err := protocol.NewForwardFrame(protocol.Address{Host: dest.Host, Port: uint16(dest.Port)})
	if err != nil {
		t.Fatalf("build forward frame: %v", err)
	}
	if err := protocol.NewEncoder(stream).WriteFrame(frame); err != nil {
		t.Fatalf("write forward frame: %v", err)
	}
	resp, err := protocol.NewDecoder(stream).ReadFrame()
	if err != nil {
		t.Fatalf("read forward result: %v", err)
	}
	if resp.Type != protocol.ForwardResult {
		_ = resp.CloseBody()
		t.Fatalf("frame type = %d, want ForwardResult", resp.Type)
	}
	payload, err := protocol.ReadPayload(resp)
	if err != nil {
		t.Fatalf("read forward payload: %v", err)
	}
	res, err := protocol.ParseReply(payload)
	if err != nil {
		t.Fatalf("parse forward reply: %v", err)
	}
	return res.OK, res.Reason
}

// echoLine 写一行并读回等长的一行。
func echoLine(t *testing.T, conn io.ReadWriter, line string) string {
	t.Helper()

	if _, err := io.WriteString(conn, line); err != nil {
		t.Fatalf("write %q: %v", line, err)
	}
	buf := make([]byte, len(line))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo of %q: %v", line, err)
	}
	return string(buf)
}

// TestForwardHubLongLivedTunnel 验证"代理长连接"：经 A → C → D → B 建立的隧道在跨越
// 多个 keepalive 超时窗口的时间里持续可用，且字节双向无误。
func TestForwardHubLongLivedTunnel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping long-lived tunnel test in short mode")
	}

	dest, err := dispatch.ParseDestination(startEchoServer(t))
	if err != nil {
		t.Fatalf("parse echo server address: %v", err)
	}
	cfg := longConnSessionConfig()
	plane := startForwardLink(t, cfg, nil, testRegistry{forwardTestKey: forwardClient}, forwardClient)

	user, res := dialUser(t, plane, forwardTestKey, cfg, nil)
	if !res.OK {
		t.Fatalf("user auth rejected: %s", res.Reason)
	}
	stream := openForward(t, user, dest)

	start := time.Now()
	var rounds int
	for time.Since(start) < tunnelHold {
		line := fmt.Sprintf("ping-%04d\n", rounds)
		if got := echoLine(t, stream, line); got != line {
			t.Fatalf("round %d echo = %q, want %q", rounds, got, line)
		}
		rounds++
		time.Sleep(longConnRequestInterval)
	}

	t.Logf("held one tunnel for %s across %d keepalive timeout windows with %d echo round trips",
		tunnelHold, int(tunnelHold/longConnKeepAliveTimeout), rounds)
}

// TestForwardHubDenials 验证用户接入的拒绝路径：密钥未注册、用户接入未启用、
// 客户端离线、目标被 ACL 拒绝。
func TestForwardHubDenials(t *testing.T) {
	dest, err := dispatch.ParseDestination(startEchoServer(t))
	if err != nil {
		t.Fatalf("parse echo server address: %v", err)
	}
	registry := testRegistry{forwardTestKey: forwardClient}

	t.Run("未注册的 user_key", func(t *testing.T) {
		plane := startForwardLink(t, nil, nil, registry, forwardClient)
		_, res := dialUser(t, plane, "not-registered-key", nil, nil)
		if res.OK {
			t.Fatal("user auth should be rejected for an unregistered key")
		}
		if !strings.Contains(res.Reason, "not registered") {
			t.Fatalf("reason = %q, want it to mention the key is not registered", res.Reason)
		}
	})

	t.Run("用户接入未启用", func(t *testing.T) {
		plane := startPlane(t, planeOptions{authKey: testAuthKey}, nil)

		_, res := dialUser(t, plane, forwardTestKey, nil, nil)
		if res.OK {
			t.Fatal("user auth should be rejected when forward is disabled")
		}
		if !strings.Contains(res.Reason, "not enabled") {
			t.Fatalf("reason = %q, want it to mention user access is not enabled", res.Reason)
		}
	})

	t.Run("客户端离线", func(t *testing.T) {
		plane := startForwardLink(t, nil, nil, registry, "")
		user, res := dialUser(t, plane, forwardTestKey, nil, nil)
		if !res.OK {
			t.Fatalf("user auth rejected: %s", res.Reason)
		}
		if ok, reason := openForwardResult(t, user, dest); ok {
			t.Fatal("forward should be rejected when the bound client is offline")
		} else if !strings.Contains(reason, "offline") {
			t.Fatalf("reason = %q, want it to mention the client is offline", reason)
		}
	})

	t.Run("目标被 ACL 拒绝", func(t *testing.T) {
		guard, gerr := acl.NewGuard("", "127.0.0.0/8")
		if gerr != nil {
			t.Fatalf("build acl guard: %v", gerr)
		}
		plane := startPlane(t, planeOptions{authKey: testAuthKey}, registry)

		client, derr := client.Dial(plane.Addr(), guard.Wrap(directDispatcher()), client.DialOptions{
			Auth: client.AuthOptions{Key: testAuthKey},
			Info: protocol.ClientInfo{Name: forwardClient},
		})
		if derr != nil {
			t.Fatalf("dial data plane: %v", derr)
		}
		defer func() { _ = client.Close() }()
		go func() { _ = client.Serve() }()

		user, res := dialUser(t, plane, forwardTestKey, nil, nil)
		if !res.OK {
			t.Fatalf("user auth rejected: %s", res.Reason)
		}
		if ok, reason := openForwardResult(t, user, dest); ok {
			t.Fatal("forward should be rejected when the client's acl denies the target")
		} else if !strings.Contains(reason, "acl") {
			t.Fatalf("reason = %q, want it to carry the acl denial", reason)
		}
	})
}

// TestUserAuthReturnsClientConfig 验证用户侧两件事：UserAuth 把用户端自报信息带到
// 服务端并随会话登记；认证应答回带绑定客户端的配置（在线=完整上报值，离线=仅名字）。
func TestUserAuthReturnsClientConfig(t *testing.T) {
	registry := testRegistry{forwardTestKey: forwardClient}

	t.Run("客户端在线", func(t *testing.T) {
		plane := startForwardLink(t, nil, nil, registry, forwardClient)
		_, res := dialUser(t, plane, forwardTestKey, nil, nil)
		if !res.OK {
			t.Fatalf("user auth rejected: %s", res.Reason)
		}
		if res.Client == nil || res.Client.Name != forwardClient {
			t.Fatalf("client = %+v, want the bound client %q", res.Client, forwardClient)
		}
		if res.Client.Version != "v-test" || res.Client.OS != "testos" || res.Client.Arch != "testarch" {
			t.Fatalf("client config = %+v, want the values reported by the client", res.Client)
		}
		assertUserInfo(t, plane)
	})

	t.Run("客户端离线", func(t *testing.T) {
		plane := startForwardLink(t, nil, nil, registry, "")
		_, res := dialUser(t, plane, forwardTestKey, nil, nil)
		if !res.OK {
			t.Fatalf("user auth rejected: %s", res.Reason)
		}
		if res.Client == nil || res.Client.Name != forwardClient || res.Client.Version != "" {
			t.Fatalf("client = %+v, want only the bound client name while it is offline", res.Client)
		}
		assertUserInfo(t, plane)
	})
}

// assertUserInfo 轮询断言服务端登记的用户会话带着 UserAuth 上报的自信息与 user_key
// （AuthResult 先于 OnSessionOpen 发出，登记可能稍迟，故轮询而非一次性快照）。
func assertUserInfo(t *testing.T, plane *core.ConnectorService) {
	t.Helper()

	deadline := time.Now().Add(sessionTimeout)
	for {
		for _, si := range plane.Sessions().Snapshot() {
			if si.Kind != tcp.KindUser {
				continue
			}
			meta, ok := si.Meta.(transport.UserMeta)
			if !ok {
				t.Fatalf("user session meta = %T, want transport.UserMeta", si.Meta)
			}
			if meta.Key != forwardTestKey {
				t.Fatalf("user key = %q, want %q", meta.Key, forwardTestKey)
			}
			if !reflect.DeepEqual(meta.Info, testUserInfo()) {
				t.Fatalf("reported user info = %+v, want %+v", meta.Info, testUserInfo())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the user session to be registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
