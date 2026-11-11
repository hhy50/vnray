package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/client"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/secure"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
)

func TestTransportEncryptionHotReload(t *testing.T) {
	b := testBindings(t)
	keyText := authkey.Generate()
	key, err := authkey.Parse(keyText)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetAuthKey(context.Background(), keyText, "team:*"); err != nil {
		t.Fatal(err)
	}
	svc := newSecureTestServer(t, b, "")
	settings := config.DefaultServiceSettings()
	settings.Heartbeat.Enabled = false
	runtime := newServiceConfig(testStore(t), svc, traffic.NewMeter(ratelimit.NewRegistry()), nil, nil, settings)
	var existing []struct {
		conn      *secure.Conn
		encrypted bool
	}
	for i, encrypted := range []bool{true, false, true} {
		settings.TransportEncryption = encrypted
		if err := runtime.Apply(context.Background(), settings); err != nil {
			t.Fatal(err)
		}
		cfg := config.DefaultServer()
		if loaded := loadServiceSettings(context.Background(), runtime.settings, &cfg); loaded.TransportEncryption != encrypted {
			t.Fatal("encryption mode did not survive settings reload")
		}
		for _, old := range existing {
			if old.conn.Encrypted() != old.encrypted {
				t.Fatal("existing connection mode changed")
			}
		}
		raw, err := net.DialTimeout("tcp", svc.Addr(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := secure.Client(context.Background(), raw, key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if conn.Encrypted() != encrypted {
			t.Fatalf("new connection encrypted=%v, want %v", conn.Encrypted(), encrypted)
		}
		existing = append(existing, struct {
			conn      *secure.Conn
			encrypted bool
		}{conn, encrypted})
		for _, tc := range []struct {
			name, key string
			allow     bool
		}{
			{fmt.Sprintf("team:edge%d", i), keyText, true},
			{"other:edge", keyText, false},
			{"team:wrong", authkey.Generate(), false},
		} {
			group, name, _ := strings.Cut(tc.name, ":")
			c, err := client.Dial(svc.Addr(), nil, client.DialOptions{Auth: client.AuthOptions{Key: tc.key}, Info: protocol.ClientInfo{Group: group, Name: name}})
			if c != nil {
				_ = c.Close()
			}
			if (err == nil) != tc.allow {
				t.Fatalf("encryption=%v client=%s allowed=%v: %v", encrypted, tc.name, tc.allow, err)
			}
		}
	}
	for i, old := range existing {
		session, err := tcp.DefaultMuxer().Client(old.conn, tcp.DefaultSessionConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		stream, err := session.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		_ = stream.SetDeadline(time.Now().Add(time.Second))
		frame, err := protocol.NewConfigReportFrame(protocol.ConfigReportRequest{Info: protocol.ClientInfo{Group: "team", Name: fmt.Sprintf("retained%d", i)}})
		if err != nil {
			t.Fatal(err)
		}
		if err := protocol.NewEncoder(stream).WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
		response, err := protocol.NewDecoder(stream).ReadFrame()
		if err != nil {
			t.Fatalf("existing connection stopped working: %v", err)
		}
		payload, err := protocol.ReadPayload(response)
		if err != nil {
			t.Fatal(err)
		}
		reply, err := protocol.ParseReply(payload)
		if err != nil || !reply.OK {
			t.Fatalf("existing connection authentication: %+v, %v", reply, err)
		}
	}
}

func newSecureTestServer(t *testing.T, b *admin.Bindings, fallback string) *ConnectorService {
	t.Helper()
	cfg := config.DefaultServer()
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.Auth.Key = fallback
	svc := NewConnectorService(&cfg, transport.NewSessionManager(), b, nil)
	svc.SetSessionConfig(b)
	svc.ApplyHeartbeat(0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	if err := svc.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		ctx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if err := svc.Drain(ctx); err != nil {
			t.Error(err)
		}
	})
	return svc
}

func TestEncryptedScopedClient(t *testing.T) {
	b := testBindings(t)
	key, fallback := authkey.Generate(), authkey.Generate()
	if err := b.SetAuthKey(context.Background(), key, "team:*"); err != nil {
		t.Fatal(err)
	}
	svc := newSecureTestServer(t, b, fallback)
	for _, tc := range []struct {
		name, key string
		allow     bool
	}{
		{"team:edge", key, true},
		{"other:edge", key, false},
		{"team:edge", fallback, false},
	} {
		group, name, _ := strings.Cut(tc.name, ":")
		c, err := client.Dial(svc.Addr(), nil, client.DialOptions{Auth: client.AuthOptions{Key: tc.key}, Info: protocol.ClientInfo{Group: group, Name: name}})
		if c != nil {
			_ = c.Close()
		}
		if (err == nil) != tc.allow {
			t.Fatalf("client %s allowed=%v: %v", tc.name, tc.allow, err)
		}
	}
	if _, err := b.DeleteAuthKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if c, err := client.Dial(svc.Addr(), nil, client.DialOptions{Auth: client.AuthOptions{Key: key}}); err == nil {
		_ = c.Close()
		t.Fatal("deleted key accepted")
	}
}

func TestPlaintextClientRejected(t *testing.T) {
	for _, encrypted := range []bool{true, false} {
		t.Run(fmt.Sprintf("encryption=%v", encrypted), func(t *testing.T) {
			testUnprovenClientRejected(t, encrypted)
		})
	}
}

func testUnprovenClientRejected(t *testing.T, encrypted bool) {
	t.Helper()
	key := authkey.Generate()
	svc := newSecureTestServer(t, testBindings(t), key)
	svc.ApplyTransportEncryption(encrypted)
	raw, err := net.DialTimeout("tcp", svc.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	session, err := tcp.DefaultMuxer().Client(raw, tcp.DefaultSessionConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stream, err := session.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.SetDeadline(time.Now().Add(time.Second))
	frame, err := protocol.NewConfigReportFrame(protocol.ConfigReportRequest{Info: protocol.ClientInfo{Group: "team", Name: "edge"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewEncoder(stream).WriteFrame(frame); err != nil {
		return
	}
	replyFrame, err := protocol.NewDecoder(stream).ReadFrame()
	if err == nil {
		body, err := protocol.ReadPayload(replyFrame)
		if err != nil {
			t.Fatal(err)
		}
		reply, err := protocol.ParseReply(body)
		if err != nil || reply.OK {
			t.Fatalf("plaintext client accepted: %v", err)
		}
	}
	if svc.Sessions().ClientCount() != 0 {
		t.Fatal("plaintext client registered")
	}
}

func TestTransportIdentityCannotSwitchKeys(t *testing.T) {
	b := testBindings(t)
	limited, privileged := authkey.Generate(), authkey.Generate()
	if err := b.SetAuthKey(context.Background(), limited, "limited:*"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetAuthKey(context.Background(), privileged, "*"); err != nil {
		t.Fatal(err)
	}
	svc := newSecureTestServer(t, b, "")
	raw, err := net.DialTimeout("tcp", svc.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := authkey.Parse(limited)
	conn, err := secure.Client(context.Background(), raw, key)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	session, err := tcp.DefaultMuxer().Client(conn, tcp.DefaultSessionConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stream, err := session.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.SetDeadline(time.Now().Add(time.Second))
	frame, err := protocol.NewConfigReportFrame(protocol.ConfigReportRequest{Info: protocol.ClientInfo{Group: "other", Name: "edge"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewEncoder(stream).WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	if frame, err := protocol.NewDecoder(stream).ReadFrame(); err == nil {
		payload, err := protocol.ReadPayload(frame)
		if err != nil {
			t.Fatal(err)
		}
		reply, err := protocol.ParseReply(payload)
		if err != nil || reply.OK {
			t.Fatalf("different authentication key accepted: %v", err)
		}
	}
	if svc.Sessions().ClientCount() != 0 {
		t.Fatal("wrong identity registered")
	}
}

type failingAuthStore struct{ store.Store }

func (f failingAuthStore) ListAuthKeys(context.Context) ([]store.AuthKey, error) {
	return nil, errors.New("storage unavailable")
}

func TestKeyLookupFailsClosed(t *testing.T) {
	fallback := authkey.Generate()
	b := admin.NewBindings(failingAuthStore{})
	svc := &ConnectorService{authKey: fallback, sessionCfg: b}
	key, _ := authkey.Parse(fallback)
	if _, err := svc.lookupTransportKey(context.Background(), key.ID()); err == nil {
		t.Fatal("storage failure fell back to configuration")
	}
	if err := svc.verifyKey(context.Background(), "team:edge", fallback); err == nil {
		t.Fatal("authorization failed open")
	}
}
