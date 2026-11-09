package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/tcp"
)

type configPushSession struct {
	tcp.Session
	closed chan struct{}
	open   func(context.Context) (tcp.Stream, error)
}

func (s *configPushSession) CloseChan() <-chan struct{} { return s.closed }

func (s *configPushSession) OpenStreamContext(ctx context.Context) (tcp.Stream, error) {
	return s.open(ctx)
}

func emptyPushConfig() protocol.PushConfig { return protocol.PushConfig{} }

func waitPushError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("configuration push did not stop")
		return nil
	}
}

func TestSendPushConfigCancelWhileQueued(t *testing.T) {
	backend := &configPushSession{closed: make(chan struct{})}
	s := NewSession(backend, "test", protocol.ClientInfo{})
	s.configGate <- struct{}{}
	defer func() { <-s.configGate }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.SendPushConfigContext(ctx, emptyPushConfig) }()
	cancel()
	if err := waitPushError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel queued push = %v", err)
	}
}

func TestSendPushConfigCancelOpeningStream(t *testing.T) {
	opened := make(chan struct{})
	backend := &configPushSession{closed: make(chan struct{}), open: func(ctx context.Context) (tcp.Stream, error) {
		close(opened)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	s := NewSession(backend, "test", protocol.ClientInfo{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.SendPushConfigContext(ctx, emptyPushConfig) }()
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("stream opening did not start")
	}
	cancel()
	if err := waitPushError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel stream opening = %v", err)
	}
}

type configPushStream struct {
	net.Conn
	writing chan struct{}
	once    sync.Once
}

func (s *configPushStream) ID() uint32 { return 1 }

func (s *configPushStream) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.writing) })
	return s.Conn.Write(p)
}

func TestSendPushConfigCancelWriting(t *testing.T) {
	left, right := net.Pipe()
	defer func() { _ = left.Close(); _ = right.Close() }()
	stream := &configPushStream{Conn: left, writing: make(chan struct{})}
	backend := &configPushSession{closed: make(chan struct{}), open: func(context.Context) (tcp.Stream, error) { return stream, nil }}
	s := NewSession(backend, "test", protocol.ClientInfo{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.SendPushConfigContext(ctx, emptyPushConfig) }()
	select {
	case <-stream.writing:
	case <-time.After(2 * time.Second):
		t.Fatal("stream write did not start")
	}
	cancel()
	if err := waitPushError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel stream write = %v", err)
	}
	if len(s.configGate) != 0 {
		t.Fatal("canceled push retained configuration gate")
	}
}

func TestSendPushConfigDefaultBoundsOpenStream(t *testing.T) {
	backend := &configPushSession{closed: make(chan struct{}), open: func(ctx context.Context) (tcp.Stream, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > controlStreamTimeout {
			t.Fatal("default push did not bound stream opening")
		}
		return nil, io.ErrClosedPipe
	}}
	s := NewSession(backend, "test", protocol.ClientInfo{})
	if err := s.SendPushConfig(emptyPushConfig); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("failed stream opening = %v", err)
	}
}
