package dispatch

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/protocol"
)

func TestDirectOutboundUDPShutdown(t *testing.T) {
	for _, reason := range []string{"cancel", "upstream-eof", "downstream-closed"} {
		t.Run(reason, func(t *testing.T) {
			dest, err := ParseDestination(startUDPEcho(t))
			if err != nil {
				t.Fatal(err)
			}
			dest.Network = "udp"
			in, out := (&DefaultDispatcher{}).getLink()
			t.Cleanup(func() { in.Reader.Interrupt(); in.Writer.Interrupt() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- NewDirectOutbound("test").Process(ctx, out, dest) }()
			packets := protocol.NewDatagramConn(linkRW{r: in.Reader, w: in.Writer})
			exchanged := make(chan error, 1)
			go func() {
				if err := packets.WritePacket([]byte("ready")); err != nil {
					exchanged <- err
					return
				}
				packet, err := packets.ReadPacket()
				if err == nil && string(packet) != "ready" {
					err = fmt.Errorf("unexpected reply %q", packet)
				}
				exchanged <- err
			}()
			select {
			case err := <-exchanged:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("UDP exchange did not finish")
			}
			switch reason {
			case "cancel":
				cancel()
			case "upstream-eof":
				_ = in.Writer.Close()
			case "downstream-closed":
				in.Reader.Interrupt()
				if err := packets.WritePacket([]byte("after close")); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if reason == "downstream-closed" && err == nil {
					t.Fatal("downstream write error was lost")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("UDP shutdown left a pump blocked")
			}
		})
	}
}

type outboundTestDialer struct{ conn net.Conn }

func (d outboundTestDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return d.conn, nil
}

func TestDirectOutboundTCPShutdown(t *testing.T) {
	for _, reason := range []string{"cancel", "cancel-blocked-write", "peer-close"} {
		t.Run(reason, func(t *testing.T) {
			left, right := net.Pipe()
			t.Cleanup(func() { _ = left.Close(); _ = right.Close() })
			in, out := (&DefaultDispatcher{}).getLink()
			t.Cleanup(func() { in.Reader.Interrupt(); in.Writer.Interrupt() })
			direct := NewDirectOutboundWithDialer("test", outboundTestDialer{conn: left})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- direct.Process(ctx, out, Destination{Host: "127.0.0.1", Port: 1}) }()
			_ = right.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if _, err := right.Write([]byte("ready")); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(in.Reader, make([]byte, 5)); err != nil {
				t.Fatal(err)
			}
			if reason == "cancel-blocked-write" {
				_ = right.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
				if n, err := right.Write(make([]byte, 1<<20)); n == 0 || err == nil {
					t.Fatalf("expected downstream backpressure, wrote %d bytes: %v", n, err)
				}
			}
			if reason == "peer-close" {
				_ = right.Close()
			} else {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("TCP shutdown left a pump blocked")
			}
			if reason == "peer-close" && ctx.Err() != nil {
				t.Fatal("normal shutdown canceled the caller context")
			}
		})
	}
}

type panicWriteConn struct{ net.Conn }

func (panicWriteConn) Write([]byte) (int, error) { panic("test upstream write") }

func TestDirectOutboundTCPPanicStopsPeer(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })
	in, out := (&DefaultDispatcher{}).getLink()
	t.Cleanup(func() { in.Reader.Interrupt(); in.Writer.Interrupt() })
	direct := NewDirectOutboundWithDialer("test", outboundTestDialer{conn: panicWriteConn{Conn: left}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- direct.Process(ctx, out, Destination{Host: "127.0.0.1", Port: 1}) }()
	if _, err := in.Writer.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("upstream panic was not reported")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream panic left downstream blocked")
	}
}
