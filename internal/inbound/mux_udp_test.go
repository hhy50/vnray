package inbound

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/access"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/identity"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/mux"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/vmess"
)

func TestMuxUDPRouteCancellation(t *testing.T) {
	for _, reason := range []string{"route-close", "parent-cancel"} {
		t.Run(reason, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, stop := context.WithCancel(parent)
			defer stop()
			route := &muxUDPRoute{ctx: ctx, cancel: stop, packets: make(chan []byte, 1)}
			siblingCtx, stopSibling := context.WithCancel(parent)
			defer stopSibling()
			read := make(chan error, 1)
			go func() { _, err := route.ReadPacket(); read <- err }()
			if reason == "route-close" {
				_ = route.Close()
				_ = route.Close()
				if siblingCtx.Err() != nil {
					t.Fatal("closing route canceled a sibling")
				}
			} else {
				cancel()
			}
			select {
			case err := <-read:
				if !errors.Is(err, io.EOF) {
					t.Fatalf("canceled read = %v, want EOF", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("route cancellation left read blocked")
			}
			if !route.closed() {
				t.Fatal("canceled route remains open")
			}
		})
	}
}

type xudpTunnelRequest struct {
	dest dispatch.Destination
	req  protocol.TunnelRequest
}

type xudpEchoExit struct {
	mu       sync.Mutex
	requests []xudpTunnelRequest
	wg       sync.WaitGroup
	packets  atomic.Int64
}

func (e *xudpEchoExit) OpenTunnel(dest dispatch.Destination, req protocol.TunnelRequest) (net.Conn, error) {
	e.mu.Lock()
	e.requests = append(e.requests, xudpTunnelRequest{dest: dest, req: req})
	e.mu.Unlock()
	conn, peer := net.Pipe()
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer peer.Close()
		if req.Network != "udp" {
			_, _ = io.Copy(peer, peer)
			return
		}
		d := protocol.NewDatagramConn(peer)
		for {
			data, err := d.ReadPacket()
			if err != nil {
				return
			}
			e.packets.Add(1)
			if err := d.WritePacket(data); err != nil {
				return
			}
		}
	}()
	return conn, nil
}

func (e *xudpEchoExit) snapshot() []xudpTunnelRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]xudpTunnelRequest(nil), e.requests...)
}

