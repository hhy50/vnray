package core

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/access"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
)

type pausedSettingsStore struct {
	store.Store
	started chan struct{}
	release chan struct{}
	paused  atomic.Bool
}

func (s *pausedSettingsStore) SetServiceSettings(ctx context.Context, values map[string]string) error {
	if s.paused.CompareAndSwap(false, true) {
		close(s.started)
		<-s.release
	}
	return s.Store.SetServiceSettings(ctx, values)
}

func newTestServiceConfig(st store.Store, initial config.ServiceSettings) *serviceConfig {
	cfg := config.DefaultServer()
	limits := ratelimit.NewRegistry()
	svc := NewConnectorService(&cfg, transport.NewSessionManager(), nil, limits)
	return newServiceConfig(st, svc, traffic.NewMeter(limits), nil, nil, initial)
}

func TestServiceConfigUpdatesDoNotWaitForPendingSave(t *testing.T) {
	st := &pausedSettingsStore{Store: testStore(t), started: make(chan struct{}), release: make(chan struct{})}
	sc := newTestServiceConfig(st, config.DefaultServiceSettings())
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(st.release) }) }
	defer release()
	first := make(chan error, 1)
	go func() {
		first <- sc.Update(context.Background(), func(next *config.ServiceSettings) error {
			next.LogLevel = "warn"
			return nil
		})
	}()
	select {
	case <-st.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first update did not reach storage")
	}
	second := make(chan error, 1)
	go func() {
		second <- sc.Update(context.Background(), func(next *config.ServiceSettings) error {
			next.GlobalClient.CustomHosts = []config.CustomHost{{Domain: "service.test", IP: "127.0.0.1"}}
			return nil
		})
	}()
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second update waited for pending save")
	}
	if got := sc.Current(); len(got.GlobalClient.CustomHosts) != 1 {
		t.Fatalf("second update was not published: %+v", got)
	}
	release()
	select {
	case err := <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first update did not finish")
	}
	got := sc.Current()
	if got.LogLevel != "warn" || len(got.GlobalClient.CustomHosts) != 0 {
		t.Fatalf("last completed update was not published: %+v", got)
	}
	kv, err := st.GetServiceSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := config.DecodeServiceSettings(kv, config.DefaultServiceSettings())
	if err != nil || persisted.LogLevel != "warn" || len(persisted.GlobalClient.CustomHosts) != 0 {
		t.Fatalf("stored settings differ: %+v, %v", persisted, err)
	}
}

func TestServiceConfigStaleApplyPreservesTLS(t *testing.T) {
	sc := newTestServiceConfig(testStore(t), config.DefaultServiceSettings())
	stale := sc.Current()
	want := config.VLESSTLS{Enabled: true, ServerName: "service.test", CertPEM: "certificate", KeyPEM: "key"}
	if err := sc.SetVLESSTLS(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	stale.LogLevel = "warn"
	if err := sc.Apply(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if got := sc.VLESSTLS(); got == nil || *got != want {
		t.Fatalf("stale settings overwrote TLS: %+v", got)
	}
}

func TestServiceConfigSnapshotsDoNotAlias(t *testing.T) {
	initial := config.DefaultServiceSettings()
	initial.VLESS.TLS = &config.VLESSTLS{ServerName: "service.test"}
	initial.ServerProxy.Routing.Rules = []config.Rule{{Name: "local", Outbound: "direct", Match: config.Match{Host: []string{"127.0.0.1"}}}}
	sc := newTestServiceConfig(testStore(t), initial)
	initial.VLESS.TLS.ServerName = "changed.test"
	initial.ServerProxy.Routing.Rules[0].Match.Host[0] = "127.0.0.2"
	snapshot := sc.Current()
	snapshot.VLESS.TLS.ServerName = "snapshot.test"
	snapshot.ServerProxy.Routing.Rules[0].Match.Host[0] = "127.0.0.3"
	got := sc.Current()
	if got.VLESS.TLS.ServerName != "service.test" || got.ServerProxy.Routing.Rules[0].Match.Host[0] != "127.0.0.1" {
		t.Fatalf("settings snapshot aliases runtime: %+v", got)
	}
}

type sessionConfigRecorder struct{ config *tcp.SessionConfig }

func (r *sessionConfigRecorder) ApplySessionConfig(cfg *tcp.SessionConfig) {
	copy := *cfg
	r.config = &copy
}

func TestServiceConfigUpdatesUserInboundSessions(t *testing.T) {
	sc := newTestServiceConfig(testStore(t), config.DefaultServiceSettings())
	recorder := &sessionConfigRecorder{}
	sc.setSessionConfigApplier(recorder)
	if err := sc.Update(context.Background(), func(next *config.ServiceSettings) error {
		next.Session.AcceptBacklog = 512
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if recorder.config == nil || recorder.config.AcceptBacklog != 512 {
		t.Fatalf("user inbound was not updated: %+v", recorder.config)
	}
}

type pausedConfigSession struct {
	tcp.Session
	opened  chan struct{}
	release chan struct{}
	closed  chan struct{}
}

func (s *pausedConfigSession) CloseChan() <-chan struct{} { return s.closed }

func (s *pausedConfigSession) OpenStreamContext(ctx context.Context) (tcp.Stream, error) {
	close(s.opened)
	select {
	case <-s.release:
		return nil, io.ErrClosedPipe
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestServiceConfigPushDoesNotBlockTLSUpdate(t *testing.T) {
	st := testStore(t)
	limits := ratelimit.NewRegistry()
	cfg := config.DefaultServer()
	manager := transport.NewSessionManager()
	svc := NewConnectorService(&cfg, manager, nil, limits)
	conn := &pausedConfigSession{opened: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(conn.release) }) }
	defer release()
	client := transport.NewSession(conn, "test", protocol.ClientInfo{Name: "test"})
	if err := manager.OnSessionOpen(&tcp.SessionInfo{Session: conn, Kind: tcp.KindClient, Name: "test", Meta: client}); err != nil {
		t.Fatal(err)
	}
	sc := newServiceConfig(st, svc, traffic.NewMeter(limits), nil, access.NewBindings(st), config.DefaultServiceSettings())
	updated := make(chan error, 1)
	go func() {
		updated <- sc.Update(context.Background(), func(next *config.ServiceSettings) error {
			next.GlobalClient.CustomHosts = []config.CustomHost{{Domain: "service.test", IP: "127.0.0.1"}}
			return nil
		})
	}()
	select {
	case <-conn.opened:
	case <-time.After(2 * time.Second):
		t.Fatal("configuration push did not start")
	}
	tlsUpdated := make(chan error, 1)
	go func() {
		tlsUpdated <- sc.SetVLESSTLS(context.Background(), config.VLESSTLS{ServerName: "service.test"})
	}()
	select {
	case err := <-tlsUpdated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("network push blocked the TLS update")
	}
	select {
	case err := <-updated:
		t.Fatalf("push returned before release: %v", err)
	default:
	}
	release()
	select {
	case err := <-updated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("configuration update did not finish")
	}
}
