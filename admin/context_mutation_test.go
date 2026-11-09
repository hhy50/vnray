package admin

import (
	"context"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/transport"
)

type mutationContextKey struct{}

type observedMutationBindings struct {
	UserBindings
	userContext   context.Context
	clientContext context.Context
	pushContext   context.Context
	cancelCommit  context.CancelFunc
}

func (b *observedMutationBindings) UpdateUser(ctx context.Context, p store.Protocol, key string, mutate func(*Binding) error) (Binding, error) {
	b.userContext = ctx
	return b.UserBindings.UpdateUser(ctx, p, key, mutate)
}

func (b *observedMutationBindings) UpdateClientServerConfig(ctx context.Context, name string, mutate func(string) (string, error)) error {
	b.clientContext = ctx
	err := b.UserBindings.UpdateClientServerConfig(ctx, name, mutate)
	if err == nil && b.cancelCommit != nil {
		b.cancelCommit()
	}
	return err
}

func (b *observedMutationBindings) PushClientConfig(ctx context.Context, client *transport.Session) error {
	b.pushContext = ctx
	return b.UserBindings.PushClientConfig(ctx, client)
}

func TestAdminUserMutationsUseRequestContext(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		for _, endpoint := range []string{"status", "limits"} {
			name := endpoint + "/active"
			if canceled {
				name = endpoint + "/canceled"
			}
			t.Run(name, func(t *testing.T) {
				s := newTestAdminService(t)
				st := s.opts.Store.(store.Store)
				original := store.UserBinding{
					Protocol: store.ProtocolSS, Key: "alice", Name: "Alice", Status: store.UserEnabled, Client: "edge",
					Config: store.UserConfig{SSMethod: "aes-256-gcm", UpLimit: 1024, DownLimit: 2048, Concurrency: 3, LogLevel: "warn"},
				}
				if err := st.SetUser(context.Background(), original); err != nil {
					t.Fatal(err)
				}
				b := &observedMutationBindings{UserBindings: NewBindings(st)}
				s.opts.Bindings = b
				ctx, cancel := context.WithCancel(context.WithValue(context.Background(), mutationContextKey{}, name))
				defer cancel()
				if canceled {
					cancel()
				}
				var body any
				if endpoint == "limits" {
					body = apiUserLimitsIn{UpKB: 7, DownKB: 11, Concurrency: 5, LogLevel: "debug"}
				}
				w, r := apiContext(http.MethodPost, "/?protocol=ss&key=alice&enabled=false", body, nil)
				r = r.WithContext(ctx)
				if endpoint == "status" {
					s.apiUserStatus(w, r)
				} else {
					s.apiUserLimits(w, r)
				}
				if b.userContext != ctx {
					t.Fatal("handler did not pass the request context to the mutation")
				}
				wantCode := http.StatusOK
				if canceled {
					wantCode = http.StatusInternalServerError
				}
				if w.Code != wantCode {
					t.Fatalf("response = %d: %s", w.Code, w.Body.String())
				}
				got, ok, err := st.GetUser(context.Background(), original.Protocol, original.Key)
				if err != nil || !ok {
					t.Fatalf("read stored user: %v, %v", ok, err)
				}
				want := original
				if !canceled && endpoint == "status" {
					want.Status = store.UserDisabled
				}
				if !canceled && endpoint == "limits" {
					want.Config.UpLimit = 7 * 1024
					want.Config.DownLimit = 11 * 1024
					want.Config.Concurrency = 5
					want.Config.LogLevel = "debug"
				}
				if got.Name != want.Name || got.Client != want.Client || got.Status != want.Status || !reflect.DeepEqual(got.Config, want.Config) {
					t.Fatalf("mutation changed unrelated fields or lost its own changes: %+v", got)
				}
			})
		}
	}
}

