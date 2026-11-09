package client

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/tcp"
)

type lifecycleStream struct{ net.Conn }

func (lifecycleStream) ID() uint32 { return 1 }

type lifecycleSession struct {
	tcp.Session
	stream tcp.Stream
	ready  chan tcp.Stream
	done   chan struct{}
	once   sync.Once
}

func (s *lifecycleSession) AcceptStream() (tcp.Stream, error) {
	select {
	case stream := <-s.ready:
		return stream, nil
	case <-s.done:
		return nil, io.EOF
	}
}

func (s *lifecycleSession) CloseChan() <-chan struct{} { return s.done }
func (s *lifecycleSession) IsClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}
func (s *lifecycleSession) Close() error {
	s.once.Do(func() { close(s.done); _ = s.stream.Close() })
	return nil
}

type pendingDispatcher struct{ entered chan context.Context }

func (d *pendingDispatcher) Dispatch(ctx context.Context, _ dispatch.Destination) (*dispatch.Link, error) {
	d.entered <- ctx
	<-ctx.Done()
	return nil, ctx.Err()
}

func newTestLifecycleClient(t *testing.T, dispatcher dispatch.Dispatcher) (*Client, net.Conn) {
	t.Helper()
	stream, peer := net.Pipe()
	session := &lifecycleSession{stream: lifecycleStream{stream}, ready: make(chan tcp.Stream, 1), done: make(chan struct{})}
	session.ready <- session.stream
	t.Cleanup(func() { _ = session.Close(); _ = peer.Close() })
	_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
	return &Client{session: session, dispatcher: dispatcher}, peer
}

func TestServeContextCancelsPendingDispatch(t *testing.T) {
	for _, parentCancel := range []bool{true, false} {
		name := "session"
		if parentCancel {
			name = "parent"
		}
		t.Run(name, func(t *testing.T) {
			dispatcher := &pendingDispatcher{entered: make(chan context.Context, 1)}
			client, peer := newTestLifecycleClient(t, dispatcher)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- client.ServeContext(ctx) }()
			frame, err := protocol.NewTunnelFrame(protocol.TunnelRequest{}, protocol.Address{Host: "pending.test", Port: 80})
			if err != nil {
				t.Fatal(err)
			}
			if err := protocol.NewEncoder(peer).WriteFrame(frame); err != nil {
				t.Fatal(err)
			}
			var opening context.Context
			select {
			case opening = <-dispatcher.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("dispatch did not start")
			}
			if parentCancel {
				cancel()
			} else {
				_ = client.Close()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("ServeContext did not cancel pending dispatch")
			}
			if opening.Err() == nil {
				t.Fatal("stream context remained active")
			}
		})
	}
}

func TestServeContextCancelsPortMapLocalRead(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		accepted <- conn
	}()
	client, peer := newTestLifecycleClient(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.ServeContext(ctx) }()
	frame, err := protocol.NewPortMapFrame(protocol.Address{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)})
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
		t.Fatalf("portmap reply = %+v, %v", result, err)
	}
	var local net.Conn
	select {
	case local = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("local connection did not arrive")
	}
	if local == nil {
		t.Fatal("local accept failed")
	}
	defer local.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("portmap blocked on local read after parent cancellation")
	}
}