func TestMuxUDPPerPacketRulesAndTargets(t *testing.T) {
	for _, proto := range []store.Protocol{store.ProtocolVLESS, store.ProtocolVMess} {
		t.Run(string(proto), func(t *testing.T) {
			st := seedTestStore(t)
			users := access.NewBindings(st)
			user := store.UserBinding{Key: "xudp-user", Protocol: proto, Name: "xudp", Status: store.UserEnabled, Client: transport.LocalClientName, Config: store.UserConfig{Rule: "office"}}
			if err := users.PutUser(context.Background(), user); err != nil {
				t.Fatal(err)
			}
			exit := &xudpEchoExit{}
			conn := startXUDPRuleTest(t, proto, users, exit, user.Key)
			writeXUDPTestFrame(t, conn, 2, mux.StatusNew, mux.NetworkTCP, "allowed.example", []byte("tcp-before"))
			readXUDPTestFrame(t, conn, 2, "", []byte("tcp-before"))
			writeXUDPTestFrame(t, conn, 1, mux.StatusNew, mux.NetworkUDP, "allowed.example", []byte("first"))
			readXUDPTestFrame(t, conn, 1, "allowed.example", []byte("first"))
			writeXUDPTestFrame(t, conn, 1, mux.StatusKeep, mux.NetworkUDP, "10.1.2.3", []byte("direct"))
			readXUDPTestFrame(t, conn, 1, "10.1.2.3", []byte("direct"))
			writeXUDPTestFrame(t, conn, 1, mux.StatusKeep, 0, "", nil)
			readXUDPTestFrame(t, conn, 1, "allowed.example", nil)
			requests := exit.snapshot()
			if len(requests) != 3 || requests[1].req.Direct || !requests[2].req.Direct || requests[2].dest.Host != "10.1.2.3" || requests[2].req.Network != "udp" {
				t.Fatalf("target routing = %+v", requests)
			}
			user.Config.Rule = "deleted"
			if err := users.PutUser(context.Background(), user); err != nil {
				t.Fatal(err)
			}
			writeXUDPTestFrame(t, conn, 1, mux.StatusKeep, mux.NetworkUDP, "10.1.2.3", []byte("pass"))
			readXUDPTestFrame(t, conn, 1, "10.1.2.3", []byte("pass"))
			requests = exit.snapshot()
			if len(requests) != 4 || requests[3].req.Direct {
				t.Fatalf("updated direct verdict reused old tunnel: %+v", requests)
			}
			writeXUDPTestFrame(t, conn, 1, mux.StatusKeep, mux.NetworkUDP, "blocked.example", []byte("initially-pass"))
			readXUDPTestFrame(t, conn, 1, "blocked.example", []byte("initially-pass"))
			user.Config.Rule = "office"
			if err := users.PutUser(context.Background(), user); err != nil {
				t.Fatal(err)
			}
			writeXUDPTestFrame(t, conn, 1, mux.StatusKeep, mux.NetworkUDP, "blocked.example", []byte("denied"))
			var end [6]byte
			if _, err := io.ReadFull(conn, end[:]); err != nil {
				t.Fatal(err)
			}
			if end != [6]byte{0, 4, 0, 1, mux.StatusEnd, mux.OptionError} {
				t.Fatalf("reject response = %x", end)
			}
			if got := exit.snapshot(); len(got) != 5 {
				t.Fatalf("rejected packet opened a tunnel: %+v", got)
			}
			if got := exit.packets.Load(); got != 5 {
				t.Fatalf("forwarded UDP packets = %d, want 5", got)
			}
			writeXUDPTestFrame(t, conn, 2, mux.StatusKeep, 0, "", []byte("tcp-after"))
			readXUDPTestFrame(t, conn, 2, "", []byte("tcp-after"))
		})
	}
}

func TestMuxUDPTargetLimit(t *testing.T) {
	st := seedTestStore(t)
	users := access.NewBindings(st)
	if err := users.PutUser(context.Background(), store.UserBinding{
		Key: "xudp-user", Protocol: store.ProtocolVLESS, Name: "xudp", Status: store.UserEnabled, Client: transport.LocalClientName,
	}); err != nil {
		t.Fatal(err)
	}
	exit := &xudpEchoExit{}
	conn := startXUDPRuleTest(t, store.ProtocolVLESS, users, exit, "xudp-user")
	for i := 0; i < maxMuxUDPTargets; i++ {
		status := byte(mux.StatusKeep)
		if i == 0 {
			status = mux.StatusNew
		}
		host := fmt.Sprintf("host%d.example", i)
		writeXUDPTestFrame(t, conn, 1, status, mux.NetworkUDP, host, []byte("dns"))
		readXUDPTestFrame(t, conn, 1, host, []byte("dns"))
	}
	writeXUDPTestFrame(t, conn, 1, mux.StatusKeep, mux.NetworkUDP, "overflow.example", []byte("denied"))
	var end [6]byte
	if _, err := io.ReadFull(conn, end[:]); err != nil {
		t.Fatal(err)
	}
	if end != [6]byte{0, 4, 0, 1, mux.StatusEnd, mux.OptionError} {
		t.Fatalf("limit response = %x", end)
	}
	if got := exit.snapshot(); len(got) != maxMuxUDPTargets {
		t.Fatalf("opened %d tunnels, want %d", len(got), maxMuxUDPTargets)
	}
}

type xudpBlockingExit struct {
	xudpEchoExit
	entered chan struct{}
}

