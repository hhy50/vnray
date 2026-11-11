package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/secure"
	"github.com/hhy50/vnray/internal/tcp"
)

func TestClientReportIdentityFields(t *testing.T) {
	for _, authorization := range []string{"managed", "fallback"} {
		t.Run(authorization, func(t *testing.T) {
			b := testBindings(t)
			key := authkey.Generate()
			fallback := key
			if authorization == "managed" {
				if err := b.SetAuthKey(context.Background(), key, "*"); err != nil {
					t.Fatal(err)
				}
				fallback = ""
			}
			svc := newSecureTestServer(t, b, fallback)
			for _, tc := range []struct {
				group string
				name  string
				allow bool
			}{
				{"default", "edge", true},
				{"office", "edge", true},
				{"", "default:edge", false},
				{"", "default:default:edge", false},
				{"", "edge", false},
				{"", "", false},
				{"office", "", false},
				{"office", "*", false},
				{"office*", "edge", false},
				{"office:lab", "edge", false},
				{"office", "lab:edge", false},
				{"server", "self", false},
			} {
				t.Run(tc.group+"/"+tc.name, func(t *testing.T) {
					info := protocol.ClientInfo{Group: tc.group, Name: tc.name}
					before, existed := b.LookupClient(context.Background(), info.ClientKey())
					session := newReportSession(t, svc, key)
					reply, err := exchangeReport(t, session, info)
					if err != nil || reply.OK != tc.allow {
						t.Fatalf("report name %q: reply=%+v, err=%v, want allowed=%v", tc.name, reply, err, tc.allow)
					}
					after, found := b.LookupClient(context.Background(), info.ClientKey())
					if tc.allow {
						var saved protocol.ClientInfo
						if !found || json.Unmarshal([]byte(after.Config), &saved) != nil || saved.Group != tc.group || saved.Name != tc.name {
							t.Fatalf("reported identity not saved: %+v", after)
						}
					} else if found != existed || !reflect.DeepEqual(before, after) {
						t.Fatal("rejected report changed the binding")
					}
				})
			}
		})
	}
}

