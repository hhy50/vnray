package core

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/secure"
	"github.com/hhy50/vnray/internal/tcp"
)

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
	info := protocol.ClientInfo{Name: "team:g:edge", Hostname: "initial"}
	first, err := exchangeReport(t, session, info)
	if err != nil || !first.OK || first.Config == nil || first.Config.Revision == 0 {
		t.Fatalf("initial report: %+v, %v", first, err)
	}
	binding, ok := b.LookupClient(context.Background(), info.Name)
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
	live, ok := svc.Sessions().SessionByName(info.Name)
	if !ok || clientOf(live).Info().Hostname != "updated" {
		t.Fatal("live metadata did not refresh")
	}
	binding, _ = b.LookupClient(context.Background(), info.Name)
	var saved protocol.ClientInfo
	if err := json.Unmarshal([]byte(binding.Config), &saved); err != nil || saved.Hostname != "updated" {
		t.Fatalf("saved metadata: %+v, %v", saved, err)
	}
	if svc.Sessions().ClientCount() != 1 {
		t.Fatal("repeated report created another client")
	}
}

func TestConfigReportAuthorization(t *testing.T) {
	for _, change := range []string{"rename", "revoked", "scope"} {
		t.Run(change, func(t *testing.T) {
			b := testBindings(t)
			key := authkey.Generate()
			if err := b.SetAuthKey(context.Background(), key, "team:*"); err != nil {
				t.Fatal(err)
			}
			svc := newSecureTestServer(t, b, "")
			session := newReportSession(t, svc, key)
			info := protocol.ClientInfo{Name: "team:g:edge", Hostname: "original"}
			if reply, err := exchangeReport(t, session, info); err != nil || !reply.OK {
				t.Fatalf("initial report: %+v, %v", reply, err)
			}
			switch change {
			case "rename":
				info.Name = "team:g:other"
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
			binding, _ := b.LookupClient(context.Background(), "team:g:edge")
			var saved protocol.ClientInfo
			if err := json.Unmarshal([]byte(binding.Config), &saved); err != nil || saved.Hostname != "original" {
				t.Fatalf("rejected report overwrote metadata: %+v, %v", saved, err)
			}
		})
	}
}
