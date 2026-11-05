package secure

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/authkey"
)

func testKey(t *testing.T) authkey.Key {
	t.Helper()
	key, err := authkey.Parse(authkey.Generate())
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func keyLookup(key authkey.Key) Lookup {
	return func(_ context.Context, id authkey.ID) (authkey.Key, error) {
		if id != key.ID() {
			return authkey.Key{}, ErrHandshake
		}
		return key, nil
	}
}

func securePair(t *testing.T) (*Conn, *Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key := testKey(t)
	type result struct {
		conn *Conn
		err  error
	}
	ready := make(chan result, 1)
	go func() { conn, err := Server(ctx, b, keyLookup(key), true); ready <- result{conn, err} }()
	client, err := Client(ctx, a, key)
	if err != nil {
		t.Fatal(err)
	}
	server := <-ready
	if server.err != nil {
		t.Fatal(server.err)
	}
	_ = a.SetDeadline(time.Now().Add(5 * time.Second))
	_ = b.SetDeadline(time.Now().Add(5 * time.Second))
	return client, server.conn
}

func TestFullDuplex(t *testing.T) {
	a, b := securePair(t)
	up, down := make([]byte, maxPayload*4+31), make([]byte, maxPayload*3+7)
	rand.Read(up)
	rand.Read(down)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	write := func(c net.Conn, p []byte) { defer wg.Done(); _, err := c.Write(p); errs <- err }
	read := func(c net.Conn, want []byte) {
		defer wg.Done()
		var got bytes.Buffer
		_, err := io.CopyBuffer(&got, io.LimitReader(c, int64(len(want))), make([]byte, 73))
		if err == nil && !bytes.Equal(got.Bytes(), want) {
			err = errors.New("payload mismatch")
		}
		errs <- err
	}
	wg.Add(4)
	go write(a, up)
	go read(b, up)
	go write(b, down)
	go read(a, down)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if a.writer.key == a.reader.key {
		t.Fatal("directions share a key")
	}
}

func TestConcurrentWriters(t *testing.T) {
	a, b := securePair(t)
	const count = 16
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := range count {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := a.Write(bytes.Repeat([]byte{byte(i)}, 100)); errs <- err }()
	}
	got := make([]byte, count*100)
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[byte]bool)
	for offset := 0; offset < len(got); offset += 100 {
		chunk := got[offset : offset+100]
		if !bytes.Equal(chunk, bytes.Repeat(chunk[:1], 100)) || seen[chunk[0]] {
			t.Fatal("concurrent writes interleaved")
		}
		seen[chunk[0]] = true
	}
}

type memoryConn struct {
	bytes.Buffer
	closed   bool
	writeErr error
}

func (c *memoryConn) Close() error                     { c.closed = true; return nil }
func (c *memoryConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *memoryConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *memoryConn) SetDeadline(time.Time) error      { return nil }
func (c *memoryConn) SetReadDeadline(time.Time) error  { return nil }
func (c *memoryConn) SetWriteDeadline(time.Time) error { return nil }
func (c *memoryConn) Write(p []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return c.Buffer.Write(p)
}

func memoryPair() (*Conn, *Conn, *memoryConn) {
	var material [96]byte
	rand.Read(material[:])
	wire := &memoryConn{}
	return newConn(wire, authkey.ID{}, material, true), newConn(wire, authkey.ID{}, material, false), wire
}

func TestRejectModifiedRecords(t *testing.T) {
	for _, offset := range []int{0, 2, lengthSize, lengthSize + 9} {
		t.Run(string(rune('a'+offset)), func(t *testing.T) {
			writer, reader, wire := memoryPair()
			_, _ = writer.Write([]byte("private target and credential"))
			wire.Bytes()[offset] ^= 1
			var out [64]byte
			n, err := reader.Read(out[:])
			if n != 0 || !errors.Is(err, ErrRecord) || !wire.closed {
				t.Fatalf("tampering returned %d, %v", n, err)
			}
			if n, err := reader.Read(out[:]); n != 0 || !errors.Is(err, ErrRecord) {
				t.Fatal("reader resumed after authentication error")
			}
		})
	}
}

func TestRecordReplayAndTruncation(t *testing.T) {
	for _, mode := range []string{"replay", "header", "payload", "boundary"} {
		t.Run(mode, func(t *testing.T) {
			writer, reader, wire := memoryPair()
			_, _ = writer.Write([]byte("secret"))
			record := bytes.Clone(wire.Bytes())
			wire.Reset()
			switch mode {
			case "replay":
				wire.Write(record)
				wire.Write(record)
			case "header":
				wire.Write(record[:3])
			case "payload":
				wire.Write(record[:len(record)-1])
			case "boundary":
				wire.Write(record)
			}
			buf := make([]byte, 6)
			if mode == "replay" || mode == "boundary" {
				if _, err := io.ReadFull(reader, buf); err != nil {
					t.Fatal(err)
				}
			}
			n, err := reader.Read(buf)
			if n != 0 || err == nil || errors.Is(err, io.EOF) || !wire.closed {
				t.Fatalf("accepted %s: %d, %v", mode, n, err)
			}
		})
	}
}

