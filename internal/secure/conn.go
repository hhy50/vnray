package secure

import (
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"

	"github.com/hhy50/vnray/internal/authkey"
)

const (
	maxPayload   = 16 * 1024
	rekeyRecords = 1 << 16
	lengthSize   = 2 + chacha20poly1305.Overhead
)

// ErrRecord indicates a corrupt, replayed, or oversized encrypted record.
var ErrRecord = errors.New("invalid secure transport record")

// Conn carries a byte stream after peer authentication and mode negotiation.
type Conn struct {
	net.Conn
	id        authkey.ID
	plaintext bool
	readMu    sync.Mutex
	writeMu   sync.Mutex
	reader    direction
	writer    direction
	pending   []byte
	readErr   error
	writeErr  error
}

type direction struct {
	aead    cipher.AEAD
	key     [32]byte
	nonce   uint64
	records uint32
}

func newConn(raw net.Conn, id authkey.ID, material [96]byte, client bool) *Conn {
	c2s, s2c := material[32:64], material[64:96]
	if !client {
		c2s, s2c = s2c, c2s
	}
	c := &Conn{Conn: raw, id: id}
	c.writer.setKey(c2s)
	c.reader.setKey(s2c)
	return c
}

// KeyID returns the identity proven during the transport handshake.
func (c *Conn) KeyID() authkey.ID { return c.id }

// Encrypted reports the mode authenticated during the transport handshake.
func (c *Conn) Encrypted() bool { return !c.plaintext }

func (d *direction) setKey(key []byte) {
	copy(d.key[:], key)
	// The fixed-size key makes AEAD construction infallible.
	d.aead, _ = chacha20poly1305.New(d.key[:])
	d.nonce = 0
	d.records = 0
}

func (d *direction) nextRecord() {
	if d.records == rekeyRecords {
		var next [32]byte
		r := hkdf.New(sha256.New, d.key[:], nil, []byte("vnray transport v1 rekey"))
		// Fixed output is below HKDF's expansion limit.
		_, _ = io.ReadFull(r, next[:])
		d.setKey(next[:])
	}
	d.records++
}

func (d *direction) nextNonce() []byte {
	var nonce [chacha20poly1305.NonceSize]byte
	binary.LittleEndian.PutUint64(nonce[4:], d.nonce)
	d.nonce++
	return nonce[:]
}

// Read verifies AEAD records when encryption is enabled, otherwise reads raw bytes.
func (c *Conn) Read(p []byte) (int, error) {
	if c.plaintext {
		return c.Conn.Read(p)
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if c.readErr != nil {
		return 0, c.readErr
	}
	if len(c.pending) == 0 {
		if err := c.readRecord(); err != nil {
			c.readErr = err
			_ = c.Conn.Close()
			return 0, err
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *Conn) readRecord() error {
	c.reader.nextRecord()
	var header [lengthSize]byte
	if _, err := io.ReadFull(c.Conn, header[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	length, err := c.reader.aead.Open(header[:0], c.reader.nextNonce(), header[:], nil)
	if err != nil {
		return ErrRecord
	}
	n := int(binary.BigEndian.Uint16(length))
	if n == 0 || n > maxPayload {
		return ErrRecord
	}
	buf := make([]byte, n+chacha20poly1305.Overhead)
	if _, err := io.ReadFull(c.Conn, buf); err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	c.pending, err = c.reader.aead.Open(buf[:0], c.reader.nextNonce(), buf, nil)
	if err != nil {
		return ErrRecord
	}
	return nil
}

// Write uses the negotiated mode and serializes encrypted records across callers.
func (c *Conn) Write(p []byte) (int, error) {
	if c.plaintext {
		return c.Conn.Write(p)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxPayload)
		c.writer.nextRecord()
		var length [2]byte
		binary.BigEndian.PutUint16(length[:], uint16(n))
		buf := make([]byte, 0, lengthSize+n+chacha20poly1305.Overhead)
		buf = c.writer.aead.Seal(buf, c.writer.nextNonce(), length[:], nil)
		buf = c.writer.aead.Seal(buf, c.writer.nextNonce(), p[:n], nil)
		if err := writeAll(c.Conn, buf); err != nil {
			c.writeErr = err
			_ = c.Conn.Close()
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}
