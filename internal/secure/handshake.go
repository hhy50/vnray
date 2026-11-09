// Package secure authenticates TCP connections and negotiates transport encryption.
package secure

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/routine"
)

const (
	preamble         = "VNRAY\x02\x01\x00"
	helloSize        = len(preamble) + 32 + 32
	responseSize     = 32 + 1 + 32
	handshakeTimeout = 5 * time.Second
)

// ErrHandshake indicates an unsupported transport or failed peer proof.
var ErrHandshake = errors.New("secure transport handshake rejected")

// Lookup resolves a public key identifier to its current secret key.
type Lookup func(context.Context, authkey.ID) (authkey.Key, error)

// Client authenticates the server and adopts its authenticated encryption mode.
func Client(ctx context.Context, raw net.Conn, key authkey.Key) (*Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	return handshake(ctx, raw, func() (*Conn, error) {
		var hello [helloSize]byte
		copy(hello[:], preamble)
		id := key.ID()
		copy(hello[len(preamble):], id[:])
		rand.Read(hello[len(preamble)+len(id):])
		if err := writeAll(raw, hello[:]); err != nil {
			return nil, err
		}
		var response [responseSize]byte
		if _, err := io.ReadFull(raw, response[:]); err != nil {
			return nil, err
		}
		if response[32] > 1 {
			return nil, ErrHandshake
		}
		material := derive(key, hello[:], response[:33])
		if !hmac.Equal(response[33:], proof(material, "server")) {
			return nil, ErrHandshake
		}
		if err := writeAll(raw, proof(material, "client")); err != nil {
			return nil, err
		}
		return negotiatedConn(raw, id, material, true, response[32] == 1), nil
	})
}

// Server proves both peers' identities and authenticates the selected encryption mode.
func Server(ctx context.Context, raw net.Conn, lookup Lookup, encrypt bool) (*Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	return handshake(ctx, raw, func() (*Conn, error) {
		var hello [helloSize]byte
		if _, err := io.ReadFull(raw, hello[:]); err != nil {
			return nil, err
		}
		if string(hello[:len(preamble)]) != preamble {
			return nil, ErrHandshake
		}
		var id authkey.ID
		copy(id[:], hello[len(preamble):])
		key, err := lookup(ctx, id)
		if err != nil || key.ID() != id {
			return nil, ErrHandshake
		}
		var response [responseSize]byte
		rand.Read(response[:32])
		if encrypt {
			response[32] = 1
		}
		material := derive(key, hello[:], response[:33])
		copy(response[33:], proof(material, "server"))
		if err := writeAll(raw, response[:]); err != nil {
			return nil, err
		}
		var clientProof [32]byte
		if _, err := io.ReadFull(raw, clientProof[:]); err != nil {
			return nil, err
		}
		if !hmac.Equal(clientProof[:], proof(material, "client")) {
			return nil, ErrHandshake
		}
		return negotiatedConn(raw, id, material, false, encrypt), nil
	})
}

func negotiatedConn(raw net.Conn, id authkey.ID, material [96]byte, client, encrypt bool) *Conn {
	if !encrypt {
		return &Conn{Conn: raw, id: id, plaintext: true}
	}
	return newConn(raw, id, material, client)
}

func handshake(ctx context.Context, raw net.Conn, exchange func() (*Conn, error)) (*Conn, error) {
	deadline := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := raw.SetDeadline(deadline); err != nil {
		_ = raw.Close()
		return nil, err
	}
	var watcher sync.WaitGroup
	watcher.Add(1)
	finished := make(chan struct{})
	routine.Go("secure.handshake.cancel", func() {
		defer watcher.Done()
		select {
		case <-ctx.Done():
			_ = raw.Close()
		case <-finished:
		}
	})
	conn, err := exchange()
	close(finished)
	watcher.Wait()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil {
		err = raw.SetDeadline(time.Time{})
	}
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("secure handshake: %w", err)
	}
	return conn, nil
}

func derive(key authkey.Key, hello, salt []byte) [96]byte {
	h := sha256.New()
	h.Write(hello)
	h.Write(salt)
	var material [96]byte
	r := hkdf.New(sha256.New, key[:], h.Sum(nil), []byte("vnray transport v2 keys"))
	// Fixed output is below HKDF's expansion limit.
	_, _ = io.ReadFull(r, material[:])
	return material
}

func proof(material [96]byte, role string) []byte {
	h := hmac.New(sha256.New, material[:32])
	h.Write([]byte("vnray transport v2 " + role + " finished"))
	return h.Sum(nil)
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
