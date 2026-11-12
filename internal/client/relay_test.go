package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/acl"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/pipe"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/tcp"
)

func newTestRelayStreams(t *testing.T) (tcp.Stream, tcp.Stream) {
	t.Helper()
	a, b := net.Pipe()
	muxer := tcp.YamuxMuxer()
	left, err := muxer.Client(a, tcp.DefaultSessionConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = left.Close() })
	right, err := muxer.Server(b, tcp.DefaultSessionConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = right.Close() })
	out, err := left.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	in, err := right.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	_ = out.SetDeadline(time.Now().Add(5 * time.Second))
	_ = in.SetDeadline(time.Now().Add(5 * time.Second))
	return out, in
}

func newTestTCPPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conn, err := net.DialTCP("tcp", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	peer, err := ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	return conn, peer
}

func waitRelay(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay left a pump blocked")
	}
}

func TestRelayStreamHalfClose(t *testing.T) {
	for _, upstreamEOF := range []bool{true, false} {
		name := "downstream-first"
		if upstreamEOF {
			name = "request-eof"
		}
		t.Run(name, func(t *testing.T) {
			peer, stream := newTestRelayStreams(t)
			outbound, target := newTestTCPPair(t)
			done := make(chan struct{})
			go func() {
				relayStream(context.Background(), stream, outbound, false)
				close(done)
			}()
			if upstreamEOF {
				if _, err := peer.Write([]byte("request")); err != nil {
					t.Fatal(err)
				}
				if err := peer.Close(); err != nil {
					t.Fatal(err)
				}
				request, err := io.ReadAll(target)
				if err != nil || string(request) != "request" {
					t.Fatalf("request=%q, err=%v", request, err)
				}
			}
			response := bytes.Repeat([]byte("response"), 128*1024)
			sent := make(chan error, 1)
			go func() {
				_, err := target.Write(response)
				_ = target.CloseWrite()
				sent <- err
			}()
			got, err := io.ReadAll(peer)
			if err != nil || !bytes.Equal(got, response) {
				t.Fatalf("response bytes=%d, want=%d, err=%v", len(got), len(response), err)
			}
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
			waitRelay(t, done)
		})
	}
}

type observedWriteConn struct {
	net.Conn
	entered chan struct{}
	fail    bool
	panics  bool
}

func (c observedWriteConn) Write(p []byte) (int, error) {
	if c.entered != nil {
		select {
		case c.entered <- struct{}{}:
		default:
		}
	}
	if c.panics {
		panic("test relay write")
	}
	if c.fail {
		return 0, io.ErrUnexpectedEOF
	}
	return c.Conn.Write(p)
}

func TestRelayStreamShutdown(t *testing.T) {
	for _, mode := range []string{"cancel-read", "cancel-up-write", "cancel-down-write", "up-error", "up-panic", "udp-eof"} {
		t.Run(mode, func(t *testing.T) {
			stream, peer := net.Pipe()
			outbound, target := net.Pipe()
			t.Cleanup(func() {
				_ = stream.Close()
				_ = peer.Close()
				_ = outbound.Close()
				_ = target.Close()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{}, 1)
			up := observedWriteConn{Conn: outbound, entered: entered, fail: mode == "up-error", panics: mode == "up-panic"}
			down := observedWriteConn{Conn: stream}
			if mode == "cancel-down-write" {
				down.entered = entered
			}
			done := make(chan struct{})
			go func() {
				relayStream(ctx, lifecycleStream{down}, up, mode == "udp-eof")
				close(done)
			}()
			switch mode {
			case "cancel-read":
				cancel()
			case "udp-eof":
				_ = peer.Close()
			default:
				writer := peer
				if mode == "cancel-down-write" {
					writer = target
				}
				_ = writer.SetWriteDeadline(time.Now().Add(3 * time.Second))
				if _, err := writer.Write([]byte("data")); err != nil {
					t.Fatal(err)
				}
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("pump did not reach write")
				}
				if strings.HasPrefix(mode, "cancel") {
					cancel()
				}
			}
			waitRelay(t, done)
		})
	}
}

type relayDialerFunc func(context.Context, string, string) (net.Conn, error)

