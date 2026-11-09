package inbound

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/mux"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
	"gorm.io/gorm"
)

var errPluginSave = errors.New("injected plugin write failure")

type failingPluginStore struct {
	store.Store
	fail atomic.Bool
}

func (s *failingPluginStore) GORM() *gorm.DB { return s.Store.(store.GORMSource).GORM() }

func (s *failingPluginStore) SetPlugin(ctx context.Context, state store.PluginState) error {
	if s.fail.Load() {
		return errPluginSave
	}
	return s.Store.SetPlugin(ctx, state)
}

// freePort 取一个当下空闲的回环端口，供测试监听用。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func drainPlugin(t *testing.T, p plugin.Plugin) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Drain(ctx); err != nil {
		t.Errorf("drain: %v", err)
	}
}

func assertPluginPort(t *testing.T, port int, open bool) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err == nil {
		_ = conn.Close()
	}
	if (err == nil) != open {
		t.Fatalf("port %d open=%v, want %v", port, err == nil, open)
	}
}

func TestV2RaySetSinglePortAndPersistenceFailure(t *testing.T) {
	st := &failingPluginStore{Store: seedTestStore(t)}
	p := NewV2RayPlugin(st, nil, nil, nil)
	t.Cleanup(func() { drainPlugin(t, p) })
	vmessPort, vlessPort := freePort(t), freePort(t)
	if err := p.Enable(context.Background(), map[string]string{"vmess_port": strconv.Itoa(vmessPort), "vless_port": strconv.Itoa(vlessPort), "vless_tls": "off"}); err != nil {
		t.Fatal(err)
	}
	unchanged := p.inbound(store.ProtocolVLESS).run
	nextPort := freePort(t)
	if err := p.SetPorts(context.Background(), map[store.Protocol]int{store.ProtocolVMess: nextPort}); err != nil {
		t.Fatal(err)
	}
	if p.inbound(store.ProtocolVLESS).run != unchanged {
		t.Fatal("unchanged inbound restarted")
	}
	assertPluginPort(t, vmessPort, false)
	assertPluginPort(t, nextPort, true)
	assertPluginPort(t, vlessPort, true)
	state, _, err := st.GetPlugin(context.Background(), pluginV2Ray)
	if err != nil {
		t.Fatal(err)
	}
	failedPort := freePort(t)
	st.fail.Store(true)
	if err := p.SetPorts(context.Background(), map[store.Protocol]int{store.ProtocolVMess: failedPort}); !errors.Is(err, errPluginSave) {
		t.Fatalf("set ports: %v", err)
	}
	assertPluginPort(t, nextPort, true)
	assertPluginPort(t, failedPort, false)
	current, _, err := st.GetPlugin(context.Background(), pluginV2Ray)
	if err != nil || !reflect.DeepEqual(state, current) {
		t.Fatalf("persisted state changed after failure: %v", err)
	}
	if !p.Status().Enabled || p.Status().LastError == "" {
		t.Fatal("failed change lost running state or error")
	}
}

