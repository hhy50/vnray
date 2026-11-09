package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/concurrency"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/tcp"
)

func newLifecycleSessions(t *testing.T) (tcp.Session, tcp.Session) {
	t.Helper()
	a, b := net.Pipe()
	muxer := tcp.YamuxMuxer()
	left, err := muxer.Client(a, tcp.DefaultSessionConfig())
	if err != nil {
		t.Fatal(err)
	}
	right, err := muxer.Server(b, tcp.DefaultSessionConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })
	return left, right
}

func newLifecycleStreams(t *testing.T) (tcp.Stream, tcp.Stream) {
	t.Helper()
	left, right := newLifecycleSessions(t)
	out, err := left.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	in, err := right.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	return out, in
}

func TestProxyYamuxHalfCloseRetainsConcurrency(t *testing.T) {
	stream, peer := newLifecycleStreams(t)
	gate := concurrency.NewGate()
	gate.SetMax(1)
	if err := gate.Acquire(nil); err != nil {
		t.Fatal(err)
	}
	tunnel := &gateStream{Stream: stream, release: gate.Release}
	response := &limitedBuffer{}
	user := rwCloser{Reader: strings.NewReader("request"), Writer: response, Closer: nopCloser{}}
	done := make(chan struct{})
	go func() { ProxyBidirectional(user, tunnel, "test.halfclose"); close(done) }()
	_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
	request, err := io.ReadAll(peer)
	if err != nil || string(request) != "request" {
		t.Fatalf("request=%q, err=%v", request, err)
	}
	if got := gate.Usage(); got != 1 {
		t.Fatalf("half-close released concurrency: %d", got)
	}
	if _, err := peer.Write([]byte("response")); err != nil {
		t.Fatal(err)
	}
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not finish")
	}
	if string(response.data) != "response" {
		t.Fatalf("response=%q", response.data)
	}
	if got := gate.Usage(); got != 0 {
		t.Fatalf("completed relay retained concurrency: %d", got)
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestProxyYamuxAbortInterruptsSilentRead(t *testing.T) {
	stream, _ := newLifecycleStreams(t)
	gate := concurrency.NewGate()
	if err := gate.Acquire(nil); err != nil {
		t.Fatal(err)
	}
	tunnel := &gateStream{Stream: stream, release: gate.Release}
	user := rwCloser{Reader: failedReader{}, Writer: io.Discard, Closer: nopCloser{}}
	done := make(chan struct{})
	go func() { ProxyBidirectional(user, tunnel, "test.abort"); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("abort left yamux read blocked")
	}
	if got := gate.Usage(); got != 0 {
		t.Fatalf("aborted relay retained concurrency: %d", got)
	}
}

func TestOpenTunnelContextCancelsQueue(t *testing.T) {
	left, _ := newLifecycleSessions(t)
	sess := NewSession(left, "test", protocol.ClientInfo{})
	sess.SetClientConcurrency(1)
	if err := sess.gate.Acquire(nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sess.OpenTunnelContext(ctx, dispatch.Destination{Host: "127.0.0.1", Port: 80}, protocol.TunnelRequest{})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v, want canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled queue did not return")
	}
	if got := sess.gate.Usage(); got != 1 {
		t.Fatalf("queue changed active slots: %d", got)
	}
}

func TestOpenTunnelContextCancelsHandshake(t *testing.T) {
	left, right := newLifecycleSessions(t)
	sess := NewSession(left, "test", protocol.ClientInfo{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := sess.OpenTunnelContext(ctx, dispatch.Destination{Host: "127.0.0.1", Port: 80}, protocol.TunnelRequest{})
		done <- err
	}()
	peer, err := right.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	frame, err := protocol.NewDecoder(peer).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if err := frame.CloseBody(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled handshake succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled handshake did not return")
	}
	if got := sess.gate.Usage(); got != 0 {
		t.Fatalf("canceled handshake retained slots: %d", got)
	}
}

func TestClientsSortsOnlyOnlineEntries(t *testing.T) {
	m := NewSessionManager()
	m.byID[9] = &sessionEntry{id: 9}
	m.byID[2] = &sessionEntry{id: 2}
	clients := m.Clients()
	if len(clients) != 2 || clients[0].ID != 2 || clients[1].ID != 9 {
		t.Fatalf("clients=%+v", clients)
	}
}

func TestProxyContextCancelsReadsAndLimiters(t *testing.T) {
	for _, network := range []string{"tcp", "udp"} {
		for _, limited := range []bool{false, true} {
			name := network + "/read"
			if limited {
				name = network + "/limiter"
			}
			t.Run(name, func(t *testing.T) {
				a, aPeer := net.Pipe()
				b, bPeer := net.Pipe()
				defer aPeer.Close()
				defer bPeer.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				up := &blockingLimiter{ready: make(chan struct{})}
				down := &blockingLimiter{ready: make(chan struct{})}
				opts := ProxyOptions{Context: ctx}
				if limited {
					opts.UpLimit, opts.DownLimit = up, down
				}
				done := make(chan struct{})
				go func() {
					if network == "udp" {
						ProxyDatagramBidirectional(protocol.NewDatagramConn(a), protocol.NewDatagramConn(b), "test.context", 0, opts)
					} else {
						ProxyBidirectional(a, b, "test.context", opts)
					}
					close(done)
				}()
				if limited {
					for _, peer := range []net.Conn{aPeer, bPeer} {
						_ = peer.SetWriteDeadline(time.Now().Add(3 * time.Second))
						var err error
						if network == "udp" {
							err = protocol.NewDatagramConn(peer).WritePacket([]byte("data"))
						} else {
							_, err = peer.Write([]byte("data"))
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					for _, ready := range []chan struct{}{up.ready, down.ready} {
						select {
						case <-ready:
						case <-time.After(3 * time.Second):
							t.Fatal("limiter did not start")
						}
					}
				}
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("context cancellation left relay blocked")
				}
			})
		}
	}
}