func TestRejectOversizedRecord(t *testing.T) {
	writer, reader, wire := memoryPair()
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], maxPayload+1)
	header := writer.writer.aead.Seal(nil, writer.writer.nextNonce(), length[:], nil)
	wire.Write(header)
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, ErrRecord) {
		t.Fatalf("oversized record: %v", err)
	}
}

func TestAutomaticRekey(t *testing.T) {
	writer, reader, wire := memoryPair()
	initial := writer.writer.key
	for range rekeyRecords + 1 {
		if _, err := writer.Write([]byte{7}); err != nil {
			t.Fatal(err)
		}
		var got [1]byte
		if _, err := io.ReadFull(reader, got[:]); err != nil || got[0] != 7 {
			t.Fatalf("rekey read: %v", err)
		}
	}
	if writer.writer.key == initial || writer.writer.key != reader.reader.key || wire.closed {
		t.Fatal("key rotation not synchronized")
	}
}

func TestWriteFailureIsTerminal(t *testing.T) {
	writer, _, wire := memoryPair()
	wire.writeErr = io.ErrClosedPipe
	if _, err := writer.Write([]byte("secret")); !errors.Is(err, io.ErrClosedPipe) || !wire.closed {
		t.Fatal("write failure not terminal")
	}
	wire.writeErr = nil
	if _, err := writer.Write([]byte("secret")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("writer resumed after failure")
	}
}

func TestHandshakeRejectsWrongKey(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	serverKey, clientKey := testKey(t), testKey(t)
	go func() { _, err := Server(ctx, b, keyLookup(serverKey), true); done <- err }()
	if _, err := Client(ctx, a, clientKey); err == nil {
		t.Fatal("wrong key accepted")
	}
	if err := <-done; !errors.Is(err, ErrHandshake) {
		t.Fatalf("server rejection: %v", err)
	}
}

func TestHandshakeCancellation(t *testing.T) {
	for _, server := range []bool{false, true} {
		a, b := net.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		key := testKey(t)
		done := make(chan error, 1)
		go func() {
			var err error
			if server {
				_, err = Server(ctx, a, keyLookup(key), true)
			} else {
				_, err = Client(ctx, a, key)
			}
			done <- err
		}()
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("handshake did not cancel")
		}
		_ = a.Close()
		_ = b.Close()
	}
}

func TestHandshakeDeadline(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := Server(ctx, a, keyLookup(testKey(t)), true); err == nil {
		t.Fatal("idle handshake did not time out")
	}
}

func TestHandshakeReplay(t *testing.T) {
	key := testKey(t)
	var hello [helloSize]byte
	copy(hello[:], preamble)
	id := key.ID()
	copy(hello[len(preamble):], id[:])
	rand.Read(hello[len(preamble)+len(id):])
	var recordedProof []byte
	for attempt := range 2 {
		a, b := net.Pipe()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		done := make(chan error, 1)
		go func() {
			c, err := Server(ctx, b, keyLookup(key), true)
			if c != nil {
				_ = c.Close()
			}
			done <- err
		}()
		if err := writeAll(a, hello[:]); err != nil {
			t.Fatal(err)
		}
		var response [responseSize]byte
		if _, err := io.ReadFull(a, response[:]); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			recordedProof = proof(derive(key, hello[:], response[:33]), "client")
		}
		if err := writeAll(a, recordedProof); err != nil {
			t.Fatal(err)
		}
		err := <-done
		cancel()
		_ = a.Close()
		_ = b.Close()
		if attempt == 0 && err != nil {
			t.Fatal(err)
		}
		if attempt == 1 && !errors.Is(err, ErrHandshake) {
			t.Fatalf("recorded handshake accepted: %v", err)
		}
	}
}

func TestRejectForgedServer(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var hello [helloSize]byte
		_, _ = io.ReadFull(b, hello[:])
		_ = writeAll(b, make([]byte, responseSize))
	}()
	if _, err := Client(ctx, a, testKey(t)); !errors.Is(err, ErrHandshake) {
		t.Fatalf("forged server accepted: %v", err)
	}
	<-done
}

func TestCloseUnblocksIO(t *testing.T) {
	a, b := securePair(t)
	done := make(chan error, 2)
	go func() { _, err := a.Read(make([]byte, 1)); done <- err }()
	go func() { _, err := a.Write([]byte("blocked")); done <- err }()
	_ = a.Close()
	_ = b.Close()
	for range 2 {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("I/O succeeded after close")
			}
		case <-time.After(time.Second):
			t.Fatal("close did not unblock I/O")
		}
	}
}
