package mux

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestXUDPFrameMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status byte
		addr   []byte
		host   string
		tail   []byte
	}{
		{"new global ID", StatusNew, []byte{1, 1, 1, 1, 1}, "1.1.1.1", []byte{1, 2, 3, 4, 5, 6, 7, 8}},
		{"new zero global ID", StatusNew, []byte{1, 1, 1, 1, 1}, "1.1.1.1", make([]byte, 8)},
		{"keep ipv4", StatusKeep, []byte{1, 8, 8, 8, 8}, "8.8.8.8", nil},
		{"keep domain", StatusKeep, append([]byte{2, 11}, "dns.example"...), "dns.example", nil},
		{"keep ipv6", StatusKeep, []byte{3, 0x20, 1, 0xd, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, "2001:db8::1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := append([]byte{0, 7, tc.status, OptionData, NetworkUDP, 0, 53}, tc.addr...)
			meta = append(meta, tc.tail...)
			f, err := readFrame(bytes.NewReader(xudpWireFrame(meta, []byte("dns"))))
			if err != nil {
				t.Fatal(err)
			}
			if f.SessionID != 7 || f.Network != NetworkUDP || f.Target.Host != tc.host || f.Target.Port != 53 || string(f.Data) != "dns" {
				t.Fatalf("frame = %+v", f)
			}
			if tc.name == "new global ID" && f.GlobalID != [8]byte{1, 2, 3, 4, 5, 6, 7, 8} {
				t.Fatalf("global ID = %x", f.GlobalID)
			}
		})
	}
}

func TestXUDPInvalidMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status byte
		tail   []byte
	}{
		{"short global ID", StatusNew, make([]byte, 7)},
		{"long global ID", StatusNew, make([]byte, 9)},
		{"unexpected keep suffix", StatusKeep, make([]byte, 8)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := append([]byte{0, 7, tc.status, OptionData, NetworkUDP, 0, 53, 1, 1, 1, 1, 1}, tc.tail...)
			if _, err := readFrame(bytes.NewReader(xudpWireFrame(meta, nil))); err == nil {
				t.Fatal("malformed metadata accepted")
			}
		})
	}
}

func TestXUDPResponseLayout(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out)
	if err := w.writePacketFrom(7, Target{Host: "8.8.8.8", Port: 53}, []byte("dns")); err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 12, 0, 7, StatusKeep, OptionData, NetworkUDP, 0, 53, 1, 8, 8, 8, 8, 0, 3, 'd', 'n', 's'}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("response = %x, want %x", out.Bytes(), want)
	}
}

func TestXUDPServerTargetsAndTCPIsolation(t *testing.T) {
	conn, serverConn := net.Pipe()
	_ = conn.SetDeadline(time.Now().Add(testTimeout))
	srv := NewServer(lifecycleHandler(func(target Target, c *Conn) {
		if target.Network == "tcp" {
			_, _ = io.Copy(c, c)
			return
		}
		for {
			dest, data, err := c.ReadPacketTo()
			if err != nil {
				return
			}
			if err := c.WritePacketFrom(dest, data); err != nil {
				return
			}
		}
	}))
	done := make(chan error, 1)
	go func() { done <- srv.Serve(serverConn) }()
	defer func() {
		_ = conn.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(testTimeout):
			t.Error("mux server did not stop")
		}
	}()
	for _, tc := range []struct {
		meta []byte
		data []byte
		host string
	}{
		{append([]byte{0, 1, StatusNew, OptionData, NetworkUDP, 0, 53, 1, 1, 1, 1, 1}, 1, 2, 3, 4, 5, 6, 7, 8), []byte("first"), "1.1.1.1"},
		{[]byte{0, 1, StatusKeep, OptionData, NetworkUDP, 0, 53, 1, 8, 8, 8, 8}, []byte("changed"), "8.8.8.8"},
		{[]byte{0, 1, StatusKeep, OptionData}, nil, "1.1.1.1"},
		{[]byte{0, 1, StatusKeep, OptionData}, bytes.Repeat([]byte{'x'}, maxSessionDatagram), "1.1.1.1"},
		{[]byte{0, 2, StatusNew, OptionData, NetworkTCP, 0, 80, 1, 127, 0, 0, 1}, []byte("tcp"), ""},
	} {
		if _, err := conn.Write(xudpWireFrame(tc.meta, tc.data)); err != nil {
			t.Fatal(err)
		}
		f, err := readFrame(conn)
		if err != nil {
			t.Fatal(err)
		}
		if f.Status != StatusKeep || f.Target.Host != tc.host || !bytes.Equal(f.Data, tc.data) {
			t.Fatalf("response = %+v, want target %s and %d bytes", f, tc.host, len(tc.data))
		}
	}
}

func xudpWireFrame(meta, data []byte) []byte {
	raw := binary.BigEndian.AppendUint16(nil, uint16(len(meta)))
	raw = append(raw, meta...)
	if meta[3]&OptionData != 0 {
		raw = binary.BigEndian.AppendUint16(raw, uint16(len(data)))
		raw = append(raw, data...)
	}
	return raw
}