func TestClientHostsPublishesAfterRequestCancellation(t *testing.T) {
	s := newTestAdminService(t)
	st := s.opts.Store.(store.Store)
	bindings := NewBindings(st)
	if err := bindings.SetClient(context.Background(), ClientBinding{ClientKey: "edge", ServerConfig: `{"log_level":"warn"}`}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), mutationContextKey{}, "hosts"))
	defer cancel()
	b := &observedMutationBindings{UserBindings: bindings, cancelCommit: cancel}
	s.opts.Bindings = b
	serverConn, peerConn := net.Pipe()
	serverSession, err := tcp.DefaultMuxer().Server(serverConn, tcp.DefaultSessionConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	peerSession, err := tcp.DefaultMuxer().Client(peerConn, tcp.DefaultSessionConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peerSession.Close() })
	client := transport.NewSession(serverSession, "pipe", protocol.ClientInfo{Name: "edge"})
	if err := s.clients.OnSessionOpen(&tcp.SessionInfo{Session: serverSession, Kind: tcp.KindClient, Name: "edge", Meta: client}); err != nil {
		t.Fatal(err)
	}
	type pushResult struct {
		config protocol.PushConfig
		err    error
	}
	received := make(chan pushResult, 1)
	go func() {
		_ = peerSession.SetDeadline(time.Now().Add(2 * time.Second))
		stream, err := peerSession.AcceptStream()
		if err != nil {
			received <- pushResult{err: err}
			return
		}
		defer stream.Close()
		_ = stream.SetDeadline(time.Now().Add(2 * time.Second))
		frame, err := protocol.NewDecoder(stream).ReadFrame()
		if err != nil {
			received <- pushResult{err: err}
			return
		}
		payload, err := protocol.ReadPayload(frame)
		if err != nil {
			received <- pushResult{err: err}
			return
		}
		cfg, err := protocol.ParsePushConfig(payload)
		received <- pushResult{config: cfg, err: err}
	}()
	w, r := apiContext(http.MethodPost, "/", apiClientHostsIn{
		Hosts: []apiHostRow{{Domain: "service.test", IP: "127.0.0.2"}}, Concurrency: 9,
	}, map[string]string{"name": "edge"})
	s.apiClientHostsSave(w, r.WithContext(ctx))
	if w.Code != http.StatusOK {
		t.Fatalf("response = %d: %s", w.Code, w.Body.String())
	}
	if ctx.Err() != context.Canceled || b.clientContext != ctx {
		t.Fatal("request cancellation did not occur after storage committed")
	}
	if b.pushContext == nil || b.pushContext.Value(mutationContextKey{}) != "hosts" {
		t.Fatal("postcommit push lost the request context values")
	}
	if deadline, ok := b.pushContext.Deadline(); !ok || deadline.After(time.Now().Add(dbTimeout)) {
		t.Fatal("postcommit push has no bounded deadline")
	}
	if client.ClientConcurrency() != 9 {
		t.Fatalf("committed concurrency was not applied: %d", client.ClientConcurrency())
	}
	select {
	case pushed := <-received:
		if pushed.err != nil || len(pushed.config.ClientConfig.CustomHosts) != 1 || pushed.config.ClientConfig.CustomHosts[0].Domain != "service.test" {
			t.Fatalf("client did not receive committed configuration: %+v, %v", pushed.config, pushed.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("committed configuration was not pushed")
	}
	savedBinding, ok := bindings.LookupClient(context.Background(), "edge")
	saved := store.ParseClientConfig(savedBinding.ServerConfig)
	if !ok || len(saved.CustomHosts) != 1 || saved.LogLevel != "warn" || saved.Concurrency != 9 {
		t.Fatalf("hosts mutation did not preserve the saved configuration: %+v", saved)
	}
}

type delayedPushBindings struct {
	UserBindings
	firstPushed chan struct{}
	release     chan struct{}
}

func (b *delayedPushBindings) PushClientConfig(ctx context.Context, client *transport.Session) error {
	err := b.UserBindings.PushClientConfig(ctx, client)
	if ctx.Value(mutationContextKey{}) == "first" {
		close(b.firstPushed)
		select {
		case <-b.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func TestClientHostsDelayedResponseCannotRestoreStaleConcurrency(t *testing.T) {
	s := newTestAdminService(t)
	bindings := NewBindings(s.opts.Store.(store.Store))
	if err := bindings.SetClient(context.Background(), ClientBinding{ClientKey: "edge"}); err != nil {
		t.Fatal(err)
	}
	b := &delayedPushBindings{UserBindings: bindings, firstPushed: make(chan struct{}), release: make(chan struct{})}
	s.opts.Bindings = b
	serverConn, peerConn := net.Pipe()
	serverSession, err := tcp.DefaultMuxer().Server(serverConn, tcp.DefaultSessionConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	peerSession, err := tcp.DefaultMuxer().Client(peerConn, tcp.DefaultSessionConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peerSession.Close() })
	client := transport.NewSession(serverSession, "pipe", protocol.ClientInfo{Name: "edge"})
	if err := s.clients.OnSessionOpen(&tcp.SessionInfo{Session: serverSession, Kind: tcp.KindClient, Name: "edge", Meta: client}); err != nil {
		t.Fatal(err)
	}
	received := make(chan error, 1)
	go func() {
		for range 2 {
			_ = peerSession.SetDeadline(time.Now().Add(5 * time.Second))
			stream, err := peerSession.AcceptStream()
			if err != nil {
				received <- err
				return
			}
			_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
			frame, err := protocol.NewDecoder(stream).ReadFrame()
			if err == nil {
				_, err = protocol.ReadPayload(frame)
			}
			_ = stream.Close()
			if err != nil {
				received <- err
				return
			}
		}
		received <- nil
	}()
	firstW, firstR := apiContext(http.MethodPost, "/", apiClientHostsIn{Concurrency: 5}, map[string]string{"name": "edge"})
	firstR = firstR.WithContext(context.WithValue(context.Background(), mutationContextKey{}, "first"))
	firstDone := make(chan struct{})
	go func() {
		s.apiClientHostsSave(firstW, firstR)
		close(firstDone)
	}()
	select {
	case <-b.firstPushed:
	case <-time.After(5 * time.Second):
		t.Fatal("first update did not publish")
	}
	secondW, secondR := apiContext(http.MethodPost, "/", apiClientHostsIn{Concurrency: 9}, map[string]string{"name": "edge"})
	s.apiClientHostsSave(secondW, secondR)
	close(b.release)
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not finish")
	}
	if firstW.Code != http.StatusOK || secondW.Code != http.StatusOK {
		t.Fatalf("responses: first=%d %s, second=%d %s", firstW.Code, firstW.Body.String(), secondW.Code, secondW.Body.String())
	}
	if got := client.ClientConcurrency(); got != 9 {
		t.Fatalf("older response restored concurrency %d; want latest committed value 9", got)
	}
	select {
	case err := <-received:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("configuration pushes did not finish")
	}
}
