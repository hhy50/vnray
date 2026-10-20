package dispatch

// DirectOutbound 的 UDP 出站端到端测试：经默认 dispatcher 分发 UDP 目标，link 侧以
// [2B 长度][数据报] 封帧，经本机 UDP 回声服务器往返。

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/protocol"
)

// startUDPEcho 起一个本机 UDP 回声服务，返回监听地址。
func startUDPEcho(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp echo: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, protocol.MaxDatagram)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], addr)
		}
	}()
	return pc.LocalAddr().String()
}

// TestDirectOutboundUDP 发送若干数据报并收到回声，验证 UDP socket 出站与 [2B len] 封帧。
func TestDirectOutboundUDP(t *testing.T) {
	target := startUDPEcho(t)

	d := NewDefaultDispatcher(nil, "direct", NewDirectOutbound("direct"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatalf("split target: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	link, err := d.Dispatch(ctx, Destination{Host: host, Port: port, Network: "udp"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	t.Cleanup(func() {
		link.Reader.Interrupt()
		link.Writer.Interrupt()
	})
	dc := protocol.NewDatagramConn(linkRW{r: link.Reader, w: link.Writer})

	payloads := [][]byte{[]byte("ping"), []byte("second datagram"), []byte("third")}
	for _, p := range payloads {
		if err := dc.WritePacket(p); err != nil {
			t.Fatalf("write datagram: %v", err)
		}
		rch := make(chan struct {
			got []byte
			err error
		}, 1)
		go func() {
			got, err := dc.ReadPacket()
			rch <- struct {
				got []byte
				err error
			}{got, err}
		}()
		select {
		case res := <-rch:
			if res.err != nil {
				t.Fatalf("read echo for %q: %v", p, res.err)
			}
			if string(res.got) != string(p) {
				t.Fatalf("echo = %q, want %q", res.got, p)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout waiting echo for %q", p)
		}
	}
}
