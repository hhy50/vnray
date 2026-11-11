package inbound_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/client"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/core"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/inbound"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/internal/userclient"
)

type userPluginStore struct {
	store.Store
	saveErr error
}

func (s *userPluginStore) SetPlugin(ctx context.Context, state store.PluginState) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.Store.SetPlugin(ctx, state)
}

func TestVNRayUserSeparatePortLifecycle(t *testing.T) {
	ctx := context.Background()
	st := &userPluginStore{Store: testStore(t)}
	b := admin.NewBindings(st)
	if err := b.SetUser(context.Background(), admin.Binding{UserKey: "alice", Protocol: store.ProtocolVNV1, Name: "alice", Status: store.UserEnabled, Client: "default:edge"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultServer()
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.Auth.Key = authkey.Generate()
	sessions := transport.NewSessionManager()
	svc := core.NewConnectorService(&cfg, sessions, b, nil)
	svc.ApplyHeartbeat(0, 0)
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Drain(ctx) })
	p := inbound.NewVNRayUserPlugin(st, inbound.VNRayUserDependencies{Events: sessions, Users: b, UserConfig: b, Clients: sessions})
	t.Cleanup(p.Stop)
	port := freePort(t)
	requestCtx, cancel := context.WithCancel(ctx)
	if err := p.Enable(requestCtx, map[string]string{"port": strconv.Itoa(port)}); err != nil {
		t.Fatal(err)
	}
	cancel()
	userAddr := fmt.Sprintf("127.0.0.1:%d", port)
	user, err := userclient.Dial(userAddr, protocol.UserAuthRequest{Key: "alice"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer user.Close()
	if status := p.Status(); !status.Enabled || !status.AcceptsUsers || status.Ports[0].Protocol != store.ProtocolVNV1 {
		t.Fatalf("plugin status: %+v", status)
	}
	if u, err := userclient.Dial(svc.Addr(), protocol.UserAuthRequest{Key: "alice"}, nil, nil); err == nil {
		_ = u.Close()
		t.Fatal("main client port accepted user")
	}
	dialCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if c, err := client.DialContext(dialCtx, userAddr, nil, client.DialOptions{Auth: client.AuthOptions{Key: cfg.Server.Auth.Key}}); err == nil {
		_ = c.Close()
		t.Fatal("user port accepted internal client")
	}
	raw, err := net.DialTimeout("tcp", userAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := tcp.DefaultMuxer().Client(raw, tcp.DefaultSessionConfig())
	if err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	defer mux.Close()
	stream, err := mux.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.SetDeadline(time.Now().Add(2 * time.Second))
	report, err := protocol.NewConfigReportFrame(protocol.ConfigReportRequest{Info: protocol.ClientInfo{Group: "default", Name: "edge"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewEncoder(stream).WriteFrame(report); err != nil {
		t.Fatal(err)
	}
	select {
	case <-mux.CloseChan():
	case <-time.After(2 * time.Second):
		t.Fatal("user port did not reject config report")
	}
	dispatcher := dispatch.NewDefaultDispatcher(nil, "direct", dispatch.NewDirectOutbound("direct"))
	c, err := client.Dial(svc.Addr(), dispatcher, client.DialOptions{Auth: client.AuthOptions{Key: cfg.Server.Auth.Key}, Info: protocol.ClientInfo{Group: "default", Name: "edge"}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Serve() }()
	t.Cleanup(func() {
		_ = c.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("client did not stop")
		}
	})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "separate-port-ok") }))
	defer origin.Close()
	host, portText, _ := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	destPort, _ := strconv.Atoi(portText)
	forward := func(u *userclient.Client) {
		t.Helper()
		conn, err := u.Forward(protocol.Address{Host: host, Port: uint16(destPort)})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.WriteString(conn, "GET / HTTP/1.0\r\nHost: test\r\n\r\n")
		body, err := io.ReadAll(conn)
		if err != nil || !strings.Contains(string(body), "separate-port-ok") {
			t.Fatalf("forward response: %q, %v", body, err)
		}
	}
	forward(user)
	setter := plugin.PortSetter(p)
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if err := setter.SetPort(ctx, busy.Addr().(*net.TCPAddr).Port); err == nil {
		t.Fatal("busy port accepted")
	}
	if p.Status().Ports[0].Port != port {
		t.Fatal("failed switch changed port")
	}
	forward(user)
	nextPort := freePort(t)
	saveErr := errors.New("injected plugin save failure")
	st.saveErr = saveErr
	if err := setter.SetPort(ctx, nextPort); !errors.Is(err, saveErr) {
		t.Fatalf("switch persistence error = %v", err)
	}
	st.saveErr = nil
	if status := p.Status(); !status.Enabled || status.Ports[0].Port != port {
		t.Fatalf("failed save changed listener: %+v", status)
	}
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", nextPort), time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("failed save left new listener open")
	}
	forward(user)
	if err := setter.SetPort(ctx, nextPort); err != nil {
		t.Fatal(err)
	}
	forward(user)
	if conn, err := net.DialTimeout("tcp", userAddr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("old listener still open")
	}
	p.Stop()
	restored := inbound.NewVNRayUserPlugin(st, inbound.VNRayUserDependencies{Events: sessions, Users: b, UserConfig: b, Clients: sessions})
	restored.Restore(ctx)
	defer restored.Stop()
	if !restored.Status().Enabled || restored.Status().Ports[0].Port != nextPort {
		t.Fatalf("restore: %+v", restored.Status())
	}
	userAddr = fmt.Sprintf("127.0.0.1:%d", nextPort)
	u2, err := userclient.Dial(userAddr, protocol.UserAuthRequest{Key: "alice"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer u2.Close()
	forward(u2)
	if err := restored.Disable(ctx); err != nil {
		t.Fatal(err)
	}
	forward(u2)
	restored.Restore(ctx)
	if restored.Status().Enabled {
		t.Fatal("disabled plugin restarted")
	}
	if conn, err := net.DialTimeout("tcp", userAddr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("disabled listener still open")
	}
}
