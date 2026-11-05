package client

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/secure"
	"github.com/hhy50/vnray/internal/tcp"
)

func TestDialPlaintextDoesNotDiscloseAuthKey(t *testing.T) {
	keyText := authkey.Generate()
	key, err := authkey.Parse(keyText)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	done := make(chan error, 1)
	go func() {
		done <- func() error {
			raw, err := ln.Accept()
			if err != nil {
				return err
			}
			defer raw.Close()
			conn, err := secure.Server(ctx, raw, func(_ context.Context, id authkey.ID) (authkey.Key, error) {
				if id != key.ID() {
					return authkey.Key{}, secure.ErrHandshake
				}
				return key, nil
			}, false)
			if err != nil {
				return err
			}
			session, err := tcp.DefaultMuxer().Server(conn, tcp.DefaultSessionConfig())
			if err != nil {
				return err
			}
			defer session.Close()
			_ = session.SetDeadline(time.Now().Add(time.Second))
			stream, err := session.AcceptStream()
			if err != nil {
				return err
			}
			defer stream.Close()
			_ = stream.SetDeadline(time.Now().Add(time.Second))
			frame, err := protocol.NewDecoder(stream).ReadFrame()
			if err != nil {
				return err
			}
			payload, err := protocol.ReadPayload(frame)
			if err != nil {
				return err
			}
			if bytes.Contains(payload, []byte(keyText)) || bytes.Contains(payload, key[:]) {
				return fmt.Errorf("plaintext authentication disclosed the secret")
			}
			auth, err := protocol.ParseAuthRequest(payload)
			if err != nil {
				return err
			}
			if frame.Type != protocol.Auth || auth.Key != fmt.Sprintf("%x", key.ID()) {
				return fmt.Errorf("authentication did not bind the public key identity")
			}
			protocol.WriteReply(stream, protocol.AuthResult, protocol.Reply{OK: true})
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		}()
	}()
	c, dialErr := DialContext(ctx, ln.Addr().String(), nil, DialOptions{Auth: AuthOptions{Key: keyText}, Info: protocol.ClientInfo{Name: "team:g:edge"}})
	if c != nil {
		_ = c.Close()
	}
	if dialErr != nil {
		t.Fatal(dialErr)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
