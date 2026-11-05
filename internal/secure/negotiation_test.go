package secure

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type observedConn struct {
	net.Conn
	wire   bytes.Buffer
	tamper bool
}

func (c *observedConn) Write(p []byte) (int, error) {
	p = bytes.Clone(p)
	if c.tamper {
		p[32] ^= 1
		c.tamper = false
	}
	_, _ = c.wire.Write(p)
	return c.Conn.Write(p)
}

func TestNegotiatedTransportModes(t *testing.T) {
	for _, encrypted := range []bool{true, false} {
		t.Run(map[bool]string{true: "encrypted", false: "plaintext"}[encrypted], func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			key := testKey(t)
			wire := &observedConn{Conn: a}
			type result struct {
				conn *Conn
				err  error
			}
			done := make(chan result, 1)
			go func() {
				conn, err := Server(ctx, b, keyLookup(key), encrypted)
				done <- result{conn, err}
			}()
			client, err := Client(ctx, wire, key)
			if err != nil {
				t.Fatal(err)
			}
			server := <-done
			if server.err != nil {
				t.Fatal(server.err)
			}
			if client.Encrypted() != encrypted || server.conn.Encrypted() != encrypted {
				t.Fatal("peers did not agree on the server mode")
			}
			if client.KeyID() != key.ID() || server.conn.KeyID() != key.ID() {
				t.Fatal("negotiation lost the proven identity")
			}
			_ = a.SetDeadline(time.Now().Add(time.Second))
			_ = b.SetDeadline(time.Now().Add(time.Second))
			wire.wire.Reset()
			payload := bytes.Repeat([]byte("negotiated transport payload"), 1000)
			written := make(chan error, 1)
			go func() { _, err := client.Write(payload); written <- err }()
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(server.conn, got); err != nil {
				t.Fatal(err)
			}
			if err := <-written; err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("payload mismatch")
			}
			if encrypted == bytes.Equal(wire.wire.Bytes(), payload) {
				t.Fatal("wire data does not match the negotiated mode")
			}
		})
	}
}

func TestNegotiationRejectsModeTampering(t *testing.T) {
	for _, encrypted := range []bool{true, false} {
		t.Run(map[bool]string{true: "downgrade", false: "upgrade"}[encrypted], func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			key := testKey(t)
			done := make(chan error, 1)
			go func() {
				_, err := Server(ctx, &observedConn{Conn: b, tamper: true}, keyLookup(key), encrypted)
				done <- err
			}()
			if _, err := Client(ctx, a, key); !errors.Is(err, ErrHandshake) {
				t.Fatalf("tampered server mode accepted: %v", err)
			}
			if err := <-done; err == nil {
				t.Fatal("server accepted unproven client")
			}
		})
	}
}

func TestPlaintextModeStillRejectsWrongKey(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	serverKey := testKey(t)
	go func() { _, err := Server(ctx, b, keyLookup(serverKey), false); done <- err }()
	if _, err := Client(ctx, a, testKey(t)); err == nil {
		t.Fatal("wrong key accepted in plaintext mode")
	}
	if err := <-done; !errors.Is(err, ErrHandshake) {
		t.Fatalf("server rejection: %v", err)
	}
}