func TestPluginEnableFailurePreservesListener(t *testing.T) {
	for _, name := range []string{"ss", "v2ray"} {
		t.Run(name, func(t *testing.T) {
			st := &failingPluginStore{Store: seedTestStore(t)}
			var p plugin.Plugin
			port := freePort(t)
			params := map[string]string{"port": strconv.Itoa(port), "host": "127.0.0.1"}
			switch name {
			case "ss":
				p = NewSSPlugin(st, nil, nil, nil)
			case "v2ray":
				p = NewV2RayPlugin(st, nil, nil, nil)
				params = map[string]string{"vmess_port": strconv.Itoa(port), "vless_port": strconv.Itoa(freePort(t)), "vless_tls": "off"}
			}
			t.Cleanup(func() { drainPlugin(t, p) })
			if err := p.Enable(context.Background(), params); err != nil {
				t.Fatal(err)
			}
			old, _, err := st.GetPlugin(context.Background(), name)
			if err != nil {
				t.Fatal(err)
			}
			busy, err := net.Listen("tcp", ":0")
			if err != nil {
				t.Fatal(err)
			}
			defer busy.Close()
			key := "port"
			if name == "v2ray" {
				key = "vmess_port"
			}
			params[key] = strconv.Itoa(busy.Addr().(*net.TCPAddr).Port)
			if err := p.Enable(context.Background(), params); err == nil {
				t.Fatal("busy port accepted")
			}
			assertPluginPort(t, port, true)
			current, _, _ := st.GetPlugin(context.Background(), name)
			if !reflect.DeepEqual(old, current) {
				t.Fatal("busy port changed persisted state")
			}
			newPort := freePort(t)
			params[key] = strconv.Itoa(newPort)
			st.fail.Store(true)
			if err := p.Enable(context.Background(), params); !errors.Is(err, errPluginSave) {
				t.Fatalf("enable: %v", err)
			}
			assertPluginPort(t, port, true)
			assertPluginPort(t, newPort, false)
		})
	}
}

type failingTLSStore struct {
	settings config.VLESSTLS
	fail     bool
}

func (s *failingTLSStore) VLESSTLS() *config.VLESSTLS { result := s.settings; return &result }
func (s *failingTLSStore) SetVLESSTLS(_ context.Context, v config.VLESSTLS) error {
	if s.fail {
		return errPluginSave
	}
	s.settings = v
	return nil
}

func TestVLESSTLSChangePreservesListener(t *testing.T) {
	p := NewV2RayPlugin(seedTestStore(t), nil, nil, nil)
	t.Cleanup(func() { drainPlugin(t, p) })
	p.tlsStore = &failingTLSStore{}
	port := freePort(t)
	if err := p.Enable(context.Background(), map[string]string{"vmess_port": strconv.Itoa(freePort(t)), "vless_port": strconv.Itoa(port), "vless_tls": "off"}); err != nil {
		t.Fatal(err)
	}
	in := p.inbound(store.ProtocolVLESS)
	run := in.run
	if err := p.SetTLS(context.Background(), true, "vnray.local"); err != nil {
		t.Fatal(err)
	}
	if in.run != run {
		t.Fatal("TLS update restarted listener")
	}
	conn, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{InsecureSkipVerify: true, ServerName: "vnray.local"})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	p.tlsStore.(*failingTLSStore).fail = true
	if err := p.SetTLS(context.Background(), false, ""); !errors.Is(err, errPluginSave) {
		t.Fatalf("set TLS: %v", err)
	}
	if !p.Status().TLS.Enabled || in.run != run {
		t.Fatal("failed TLS update changed running listener")
	}
}