func newReportSession(t *testing.T, svc *ConnectorService, keyText string) tcp.Session {
	t.Helper()
	key, err := authkey.Parse(keyText)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.DialTimeout("tcp", svc.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := secure.Client(context.Background(), raw, key)
	if err != nil {
		t.Fatal(err)
	}
	session, err := tcp.DefaultMuxer().Client(conn, tcp.DefaultSessionConfig())
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func exchangeReport(t *testing.T, session tcp.Session, info protocol.ClientInfo) (protocol.Reply, error) {
	t.Helper()
	stream, err := session.OpenStream()
	if err != nil {
		return protocol.Reply{}, err
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(2 * time.Second))
	frame, err := protocol.NewConfigReportFrame(protocol.ConfigReportRequest{Info: info})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewEncoder(stream).WriteFrame(frame); err != nil {
		return protocol.Reply{}, err
	}
	response, err := protocol.NewDecoder(stream).ReadFrame()
	if err != nil {
		return protocol.Reply{}, err
	}
	if response.Type != protocol.ConfigReportResult {
		t.Fatalf("response type = %d", response.Type)
	}
	payload, err := protocol.ReadPayload(response)
	if err != nil {
		return protocol.Reply{}, err
	}
	return protocol.ParseReply(payload)
}

func TestConfigReportsRefreshMetadataAndConfig(t *testing.T) {
	b := testBindings(t)
	key := authkey.Generate()
	svc := newSecureTestServer(t, b, key)
	session := newReportSession(t, svc, key)
	info := protocol.ClientInfo{Group: "team", Name: "edge", Hostname: "initial"}
	first, err := exchangeReport(t, session, info)
	if err != nil || !first.OK || first.Config == nil || first.Config.Revision == 0 {
		t.Fatalf("initial report: %+v, %v", first, err)
	}
	binding, ok := b.LookupClient(context.Background(), info.ClientKey())
	if !ok {
		t.Fatal("initial report was not saved")
	}
	binding.ServerConfig = `{"custom_hosts":[{"domain":"test.local","ip":"127.0.0.2"}]}`
	if err := b.SetClient(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	info.Hostname = "updated"
	second, err := exchangeReport(t, session, info)
	if err != nil || !second.OK || second.Config == nil {
		t.Fatalf("second report: %+v, %v", second, err)
	}
	if second.Config.Revision <= first.Config.Revision || len(second.Config.ClientConfig.CustomHosts) != 1 {
		t.Fatalf("server config did not refresh: %+v", second.Config)
	}
	live, ok := svc.Sessions().SessionByName(info.ClientKey())
	if !ok || clientOf(live).Info().Hostname != "updated" {
		t.Fatal("live metadata did not refresh")
	}
	binding, _ = b.LookupClient(context.Background(), info.ClientKey())
	var saved protocol.ClientInfo
	if err := json.Unmarshal([]byte(binding.Config), &saved); err != nil || saved.Hostname != "updated" || saved.Group != "team" || saved.Name != "edge" {
		t.Fatalf("saved metadata: %+v, %v", saved, err)
	}
	if svc.Sessions().ClientCount() != 1 {
		t.Fatal("repeated report created another client")
	}
}

func TestConfigReportAuthorization(t *testing.T) {
	for _, change := range []string{"rename", "regroup", "revoked", "scope"} {
		t.Run(change, func(t *testing.T) {
			b := testBindings(t)
			key := authkey.Generate()
			if err := b.SetAuthKey(context.Background(), key, "*"); err != nil {
				t.Fatal(err)
			}
			svc := newSecureTestServer(t, b, "")
			session := newReportSession(t, svc, key)
			info := protocol.ClientInfo{Group: "team", Name: "edge", Hostname: "original"}
			if reply, err := exchangeReport(t, session, info); err != nil || !reply.OK {
				t.Fatalf("initial report: %+v, %v", reply, err)
			}
			switch change {
			case "rename":
				info.Name = "other"
			case "regroup":
				info.Group = "other"
			case "revoked":
				if _, err := b.DeleteAuthKey(context.Background(), key); err != nil {
					t.Fatal(err)
				}
			case "scope":
				if err := b.SetAuthKey(context.Background(), key, "other:*"); err != nil {
					t.Fatal(err)
				}
			}
			info.Hostname = "unauthorized"
			if reply, err := exchangeReport(t, session, info); err == nil && reply.OK {
				t.Fatal("unauthorized report accepted")
			}
			select {
			case <-session.CloseChan():
			case <-time.After(2 * time.Second):
				t.Fatal("rejected report did not close session")
			}
			binding, _ := b.LookupClient(context.Background(), "team:edge")
			var saved protocol.ClientInfo
			if err := json.Unmarshal([]byte(binding.Config), &saved); err != nil || saved.Hostname != "original" {
				t.Fatalf("rejected report overwrote metadata: %+v, %v", saved, err)
			}
		})
	}
}

func TestClientReportsSameNameAcrossGroups(t *testing.T) {
	b := testBindings(t)
	key := authkey.Generate()
	svc := newSecureTestServer(t, b, key)
	for i, group := range []string{"office", "lab"} {
		info := protocol.ClientInfo{Group: group, Name: "edge"}
		settings := fmt.Sprintf(`{"concurrency":%d,"custom_hosts":[{"domain":"test.local","ip":"127.0.0.%d"}]}`, i+1, i+2)
		if err := b.SetClient(context.Background(), admin.ClientBinding{ClientKey: info.ClientKey(), ServerConfig: settings}); err != nil {
			t.Fatal(err)
		}
		session := newReportSession(t, svc, key)
		for report := 0; report < 2; report++ {
			reply, err := exchangeReport(t, session, info)
			if err != nil || !reply.OK || reply.Config == nil {
				t.Fatalf("report %s: %+v, %v", group, reply, err)
			}
			hosts := reply.Config.ClientConfig.CustomHosts
			if len(hosts) != 1 || hosts[0].IP != fmt.Sprintf("127.0.0.%d", i+2) {
				t.Fatalf("wrong configuration for %s: %+v", group, hosts)
			}
		}
		live, ok := svc.Sessions().SessionByName(info.ClientKey())
		if !ok || clientOf(live).ClientConcurrency() != i+1 || clientOf(live).Info().Group != group {
			t.Fatalf("wrong live client for %s", group)
		}
		binding, ok := b.LookupClient(context.Background(), info.ClientKey())
		var saved protocol.ClientInfo
		if !ok || json.Unmarshal([]byte(binding.Config), &saved) != nil || saved.Group != group || saved.Name != "edge" {
			t.Fatalf("wrong stored client for %s: %+v", group, binding)
		}
	}
	if svc.Sessions().ClientCount() != 2 {
		t.Fatal("clients in different groups replaced each other")
	}
}
