package inbound

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/access"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/identity"
	"github.com/hhy50/vnray/internal/mux"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/rule"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/shadowsocks"
	"github.com/hhy50/vnray/vmess"
)

type testRuleEvaluator map[string]*rule.Compiled

func (r testRuleEvaluator) Verdict(name, host string) (rule.Verdict, string) {
	if name == "" {
		name = rule.NameDefault
	}
	if compiled := r[name]; compiled != nil {
		return compiled.Verdict(host), name
	}
	return rule.Pass, name
}

func inboundTestRules(t *testing.T) testRuleEvaluator {
	t.Helper()
	out := testRuleEvaluator{}
	for name, spec := range map[string]rule.Rule{
		"office":  {Reject: []string{"blocked.example"}, Direct: []string{"10.0.0.0/8"}},
		"default": {Reject: []string{"blocked.example"}},
	} {
		compiled, err := rule.Compile(name, spec)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = compiled
	}
	return out
}

type policyExit struct {
	mu       sync.Mutex
	requests []protocol.TunnelRequest
}

func (e *policyExit) OpenTunnel(_ dispatch.Destination, req protocol.TunnelRequest) (net.Conn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.requests = append(e.requests, req)
	return nil, errors.New("test exit: stop after recording tunnel request")
}

func (e *policyExit) snapshot() []protocol.TunnelRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]protocol.TunnelRequest(nil), e.requests...)
}

func TestInboundUserRules(t *testing.T) {
	const key = "b831381d-6324-4d53-ad4f-8cda48b30811"
	for _, proto := range []store.Protocol{store.ProtocolVMess, store.ProtocolVLESS, store.ProtocolSS} {
		t.Run(string(proto), func(t *testing.T) {
			st := seedTestStore(t)
			users := access.NewBindings(st)
			rules := inboundTestRules(t)
			for _, tc := range []struct {
				name, host, rule         string
				denied, direct, disabled bool
			}{
				{name: "reject", host: "blocked.example", rule: "office", denied: true},
				{name: "direct", host: "10.1.2.3", rule: "office", direct: true},
				{name: "pass", host: "allowed.example", rule: "office"},
				{name: "default", host: "blocked.example", denied: true},
				{name: "missing rule", host: "blocked.example", rule: "deleted"},
				{name: "disabled user", host: "allowed.example", rule: "office", disabled: true, denied: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					status := store.UserEnabled
					if tc.disabled {
						status = store.UserDisabled
					}
					if err := users.PutUser(context.Background(), store.UserBinding{
						Key: key, Protocol: proto, Name: "policy-user", Client: transport.LocalClientName,
						Status: status, Config: store.UserConfig{Rule: tc.rule},
					}); err != nil {
						t.Fatal(err)
					}
					// 相同 key 在另一协议下使用不同规则，避免漏传协议后串用账号配置。
					other := store.ProtocolVMess
					if other == proto {
						other = store.ProtocolVLESS
					}
					if err := users.PutUser(context.Background(), store.UserBinding{
						Key: key, Protocol: other, Name: "other-protocol-user", Client: transport.LocalClientName,
						Status: store.UserEnabled, Config: store.UserConfig{Rule: "deleted"},
					}); err != nil {
						t.Fatal(err)
					}
					for _, network := range []string{"tcp", "udp"} {
						if proto == store.ProtocolSS && network == "udp" {
							continue
						}
						exit := &policyExit{}
						exits := &stubExitResolver{local: exit}
						conn, peer := net.Pipe()
						defer peer.Close()
						if proto == store.ProtocolSS {
							p := NewSSPlugin(st, users, exits, nil)
							defer drainPlugin(t, p)
							p.SetRules(rules)
							p.handle(p.runtime.Context(), key, transport.LocalClientName, shadowsocks.Target{Host: tc.host, Port: 443}, &shadowsocks.Conn{Conn: conn})
						} else {
							p := NewV2RayPlugin(st, users, exits, nil)
							defer drainPlugin(t, p)
							p.SetRules(rules)
							p.serveConn(p.runtime.Context(), proto, identity.Identity{Key: key, Client: transport.LocalClientName}, tc.host, 443, network, conn)
						}
						requests := exit.snapshot()
						if tc.denied {
							if len(requests) != 0 {
								t.Fatalf("%s: denied target reached exit: %+v", network, requests)
							}
						} else if len(requests) != 1 || requests[0].Direct != tc.direct || (proto != store.ProtocolSS && requests[0].Network != network) {
							t.Fatalf("%s: tunnel requests = %+v, want one with direct=%v", network, requests, tc.direct)
						}
						_ = peer.SetReadDeadline(time.Now().Add(time.Second))
						if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
							t.Fatalf("%s: inbound was not closed: %v", network, err)
						}
					}
				})
			}
		})
	}
}

