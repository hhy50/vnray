package vless

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/identity"
	"github.com/hhy50/vnray/vmess"
)

func TestServerDoneWaitsForHandler(t *testing.T) {
	validator := NewValidator()
	if err := validator.Add(testUUID, "client-a"); err != nil {
		t.Fatal(err)
	}
	entered, interrupted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	srv := NewServer(validator, func(_ identity.Identity, _ Target, _ string, conn net.Conn) {
		close(entered)
		_, _ = io.Copy(conn, conn)
		close(interrupted)
		<-release
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		finish()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Drain(ctx); err != nil {
			t.Error(err)
		}
	})
	served := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(served) }()
	raw, _ := vmess.ParseUUID(testUUID)
	conn, _, err := dialRequest(t, ln.Addr().String(), buildRequest(raw, CommandTCP, "example.com", 443, 0))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	_ = ln.Close()
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("listener did not stop")
	}
	select {
	case <-srv.Done():
		t.Fatal("Done closed with a live connection")
	default:
	}
	if _, err := conn.Write([]byte("alive")); err != nil {
		t.Fatal(err)
	}
	var echo [5]byte
	if _, err := io.ReadFull(conn, echo[:]); err != nil || string(echo[:]) != "alive" {
		t.Fatalf("stopping listener interrupted connection: %v", err)
	}
	_ = conn.Close()
	select {
	case <-interrupted:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not observe EOF")
	}
	select {
	case <-srv.Done():
		t.Fatal("Done closed before handler returned")
	default:
	}
	finish()
	select {
	case <-srv.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done did not close")
	}
}

func TestServerDoneBeforeServe(t *testing.T) {
	srv := NewServer(NewValidator(), nil)
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-srv.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("unstarted server did not finish")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(ln); err != nil {
		t.Fatal(err)
	}
	if conn, err := ln.Accept(); err == nil {
		_ = conn.Close()
		t.Fatal("stopped server kept listener open")
	}
}
