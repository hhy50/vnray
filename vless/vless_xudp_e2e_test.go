package vless_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestVlessPluginXUDPEndToEnd(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		name := "plain"
		if useTLS {
			name = "tls"
		}
		t.Run(name, func(t *testing.T) {
			a := newXUDPEchoServer(t, 'a')
			b := newXUDPEchoServer(t, 'b')
			runVlessPlugin(t, useTLS, func(conn net.Conn) {
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := conn.Write(vlessRequest(3, "", 0)); err != nil {
					t.Fatal(err)
				}
				readResponseHeader(t, conn)
				for i, tc := range []struct {
					dest    *net.UDPAddr
					address bool
					marker  byte
				}{
					{a, true, 'a'},
					{b, true, 'b'},
					{a, false, 'a'},
					{a, false, 'a'},
				} {
					status := byte(2)
					if i == 0 {
						status = 1
					}
					meta := []byte{0, 7, status, 1}
					if tc.address {
						meta = append(meta, 2, byte(tc.dest.Port>>8), byte(tc.dest.Port), 1)
						meta = append(meta, tc.dest.IP.To4()...)
					}
					if i == 0 {
						meta = append(meta, 1, 2, 3, 4, 5, 6, 7, 8)
					}
					raw := binary.BigEndian.AppendUint16(nil, uint16(len(meta)))
					raw = append(raw, meta...)
					payload := []byte{byte(i)}
					if i == 3 {
						payload = nil
					}
					raw = append(raw, 0, byte(len(payload)))
					raw = append(raw, payload...)
					if _, err := conn.Write(raw); err != nil {
						t.Fatal(err)
					}
					want := []byte{0, 12, 0, 7, 2, 1, 2, byte(tc.dest.Port >> 8), byte(tc.dest.Port), 1, 127, 0, 0, 1, 0, byte(1 + len(payload)), tc.marker}
					want = append(want, payload...)
					got := make([]byte, len(want))
					if _, err := io.ReadFull(conn, got); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, want) {
						t.Fatalf("XUDP response = %x, want %x", got, want)
					}
				}
			})
		})
	}
}

func newXUDPEchoServer(t *testing.T, marker byte) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 65535)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if _, err := conn.WriteToUDP(append([]byte{marker}, buf[:n]...), addr); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close(); <-done })
	return conn.LocalAddr().(*net.UDPAddr)
}