func TestMuxUserRulesPerTarget(t *testing.T) {
	const key = "b831381d-6324-4d53-ad4f-8cda48b30811"
	for _, proto := range []store.Protocol{store.ProtocolVMess, store.ProtocolVLESS} {
		t.Run(string(proto), func(t *testing.T) {
			st := seedTestStore(t)
			users := access.NewBindings(st)
			user := store.UserBinding{Key: key, Protocol: proto, Name: "mux-user", Status: store.UserEnabled, Client: transport.LocalClientName, Config: store.UserConfig{Rule: "office"}}
			if err := users.PutUser(context.Background(), user); err != nil {
				t.Fatal(err)
			}
			exit := &policyExit{}
			p := NewV2RayPlugin(st, users, &stubExitResolver{local: exit}, nil)
			p.SetRules(inboundTestRules(t))
			defer drainPlugin(t, p)
			conn, peer := net.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				p.serveConn(p.runtime.Context(), proto, identity.Identity{Key: key, Client: transport.LocalClientName}, vmess.MuxCoolHost, vmess.MuxCoolPort, "tcp", conn)
			}()
			defer func() {
				_ = peer.Close()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("mux handler did not exit")
				}
			}()
			for i, tc := range []struct {
				host    string
				network byte
			}{
				{host: "blocked.example", network: mux.NetworkTCP},
				{host: "blocked.example", network: mux.NetworkUDP},
				{host: "10.1.2.3", network: mux.NetworkTCP},
				{host: "10.1.2.3", network: mux.NetworkUDP},
				{host: "allowed.example", network: mux.NetworkTCP},
			} {
				muxRuleRequest(t, peer, byte(i+1), tc.network, tc.host)
			}
			requests := exit.snapshot()
			if len(requests) != 3 || !requests[0].Direct || !requests[1].Direct || requests[2].Direct || requests[1].Network != "udp" {
				t.Fatalf("mux tunnel requests = %+v", requests)
			}
			user.Config.Rule = "deleted"
			if err := users.PutUser(context.Background(), user); err != nil {
				t.Fatal(err)
			}
			muxRuleRequest(t, peer, 6, mux.NetworkTCP, "blocked.example")
			if got := exit.snapshot(); len(got) != 4 || got[3].Direct {
				t.Fatalf("existing mux session did not pick up user rule change: %+v", got)
			}
		})
	}
}

func muxRuleRequest(t *testing.T, conn net.Conn, id, network byte, host string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	meta := append([]byte{0, id, mux.StatusNew, 0, network, 0x01, 0xbb, 2, byte(len(host))}, host...)
	frame := make([]byte, 2, 2+len(meta))
	binary.BigEndian.PutUint16(frame, uint16(len(meta)))
	frame = append(frame, meta...)
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	var response [6]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil {
		t.Fatal(err)
	}
	if response != [6]byte{0, 4, 0, id, mux.StatusEnd, mux.OptionError} {
		t.Fatalf("mux result = %x, want error end for session %d", response, id)
	}
}

func TestUserPolicyUnavailable(t *testing.T) {
	for _, tc := range []string{"canceled", "missing user", "missing user store"} {
		t.Run(tc, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			policy := userPolicy{users: access.NewBindings(seedTestStore(t)), rules: inboundTestRules(t), proto: store.ProtocolVLESS, key: "missing"}
			if tc == "canceled" {
				cancel()
			} else if tc == "missing user store" {
				policy.users = nil
			}
			if direct, err := policy.check(ctx, "allowed.example"); direct || err == nil {
				t.Fatalf("policy = (%v, %v), want denial", direct, err)
			}
		})
	}
}
