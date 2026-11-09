package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/tcp"
)

type pendingStream struct {
	net.Conn
	ctx     context.Context
	entered chan struct{}
	once    sync.Once
}

func (s *pendingStream) ID() uint32 { return 1 }

func (s *pendingStream) SetDeadline(deadline time.Time) error {
	s.once.Do(func() {
		close(s.entered)
		<-s.ctx.Done()
	})
	return s.Conn.SetDeadline(deadline)
}

type pendingSession struct {
	tcp.Session
	stream tcp.Stream
}

func (s *pendingSession) CloseChan() <-chan struct{} { return nil }

func (s *pendingSession) OpenStreamContext(context.Context) (tcp.Stream, error) {
	return s.stream, nil
}

func TestOpenTunnelCancelBeforeStreamPublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	stream := &pendingStream{Conn: conn, ctx: ctx, entered: make(chan struct{})}
	session := NewSession(&pendingSession{stream: stream}, "test", protocol.ClientInfo{})
	done := make(chan error, 1)
	go func() {
		_, err := session.OpenTunnelContext(ctx, dispatch.Destination{Host: "127.0.0.1", Port: 80}, protocol.TunnelRequest{})
		done <- err
	}()
	select {
	case <-stream.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not reach publication boundary")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("open error = %v, want canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled tunnel did not finish")
	}
	_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("pending stream was not closed: %v", err)
	}
	if got := session.gate.Usage(); got != 0 {
		t.Fatalf("canceled tunnel retained %d slots", got)
	}
}
