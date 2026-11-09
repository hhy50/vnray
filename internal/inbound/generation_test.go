package inbound

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
)

func mustWaitGeneration(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stopped generation was not reaped")
	}
}

func TestPluginRetiresStoppedGenerations(t *testing.T) {
	for _, name := range []string{"v2ray", "ss", "vnray-user"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := seedTestStore(t)
			var p plugin.Plugin
			var current func() <-chan struct{}
			var pending func() []<-chan struct{}
			var setPort func(int) error
			want := 1
			params := map[string]string{"port": strconv.Itoa(freePort(t)), "host": "127.0.0.1"}
			switch name {
			case "v2ray":
				v := NewV2RayPlugin(st, nil, nil, nil)
				p, want = v, 2
				params = map[string]string{"vmess_port": strconv.Itoa(freePort(t)), "vless_port": strconv.Itoa(freePort(t)), "vless_tls": "off"}
				current = func() <-chan struct{} { return v.inbound(store.ProtocolVMess).run.Done() }
				pending = func() []<-chan struct{} {
					return []<-chan struct{}{v.inbound(store.ProtocolVMess).run.Done(), v.inbound(store.ProtocolVLESS).run.Done()}
				}
				setPort = func(port int) error { return v.SetPorts(ctx, map[store.Protocol]int{store.ProtocolVMess: port}) }
			case "ss":
				v := NewSSPlugin(st, nil, nil, nil)
				p = v
				current = func() <-chan struct{} { return v.run.Done() }
				pending = func() []<-chan struct{} { return []<-chan struct{}{v.run.Done()} }
				setPort = func(port int) error { return v.SetPort(ctx, port) }
			case "vnray-user":
				v := NewVNRayUserPlugin(st, VNRayUserDependencies{Events: transport.NewSessionManager()})
				p = v
				current = func() <-chan struct{} { value, _ := v.servers.Load(v.server); return value.(chan struct{}) }
				pending = func() []<-chan struct{} {
					out := make([]<-chan struct{}, 0)
					v.servers.Range(func(_, value any) bool {
						out = append(out, value.(chan struct{}))
						return true
					})
					return out
				}
				setPort = func(port int) error { return v.SetPort(ctx, port) }
			}
			t.Cleanup(func() { drainPlugin(t, p) })
			if err := p.Enable(ctx, params); err != nil {
				t.Fatal(err)
			}
			for range 12 {
				old := current()
				if err := setPort(freePort(t)); err != nil {
					t.Fatal(err)
				}
				mustWaitGeneration(t, old)
				count := len(pending())
				if count != want {
					t.Fatalf("retained generations = %d, want %d", count, want)
				}
			}
			done := pending()
			p.Stop()
			for _, ch := range done {
				mustWaitGeneration(t, ch)
			}
			if name == "vnray-user" {
				count := len(pending())
				if count != 0 {
					t.Fatalf("stopped plugin retained %d generations", count)
				}
			}
		})
	}
}

func TestPluginRetiresGenerationAfterConnection(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	exit := newRecordingExit()
	p, port := startVLESSInbound(t, &stubExitResolver{local: exit})
	t.Cleanup(func() { drainPlugin(t, p) })
	p.SyncUser(store.ProtocolVLESS, uuid, transport.LocalClientName, store.UserEnabled, false)
	conn := dialVLESS(t, port, uuid, "example.com", 443)
	var peer net.Conn
	select {
	case peer = <-exit.peers:
	case <-time.After(3 * time.Second):
		t.Fatal("tunnel not opened")
	}
	defer peer.Close()
	old := p.inbound(store.ProtocolVLESS).run
	if err := p.SetPorts(context.Background(), map[store.Protocol]int{store.ProtocolVLESS: freePort(t)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.Done():
		t.Fatal("live connection generation was reaped")
	default:
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(peer, b[:]); err != nil || b[0] != 'x' {
		t.Fatalf("hot switch interrupted old connection: %v", err)
	}
	_ = conn.Close()
	_ = peer.Close()
	mustWaitGeneration(t, old.Done())
}