func (f relayDialerFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

func TestOpenOutboundPolicy(t *testing.T) {
	for _, mode := range []string{"direct", "hosts", "ip-map", "acl", "reject", "missing", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			local, peer := net.Pipe()
			defer local.Close()
			defer peer.Close()
			called := false
			const address = "127.0.0.1:8080"
			direct := dispatch.NewDirectOutboundWithDialer("direct", relayDialerFunc(func(ctx context.Context, network, got string) (net.Conn, error) {
				called = true
				if got != address || network != "tcp" {
					t.Errorf("dial %s %s, want tcp %s", network, got, address)
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > targetDialTimeout {
					t.Error("dial has no bounded deadline")
				}
				return local, nil
			}))
			rules := []dispatch.Rule{{Name: "reject", Tag: "reject", Match: func(dispatch.Destination) bool { return mode == "reject" }}}
			defaultTag := "direct"
			if mode == "missing" {
				defaultTag = "missing"
			}
			d := dispatch.NewDefaultDispatcher(rules, defaultTag, direct, dispatch.NewRejectOutbound("reject", "blocked by test"))
			dest := dispatch.Destination{Host: "127.0.0.1", Port: 8080}
			if mode == "hosts" {
				dest.Host = "target.test"
				hosts := dispatch.NewHosts()
				hosts.Update([]protocol.CustomHost{{Domain: dest.Host, IP: "127.0.0.1"}})
				d.SetHosts(hosts)
			}
			if mode == "ip-map" {
				dest.Host = "192.0.2.1"
				maps := dispatch.NewIPMaps()
				maps.Update([]protocol.IPMap{{From: dest.Host, To: "127.0.0.1"}})
				d.SetIPMaps(maps)
			}
			var dispatcher dispatch.Dispatcher = d
			if mode == "acl" {
				guard, err := acl.NewGuard("", "127.0.0.0/8")
				if err != nil {
					t.Fatal(err)
				}
				dispatcher = guard.Wrap(d)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			conn, err := openOutbound(ctx, dispatcher, dest)
			switch mode {
			case "acl", "missing", "canceled":
				if err == nil || called {
					t.Fatalf("err=%v, dialed=%v", err, called)
				}
			case "reject":
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				_, err := conn.Read(make([]byte, 1))
				var rejected *dispatch.OutboundError
				if !errors.As(err, &rejected) || rejected.Reason != "blocked by test" || called {
					t.Fatalf("reject=%v, dialed=%v", err, called)
				}
			default:
				if err != nil || !called || conn != local {
					t.Fatalf("conn=%T, err=%v, dialed=%v", conn, err, called)
				}
			}
		})
	}
}

func TestClientTunnelDialLifecycle(t *testing.T) {
	for _, mode := range []string{"dial-failure", "cancel-dial", "cancel-reply", "direct-flag", "direct-acl"} {
		t.Run(mode, func(t *testing.T) {
			local, target := net.Pipe()
			defer local.Close()
			defer target.Close()
			entered := make(chan struct{}, 1)
			direct := dispatch.NewDirectOutboundWithDialer("direct", relayDialerFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
				entered <- struct{}{}
				switch mode {
				case "dial-failure":
					return nil, errors.New("test dial failure")
				case "cancel-dial":
					<-ctx.Done()
					return nil, ctx.Err()
				default:
					return local, nil
				}
			}))
			d := dispatch.NewDefaultDispatcher(nil, "direct", direct)
			c, peer := newTestLifecycleClient(t, d)
			if strings.HasPrefix(mode, "direct-") {
				c.dispatcher = dispatch.NewDefaultDispatcher(nil, "reject", dispatch.NewRejectOutbound("reject", "normal route"))
				c.direct = d
				if mode == "direct-acl" {
					guard, err := acl.NewGuard("", "127.0.0.0/8")
					if err != nil {
						t.Fatal(err)
					}
					c.direct = guard.Wrap(d)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				_ = c.ServeContext(ctx)
				close(done)
			}()
			frame, err := protocol.NewTunnelFrame(protocol.TunnelRequest{Direct: strings.HasPrefix(mode, "direct-")}, protocol.Address{Host: "127.0.0.1", Port: 8080})
			if err != nil {
				t.Fatal(err)
			}
			if err := protocol.NewEncoder(peer).WriteFrame(frame); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(mode, "cancel-") {
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("dial did not start")
				}
			} else {
				frame, err := protocol.NewDecoder(peer).ReadFrame()
				if err != nil {
					t.Fatal(err)
				}
				payload, err := protocol.ReadPayload(frame)
				if err != nil {
					t.Fatal(err)
				}
				reply, err := protocol.ParseReply(payload)
				if err != nil || reply.OK != (mode == "direct-flag") {
					t.Fatalf("reply=%+v, err=%v", reply, err)
				}
				if mode == "dial-failure" && !strings.Contains(reply.Reason, "test dial failure") {
					t.Fatalf("dial failure reason=%q", reply.Reason)
				}
				if mode == "direct-acl" {
					if !strings.Contains(reply.Reason, "acl") {
						t.Fatalf("ACL reason=%q", reply.Reason)
					}
					select {
					case <-entered:
						t.Fatal("direct flag bypassed ACL")
					default:
					}
				}
			}
			cancel()
			waitRelay(t, done)
			if mode == "cancel-reply" || mode == "direct-flag" {
				_ = target.SetReadDeadline(time.Now().Add(3 * time.Second))
				if _, err := target.Read(make([]byte, 1)); err != io.EOF {
					t.Fatalf("target connection was not closed: %v", err)
				}
			}
		})
	}
}

