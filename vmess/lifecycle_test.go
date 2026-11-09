package vmess

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/identity"
)

func TestServerDrainWaitsForHandler(t *testing.T) {
	validator := NewValidator()
	if err := validator.Add(testUUID, "client-a"); err != nil {
		t.Fatal(err)
	}
	entered, interrupted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	srv := NewServer(validator, func(_ identity.Identity, _ Target, _ string, conn *Conn) {
		close(entered)
		_, _ = io.Copy(io.Discard, conn)
		close(interrupted)
		<-release
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client, err := NewClientSession(testUUID, conn, Target{Host: "example.com", Port: 443}, SecurityAES128GCM, OptionChunkStream)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ReadResponseHeader(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Drain(ctx) }()
	select {
	case <-interrupted:
	case <-ctx.Done():
		t.Fatal("drain did not interrupt handler")
	}
	select {
	case err := <-done:
		t.Fatalf("drain returned before handler: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestServerDoneAfterStoppedHandlerFinishes(t *testing.T) {
	validator := NewValidator()
	if err := validator.Add(testUUID, "client-a"); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	srv := NewServer(validator, func(_ identity.Identity, _ Target, _ string, _ *Conn) {
		close(entered)
		<-release
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client, err := NewClientSession(testUUID, conn, Target{Host: "example.com", Port: 443}, SecurityAES128GCM, OptionChunkStream)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ReadResponseHeader(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-srv.Done():
		t.Fatal("Done closed with active handler")
	default:
	}
	unblock()
	select {
	case <-srv.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done did not close after handler finished")
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}