func (e *xudpBlockingExit) OpenTunnelContext(ctx context.Context, dest dispatch.Destination, req protocol.TunnelRequest) (net.Conn, error) {
	if dest.Host != "pending.example" {
		return e.OpenTunnel(dest, req)
	}
	close(e.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestMuxUDPDisconnectCancelsTunnelOpen(t *testing.T) {
	for _, pendingFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("initial=%v", pendingFirst), func(t *testing.T) {
			exit := &xudpBlockingExit{entered: make(chan struct{})}
			conn, peer := net.Pipe()
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			handler := &muxTunnel{log: logging.NewScoped(""), sess: exit}
			done := make(chan error, 1)
			go func() { done <- mux.NewServer(handler).Serve(peer) }()
			if !pendingFirst {
				writeXUDPTestFrame(t, conn, 1, mux.StatusNew, mux.NetworkUDP, "ready.example", []byte("ready"))
				readXUDPTestFrame(t, conn, 1, "ready.example", []byte("ready"))
			}
			status := byte(mux.StatusKeep)
			if pendingFirst {
				status = mux.StatusNew
			}
			writeXUDPTestFrame(t, conn, 1, status, mux.NetworkUDP, "pending.example", []byte("pending"))
			select {
			case <-exit.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("UDP tunnel open did not start")
			}
			_ = conn.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("mux disconnect did not cancel UDP tunnel open")
			}
			exit.wg.Wait()
		})
	}
}

func startXUDPRuleTest(t *testing.T, proto store.Protocol, users UserStore, exit *xudpEchoExit, key string) net.Conn {
	t.Helper()
	conn, peer := net.Pipe()
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	p := NewV2RayPlugin(seedTestStore(t), users, &stubExitResolver{local: exit}, nil)
	p.SetRules(inboundTestRules(t))
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.serveConn(p.runtime.Context(), proto, identity.Identity{Key: key, Client: transport.LocalClientName}, vmess.MuxCoolHost, 0, "tcp", conn)
	}()
	t.Cleanup(func() {
		_ = peer.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("mux handler did not stop")
			return
		}
		peersDone := make(chan struct{})
		go func() { exit.wg.Wait(); close(peersDone) }()
		select {
		case <-peersDone:
		case <-time.After(3 * time.Second):
			t.Error("UDP tunnels not closed on mux shutdown")
		}
		drainPlugin(t, p)
	})
	return peer
}

func writeXUDPTestFrame(t *testing.T, conn net.Conn, id, status, network byte, host string, data []byte) {
	t.Helper()
	meta := []byte{0, id, status, mux.OptionData}
	if host != "" {
		meta = append(meta, network, 0, 53, 2, byte(len(host)))
		meta = append(meta, host...)
	}
	if status == mux.StatusNew && network == mux.NetworkUDP {
		meta = append(meta, 1, 2, 3, 4, 5, 6, 7, 8)
	}
	raw := binary.BigEndian.AppendUint16(nil, uint16(len(meta)))
	raw = append(raw, meta...)
	raw = binary.BigEndian.AppendUint16(raw, uint16(len(data)))
	raw = append(raw, data...)
	if _, err := conn.Write(raw); err != nil {
		t.Fatal(err)
	}
}

func readXUDPTestFrame(t *testing.T, conn net.Conn, id byte, host string, data []byte) {
	t.Helper()
	var length [2]byte
	if _, err := io.ReadFull(conn, length[:]); err != nil {
		t.Fatal(err)
	}
	meta := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, err := io.ReadFull(conn, meta); err != nil {
		t.Fatal(err)
	}
	want := []byte{0, id, mux.StatusKeep, mux.OptionData}
	if host != "" {
		want = append(want, mux.NetworkUDP, 0, 53)
		if ip := net.ParseIP(host).To4(); ip != nil {
			want = append(want, 1)
			want = append(want, ip...)
		} else {
			want = append(want, 2, byte(len(host)))
			want = append(want, host...)
		}
	}
	if !bytes.Equal(meta, want) {
		t.Fatalf("response metadata = %x, want %x", meta, want)
	}
	if _, err := io.ReadFull(conn, length[:]); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("response payload = %x, want %x", got, data)
	}
}
