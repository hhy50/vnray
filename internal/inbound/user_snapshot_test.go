package inbound

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/hhy50/vnray/internal/access"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
)

type cancelPluginCommitStore struct {
	store.Store
	cancel context.CancelFunc
}

func (s *cancelPluginCommitStore) SetPlugin(ctx context.Context, state store.PluginState) error {
	if err := s.Store.SetPlugin(ctx, state); err != nil {
		return err
	}
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

func TestSSMethodChangePublishesPreparedUsersAfterCommitCancellation(t *testing.T) {
	const password = "committed-password"
	st := &cancelPluginCommitStore{Store: seedTestStore(t)}
	if err := st.SetUser(context.Background(), store.UserBinding{Key: password, Name: "committed-user", Protocol: store.ProtocolSS, Status: store.UserEnabled, Client: transport.LocalClientName}); err != nil {
		t.Fatal(err)
	}
	exit := newRecordingExit()
	p := NewSSPlugin(st, access.NewBindings(st), &stubExitResolver{local: exit}, nil)
	t.Cleanup(func() { drainPlugin(t, p) })
	if err := p.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	params := map[string]string{"port": strconv.Itoa(port)}
	if err := p.Enable(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	run := p.run
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st.cancel = cancel
	params["method"] = "aes-128-gcm"
	if err := p.Enable(ctx, params); err != nil {
		t.Fatal(err)
	}
	state, _, err := st.GetPlugin(context.Background(), pluginSS)
	if ctx.Err() != context.Canceled || err != nil || state.Config.Method != params["method"] || p.method != params["method"] || p.run != run {
		t.Fatalf("ctx=%v state=%+v method=%s same listener=%v err=%v", ctx.Err(), state, p.method, p.run == run, err)
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	writeSSAES128Request(t, conn, password)
	if _, err := io.ReadFull(conn, make([]byte, 16)); err != nil {
		t.Fatalf("new method authentication failed after commit: %v", err)
	}
	select {
	case peer := <-exit.peers:
		defer peer.Close()
		_ = peer.SetReadDeadline(time.Now().Add(time.Second))
		payload := make([]byte, len("probe"))
		if _, err := io.ReadFull(peer, payload); err != nil || string(payload) != "probe" {
			t.Fatalf("relay payload = %q, err = %v", payload, err)
		}
	case <-time.After(time.Second):
		t.Fatal("authenticated user did not reach its configured exit")
	}
}

func writeSSAES128Request(t *testing.T, conn net.Conn, password string) {
	t.Helper()
	master := md5.Sum([]byte(password))
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 16)
	if _, err := io.ReadFull(hkdf.New(sha1.New, master[:], salt, []byte("ss-subkey")), key); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	payload := append([]byte{1, 127, 0, 0, 1, 0, 80}, []byte("probe")...)
	nonce := make([]byte, aead.NonceSize())
	frame := aead.Seal(salt, nonce, []byte{0, byte(len(payload))}, nil)
	nonce[0] = 1
	frame = aead.Seal(frame, nonce, payload, nil)
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
}

type failingUserListStore struct {
	store.Store
	err    error
	cancel context.CancelFunc
}

func (s *failingUserListStore) ListUsers(ctx context.Context) ([]store.UserBinding, error) {
	if s.cancel != nil {
		s.cancel()
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.ListUsers(ctx)
}

func TestInboundRestorePropagatesUserReadFailure(t *testing.T) {
	for _, name := range []string{pluginSS, pluginV2Ray} {
		for _, failure := range []string{"read", "cancel"} {
			t.Run(name+"/"+failure, func(t *testing.T) {
				st := &failingUserListStore{Store: seedTestStore(t)}
				port := freePort(t)
				state := store.PluginState{Name: name, Status: store.PluginEnabled, Config: store.PluginConfig{Ports: map[store.Protocol]int{store.ProtocolSS: port, store.ProtocolVMess: port, store.ProtocolVLESS: freePort(t)}}}
				if err := st.SetPlugin(context.Background(), state); err != nil {
					t.Fatal(err)
				}
				var p plugin.Plugin
				if name == pluginSS {
					p = NewSSPlugin(st, access.NewBindings(st), nil, nil)
				} else {
					p = NewV2RayPlugin(st, access.NewBindings(st), nil, nil)
				}
				t.Cleanup(func() { drainPlugin(t, p) })
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				wantErr := errors.New("user list unavailable")
				if failure == "cancel" {
					st.cancel = cancel
					wantErr = context.Canceled
				} else {
					st.err = wantErr
				}
				if err := p.Restore(ctx); !errors.Is(err, wantErr) {
					t.Fatalf("restore error = %v, want %v", err, wantErr)
				}
				if p.Status().Enabled || p.Status().LastError == "" {
					t.Fatalf("restore failure status = %+v", p.Status())
				}
				assertPluginPort(t, port, false)
			})
		}
	}
}

func TestSSMethodChangeReadFailurePreservesCommittedState(t *testing.T) {
	st := &failingUserListStore{Store: seedTestStore(t)}
	p := NewSSPlugin(st, access.NewBindings(st), nil, nil)
	t.Cleanup(func() { drainPlugin(t, p) })
	port := freePort(t)
	params := map[string]string{"port": strconv.Itoa(port)}
	if err := p.Enable(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	run := p.run
	wantErr := errors.New("user list unavailable")
	st.err = wantErr
	params["method"] = "aes-128-gcm"
	if err := p.Enable(context.Background(), params); !errors.Is(err, wantErr) {
		t.Fatalf("method change error = %v, want %v", err, wantErr)
	}
	state, _, err := st.GetPlugin(context.Background(), pluginSS)
	if err != nil || state.Config.Method != DefaultSSMethod || p.method != DefaultSSMethod || p.run != run {
		t.Fatalf("state=%+v method=%s same listener=%v err=%v", state, p.method, p.run == run, err)
	}
	assertPluginPort(t, port, true)
}
