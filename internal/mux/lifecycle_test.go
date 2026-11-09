package mux

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type lifecycleHandler func(Target, *Conn)

func (h lifecycleHandler) Open(target Target, conn *Conn) { h(target, conn) }

func TestServeCancelsAndWaitsForSubConnections(t *testing.T) {
	for _, tc := range []struct {
		name         string
		endFirst     bool
		cancelParent bool
	}{
		{name: "disconnect"},
		{name: "disconnect_after_eof", endFirst: true},
		{name: "parent_cancel", cancelParent: true},
		{name: "parent_cancel_after_eof", endFirst: true, cancelParent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			srv := NewServer(lifecycleHandler(func(_ Target, conn *Conn) {
				close(entered)
				<-conn.Context().Done()
				close(canceled)
				<-release
			}))
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()
			done := make(chan error, 1)
			go func() { done <- srv.ServeContext(ctx, serverConn) }()
			cli := newTestClient(t, clientConn)
			sub := cli.open(Target{Host: "127.0.0.1", Port: 80}, nil)
			select {
			case <-entered:
			case <-time.After(testTimeout):
				t.Fatal("handler did not start")
			}
			if tc.endFirst {
				cli.end(sub, nil, false)
				select {
				case <-canceled:
					t.Fatal("request EOF canceled the response")
				default:
				}
			}
			if tc.cancelParent {
				cancel()
			} else {
				_ = clientConn.Close()
			}
			select {
			case <-canceled:
			case <-time.After(testTimeout):
				t.Fatal("master close did not cancel handler")
			}
			select {
			case err := <-done:
				t.Fatalf("Serve returned before handler: %v", err)
			default:
			}
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(testTimeout):
				t.Fatal("Serve did not wait for handler completion")
			}
			<-cli.readDone
		})
	}
}

func TestSubConnectionCancelIsIsolated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := NewSessionManager(ctx)
	defer mgr.closeAll()
	first, _ := mgr.add(1, "tcp", NewWriter(io.Discard))
	second, _ := mgr.add(2, "tcp", NewWriter(io.Discard))
	_ = first.Close()
	if ctx.Err() != nil || second.Context().Err() != nil {
		t.Fatal("sub-connection canceled parent or sibling")
	}
	cancel()
	if second.Context().Err() == nil {
		t.Fatal("parent cancellation did not reach sub-connection")
	}
	if _, ok := mgr.add(3, "tcp", NewWriter(io.Discard)); ok {
		t.Fatal("canceled manager accepted a new sub-connection")
	}
}

func TestRetiredConnDoesNotRemoveReusedID(t *testing.T) {
	mgr := NewSessionManager(context.Background())
	out := NewWriter(io.Discard)
	first, ok := mgr.add(7, "tcp", out)
	if !ok {
		t.Fatal("first add failed")
	}
	mgr.remove(7)
	second, ok := mgr.add(7, "tcp", out)
	if !ok {
		t.Fatal("id reuse failed")
	}
	_ = first.Close()
	if got, ok := mgr.get(7); !ok || got != second {
		t.Fatal("retired connection removed reused id")
	}
	mgr.closeAll()
	select {
	case <-second.Context().Done():
	default:
		t.Fatal("closeAll did not cancel active connection")
	}
}