func TestPluginDrainCancelsQueuedConnections(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	limits := ratelimit.NewRegistry()
	identity := ratelimit.Identity(store.ProtocolVLESS, uuid)
	limits.Concurrency(identity, 1)
	gate := limits.Gate(identity)
	if err := gate.Acquire(nil); err != nil {
		t.Fatal(err)
	}
	defer gate.Release()
	p := NewV2RayPlugin(seedTestStore(t), nil, &stubExitResolver{local: newRecordingExit()}, limits)
	t.Cleanup(func() { drainPlugin(t, p) })
	port := freePort(t)
	if err := p.Enable(context.Background(), map[string]string{"vmess_port": strconv.Itoa(freePort(t)), "vless_port": strconv.Itoa(port)}); err != nil {
		t.Fatal(err)
	}
	p.SyncUser(store.ProtocolVLESS, uuid, transport.LocalClientName, store.UserEnabled, false)
	conn := dialVLESS(t, port, uuid, "example.com", 443)
	old := p.inbound(store.ProtocolVLESS).run
	if err := p.SetPorts(context.Background(), map[store.Protocol]int{store.ProtocolVLESS: freePort(t)}); err != nil {
		t.Fatal(err)
	}
	drainPlugin(t, p)
	select {
	case <-old.Done():
	default:
		t.Fatal("old listener still running")
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := conn.Read(b[:]); err == nil {
		t.Fatal("queued connection remained open")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("drain left queued connection blocked")
	}
	if err := p.Enable(context.Background(), map[string]string{"vmess_port": strconv.Itoa(freePort(t)), "vless_port": strconv.Itoa(freePort(t))}); !errors.Is(err, plugin.ErrDraining) {
		t.Fatalf("enable after drain: %v", err)
	}
}

func TestPluginStopKeepsEstablishedConnection(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	exit := newRecordingExit()
	p, port := startVLESSInbound(t, &stubExitResolver{local: exit})
	t.Cleanup(func() { drainPlugin(t, p) })
	p.SyncUser(store.ProtocolVLESS, uuid, transport.LocalClientName, store.UserEnabled, false)
	conn := dialVLESS(t, port, uuid, "example.com", 443)
	var peer net.Conn
	select {
	case peer = <-exit.peers:
	case <-time.After(time.Second):
		t.Fatal("tunnel not opened")
	}
	defer peer.Close()
	p.Stop()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_ = peer.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(peer, b[:]); err != nil || b[0] != 'x' {
		t.Fatalf("stop interrupted existing connection: %v", err)
	}
	drainPlugin(t, p)
	if _, err := peer.Read(b[:]); err == nil {
		t.Fatal("drain did not terminate tunnel")
	}
}

func TestManagerPropagatesRestoreFailure(t *testing.T) {
	st := seedTestStore(t)
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if err := st.SetPlugin(context.Background(), store.PluginState{Name: pluginSS, Status: store.PluginEnabled, Config: store.PluginConfig{Ports: map[store.Protocol]int{store.ProtocolSS: busy.Addr().(*net.TCPAddr).Port}}}); err != nil {
		t.Fatal(err)
	}
	p := NewSSPlugin(st, nil, nil, nil)
	m := plugin.NewRegistry(context.Background(), p)
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("restore failure was hidden")
	}
	if p.Status().Enabled || p.Status().LastError == "" {
		t.Fatal("restore status lacks failure")
	}
	drainPlugin(t, p)
}

type observingMuxHandler struct {
	tunnel   *muxTunnel
	entered  chan struct{}
	finished chan struct{}
}

func (h *observingMuxHandler) Open(target mux.Target, conn *mux.Conn) {
	close(h.entered)
	defer close(h.finished)
	h.tunnel.Open(target, conn)
}

func TestMuxDisconnectCancelsPluginQueue(t *testing.T) {
	limits := ratelimit.NewRegistry()
	limits.Concurrency("mux-user", 1)
	gate := limits.Gate("mux-user")
	if err := gate.Acquire(nil); err != nil {
		t.Fatal(err)
	}
	defer gate.Release()
	handler := &observingMuxHandler{
		tunnel:  &muxTunnel{log: logging.NewScoped(""), limits: limits, identity: "mux-user", sess: newRecordingExit()},
		entered: make(chan struct{}), finished: make(chan struct{}),
	}
	client, peer := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() { done <- mux.NewServer(handler).Serve(peer) }()
	meta := []byte{0, 1, mux.StatusNew, 0, mux.NetworkTCP, 0, 80, 1, 127, 0, 0, 1}
	frame := make([]byte, 2, len(meta)+2)
	binary.BigEndian.PutUint16(frame, uint16(len(meta)))
	frame = append(frame, meta...)
	_ = client.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := client.Write(frame); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handler.entered:
	case <-time.After(time.Second):
		t.Fatal("mux handler did not start")
	}
	_ = client.Close()
	select {
	case <-handler.finished:
	case <-time.After(time.Second):
		t.Fatal("mux disconnect left plugin queue blocked")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("mux server did not wait and exit")
	}
	if gate.Usage() != 1 {
		t.Fatalf("queued handler changed held slot: %d", gate.Usage())
	}
}
