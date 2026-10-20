package protocol

// 隧道级数据报封帧的单元测试：往返帧、超长报拒绝、EOF 语义。
// net.Pipe 是严格同步的（写要等对端读），因此读侧在独立协程里并发进行。

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"reflect"
	"testing"
)

// TestDatagramConnRoundtrip 在一个内存双工对上往返若干数据报，边界保持不合并。
func TestDatagramConnRoundtrip(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	a := NewDatagramConn(clientConn)
	b := NewDatagramConn(serverConn)

	payloads := [][]byte{
		[]byte("hello"),
		[]byte("world world world"),
		{},
	}
	// 超长报在写方直接拒绝，不上线路；读方应照常收到前面三个数据报。
	oversized := make([]byte, MaxDatagram+1)

	got := make([][]byte, 0, len(payloads)+1)
	readErr := make(chan error, 1)
	go func() {
		for i := 0; i < 3; i++ {
			pkt, err := b.ReadPacket()
			if err != nil {
				readErr <- err
				return
			}
			got = append(got, pkt)
		}
		readErr <- nil
	}()

	for _, p := range payloads {
		if err := a.WritePacket(p); err != nil {
			t.Fatalf("write packet: %v", err)
		}
	}
	if err := a.WritePacket(oversized); err == nil {
		t.Fatalf("oversized datagram not rejected")
	}
	if err := <-readErr; err != nil {
		t.Fatalf("read packet: %v", err)
	}
	if !reflect.DeepEqual(got, payloads) {
		t.Fatalf("packets = %q, want %q", got, payloads)
	}
}

// TestDatagramConnEOF 读到对端干净关闭后返回 io.EOF。
func TestDatagramConnEOF(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	a := NewDatagramConn(clientConn)
	b := NewDatagramConn(serverConn)

	done := make(chan error, 1)
	go func() {
		pkt, err := b.ReadPacket()
		if err != nil {
			done <- err
			return
		}
		if !bytes.Equal(pkt, []byte("x")) {
			done <- fmt.Errorf("unexpected packet %q", pkt)
			return
		}
		_, err = b.ReadPacket()
		done <- err
	}()
	if err := a.WritePacket([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 关闭写端后，读端在消费完既有数据后应读到 io.EOF。
	_ = a.Close()
	if err := <-done; err != io.EOF {
		t.Fatalf("read after close = %v, want io.EOF", err)
	}
}
