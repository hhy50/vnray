package inbound

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/shadowsocks"
	"github.com/hhy50/vnray/vless"
	"github.com/hhy50/vnray/vmess"
)

type stoppableProtocol interface {
	Serve(net.Listener) error
	Close() error
	Done() <-chan struct{}
}

type heldAcceptListener struct {
	net.Listener
	entered chan struct{}
	release chan struct{}
}

func (l *heldAcceptListener) Accept() (net.Conn, error) {
	close(l.entered)
	<-l.release
	return nil, net.ErrClosed
}

func TestProtocolDoneWaitsForAcceptExit(t *testing.T) {
	for name, create := range map[string]func() stoppableProtocol{
		"vmess": func() stoppableProtocol { return vmess.NewServer(vmess.NewValidator(), nil) },
		"vless": func() stoppableProtocol { return vless.NewServer(vless.NewValidator(), nil) },
		"shadowsocks": func() stoppableProtocol {
			return shadowsocks.NewServer(shadowsocks.NewValidator(shadowsocks.MethodAES256GCM), nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			listener := &heldAcceptListener{Listener: ln, entered: make(chan struct{}), release: make(chan struct{})}
			var once sync.Once
			release := func() { once.Do(func() { close(listener.release) }) }
			t.Cleanup(func() { release(); _ = ln.Close() })
			server := create()
			served := make(chan error, 1)
			go func() { served <- server.Serve(listener) }()
			select {
			case <-listener.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("server did not enter Accept")
			}
			if err := server.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-server.Done():
				t.Fatal("Done closed before Accept returned")
			default:
			}
			release()
			select {
			case err := <-served:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Serve did not exit")
			}
			mustWaitGeneration(t, server.Done())
		})
	}
}

func TestProtocolCloseDuringStart(t *testing.T) {
	for name, create := range map[string]func() stoppableProtocol{
		"vmess": func() stoppableProtocol { return vmess.NewServer(vmess.NewValidator(), nil) },
		"vless": func() stoppableProtocol { return vless.NewServer(vless.NewValidator(), nil) },
		"shadowsocks": func() stoppableProtocol {
			return shadowsocks.NewServer(shadowsocks.NewValidator(shadowsocks.MethodAES256GCM), nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			for range 32 {
				server := create()
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				served := make(chan error, 1)
				go func() { served <- server.Serve(ln) }()
				if err := server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					t.Fatal(err)
				}
				select {
				case err := <-served:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					_ = ln.Close()
					t.Fatal("listener did not stop")
				}
				select {
				case <-server.Done():
				case <-time.After(3 * time.Second):
					t.Fatal("server did not finish")
				}
				if err := server.Close(); err != nil {
					t.Fatalf("repeated close: %v", err)
				}
			}
		})
	}
}
