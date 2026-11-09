package tcp

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

type drainingRecorder struct {
	*eventRecorder
	interrupted chan struct{}
	release     chan struct{}
}

func (r *drainingRecorder) OnStreamOpen(_ *SessionInfo, stream Stream) error {
	r.events <- "stream.open"
	_, _ = io.Copy(io.Discard, stream)
	close(r.interrupted)
	<-r.release
	return nil
}

func TestDrainWaitsForStreamBeforeSessionClose(t *testing.T) {
	rec := &drainingRecorder{eventRecorder: newEventRecorder(), interrupted: make(chan struct{}), release: make(chan struct{})}
	srv := NewServer("127.0.0.1:0", rec)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	client := dialClient(t, YamuxMuxer(), srv.Addr())
	rec.expect(t, "session.open")
	if _, err := client.OpenStream(); err != nil {
		t.Fatal(err)
	}
	rec.expect(t, "stream.open")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Drain(ctx) }()
	select {
	case <-rec.interrupted:
	case <-ctx.Done():
		t.Fatal("drain did not interrupt stream read")
	}
	select {
	case err := <-done:
		t.Fatalf("drain returned before handler finished: %v", err)
	default:
	}
	close(rec.release)
	rec.expect(t, "stream.close")
	rec.expect(t, "session.close")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDrainClosesUnauthenticatedConnection(t *testing.T) {
	entered := make(chan struct{})
	srv := NewServer("127.0.0.1:0", newEventRecorder(), WithConnectionHandshake(func(_ context.Context, conn net.Conn) (net.Conn, error) {
		close(entered)
		_, err := io.Copy(io.Discard, conn)
		return nil, err
	}))
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handshake did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Drain(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestStopPreservesEstablishedSession(t *testing.T) {
	rec := newEventRecorder()
	srv := NewServer("127.0.0.1:0", rec)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client := dialClient(t, YamuxMuxer(), srv.Addr())
	rec.expect(t, "session.open")
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-srv.Done():
		t.Fatal("Stop completed while session is still active")
	default:
	}
	stream, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	rec.expect(t, "stream.open")
	_ = stream.Close()
	rec.expect(t, "stream.close")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-srv.Done():
	default:
		t.Fatal("Drain returned before Done")
	}
}

func TestDoneAfterStoppedSessionFinishes(t *testing.T) {
	rec := newEventRecorder()
	srv := NewServer("127.0.0.1:0", rec)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client := dialClient(t, YamuxMuxer(), srv.Addr())
	rec.expect(t, "session.open")
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-srv.Done():
		t.Fatal("Done closed with active session")
	default:
	}
	_ = client.Close()
	select {
	case <-srv.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done did not close after session finished")
	}
}

type contextRecorder struct {
	*eventRecorder
	entered chan context.Context
}

func (r *contextRecorder) OnStreamOpen(info *SessionInfo, _ Stream) error {
	r.entered <- info.Context
	<-info.Context.Done()
	return nil
}

func TestParentCancelReachesStoppedServerSessions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &contextRecorder{eventRecorder: newEventRecorder(), entered: make(chan context.Context, 1)}
	srv := NewServer("127.0.0.1:0", rec)
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	client := dialClient(t, YamuxMuxer(), srv.Addr())
	rec.expect(t, "session.open")
	if _, err := client.OpenStream(); err != nil {
		t.Fatal(err)
	}
	var streamCtx context.Context
	select {
	case streamCtx = <-rec.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("stream handler did not start")
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if streamCtx.Err() != nil {
		t.Fatal("Stop canceled an established session")
	}
	cancel()
	select {
	case <-srv.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("parent cancellation did not finish stopped server")
	}
	if streamCtx.Err() == nil {
		t.Fatal("stream did not inherit parent cancellation")
	}
	rec.expect(t, "stream.close")
	rec.expect(t, "session.close")
}
