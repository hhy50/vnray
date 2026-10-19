package core

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/transport"
)

// startEcho 起一个回显 TCP 服务，返回监听地址与端口。
func startEcho(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

// TestLocalExitACLReject 验证 ACL 黑名单命中时同步返回结构化拒绝错误。
func TestLocalExitACLReject(t *testing.T) {
	e, err := buildLocalExit(config.ServerProxySettings{Private: "127.0.0.0/8"})
	if err != nil {
		t.Fatalf("newLocalExit: %v", err)
	}
	_, err = e.OpenTunnel(dispatch.Destination{Host: "127.0.0.1", Port: 80}, protocol.TunnelRequest{})
	if err == nil {
		t.Fatal("expected acl rejection")
	}
	var oe *dispatch.OutboundError
	if !errors.As(err, &oe) {
		t.Fatalf("want OutboundError, got %v", err)
	}
}

// TestLocalExitRoutingReject 验证 routing 规则命中的目标走 reject 出站，原因随 Link 回传。
func TestLocalExitRoutingReject(t *testing.T) {
	rc := config.Routing{
		Default: "direct",
		Outbounds: []config.Outbound{
			{Tag: "direct", Type: "direct"},
			{Tag: "reject", Type: "reject", Reason: "blocked by test route"},
		},
		Rules: []config.Rule{
			{Name: "block", Outbound: "reject", Match: config.Match{Host: []string{"blocked.example"}}},
		},
	}
	e, err := buildLocalExit(config.ServerProxySettings{Routing: rc})
	if err != nil {
		t.Fatalf("newLocalExit: %v", err)
	}
	conn, err := e.OpenTunnel(dispatch.Destination{Host: "blocked.example", Port: 443}, protocol.TunnelRequest{})
	if err != nil {
		t.Fatalf("open tunnel: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_, rerr := conn.Read(make([]byte, 1))
	if rerr == nil {
		t.Fatal("expected read error from reject outbound")
	}
	if !strings.Contains(rerr.Error(), "blocked by test route") {
		t.Fatalf("reject reason = %v, want to contain route reason", rerr)
	}
}

// TestLocalExitDirect 验证本机出口对允许目标直连并完成双向搬运。
func TestLocalExitDirect(t *testing.T) {
	host, port := startEcho(t)
	e, err := buildLocalExit(config.ServerProxySettings{Routing: config.DefaultServerProxyRouting()})
	if err != nil {
		t.Fatalf("newLocalExit: %v", err)
	}
	conn, err := e.OpenTunnel(dispatch.Destination{Host: host, Port: port}, protocol.TunnelRequest{})
	if err != nil {
		t.Fatalf("open tunnel: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 4)
		_, err := io.ReadFull(conn, buf)
		if err == nil && string(buf) != "ping" {
			err = fmt.Errorf("echo mismatch %q", buf)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("echo: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting echo")
	}
}

// fakeExit 是仅用于解析测试的占位出口。
type fakeExit struct{}

func (fakeExit) OpenTunnel(dispatch.Destination, protocol.TunnelRequest) (net.Conn, error) {
	return nil, nil
}

// fakeRemote 是仅按名放行 remote-1 的占位远程解析器。
type fakeRemote struct{ e transport.Exit }

func (f fakeRemote) Resolve(name string) (transport.Exit, bool) {
	if name == "remote-1" {
		return f.e, true
	}
	return nil, false
}

// TestExitResolverReservedName 验证保留名 server:self 只归本机出口：未启用时未命中
// 且不回落到远程会话；启用时命中本机；其余名字走远程解析。
func TestExitResolverReservedName(t *testing.T) {
	r := &exitResolver{remote: fakeRemote{e: fakeExit{}}}
	if _, ok := r.Resolve(transport.LocalClientName); ok {
		t.Fatal("reserved name must miss when local exit disabled")
	}

	r.SetLocal(&localExit{})
	if got, ok := r.Resolve(transport.LocalClientName); !ok || got == nil {
		t.Fatal("reserved name should resolve to local exit when enabled")
	}
	if _, ok := r.Resolve("remote-1"); !ok {
		t.Fatal("non-reserved name should resolve via remote")
	}
	if _, ok := r.Resolve("unknown"); ok {
		t.Fatal("unknown name should not resolve")
	}
}