func TestClientTunnelUDP(t *testing.T) {
	socket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	_ = socket.SetDeadline(time.Now().Add(3 * time.Second))
	echo := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		n, addr, err := socket.ReadFromUDP(buf)
		if err == nil {
			_, err = socket.WriteToUDP(buf[:n], addr)
		}
		echo <- err
	}()
	d := dispatch.NewDefaultDispatcher(nil, "direct", dispatch.NewDirectOutbound("direct"))
	c, peer := newTestLifecycleClient(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = c.ServeContext(ctx); close(done) }()
	frame, err := protocol.NewTunnelFrame(protocol.TunnelRequest{Network: "udp"}, protocol.Address{Host: "127.0.0.1", Port: uint16(socket.LocalAddr().(*net.UDPAddr).Port)})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewEncoder(peer).WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	reply, err := protocol.NewDecoder(peer).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := protocol.ReadPayload(reply)
	if err != nil {
		t.Fatal(err)
	}
	result, err := protocol.ParseReply(payload)
	if err != nil || !result.OK {
		t.Fatalf("reply=%+v, err=%v", result, err)
	}
	packets := protocol.NewDatagramConn(peer)
	if err := packets.WritePacket([]byte("udp")); err != nil {
		t.Fatal(err)
	}
	packet, err := packets.ReadPacket()
	if err != nil || string(packet) != "udp" {
		t.Fatalf("packet=%q, err=%v", packet, err)
	}
	if err := <-echo; err != nil {
		t.Fatal(err)
	}
	cancel()
	waitRelay(t, done)
}

func TestClientTunnelGoroutineBudget(t *testing.T) {
	local, target := net.Pipe()
	defer local.Close()
	defer target.Close()
	_ = target.SetDeadline(time.Now().Add(3 * time.Second))
	dialStack := make(chan string, 1)
	d := dispatch.NewDefaultDispatcher(nil, "direct", dispatch.NewDirectOutboundWithDialer("direct", relayDialerFunc(func(context.Context, string, string) (net.Conn, error) {
		buf := make([]byte, 64*1024)
		dialStack <- string(buf[:runtime.Stack(buf, false)])
		return local, nil
	})))
	guard, err := acl.NewGuard("127.0.0.0/8", "")
	if err != nil {
		t.Fatal(err)
	}
	c, peer := newTestLifecycleClient(t, guard.Wrap(d))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = c.ServeContext(ctx); close(done) }()
	before, _ := pipe.BufferAllocations()
	frame, err := protocol.NewTunnelFrame(protocol.TunnelRequest{}, protocol.Address{Host: "127.0.0.1", Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewEncoder(peer).WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	result, err := protocol.NewDecoder(peer).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if err := result.CloseBody(); err != nil {
		t.Fatal(err)
	}
	if stack := <-dialStack; !strings.Contains(stack, "client.(*Client).handleStream(") {
		t.Fatalf("dial was not owned by stream handler:\n%s", stack)
	}
	exchanged := make(chan error, 1)
	go func() {
		buf := make([]byte, 4)
		_, err := io.ReadFull(target, buf)
		if err == nil {
			_, err = target.Write(buf)
		}
		exchanged <- err
	}()
	if _, err := peer.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(peer, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	if err := <-exchanged; err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4<<20)
	n := runtime.Stack(buf, true)
	if n == len(buf) {
		t.Fatal("stack dump truncated")
	}
	dump := string(buf[:n])
	for signature, want := range map[string]int{
		"client.relayStream(":                           1,
		"client.relayStream.func":                       1,
		"dispatch.(*DefaultDispatcher).routedDispatch(": 0,
		"dispatch.(*DirectOutbound).processTCP(":        0,
	} {
		got := 0
		for _, stack := range strings.Split(dump, "\n\n") {
			if strings.Contains(stack, signature) {
				got++
			}
		}
		if got != want {
			t.Errorf("%s: %d goroutines, want %d", signature, got, want)
		}
	}
	if after, _ := pipe.BufferAllocations(); after != before {
		t.Errorf("direct relay allocated %d pipe buffers", after-before)
	}
	cancel()
	waitRelay(t, done)
}
