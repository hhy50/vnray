package admin

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"

	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/transport"
)

func TestClientIdentityListAndDetail(t *testing.T) {
	s := newTestAdminService(t)
	st := s.opts.Store.(store.Store)
	bindings := NewBindings(st)
	s.opts.Bindings = bindings
	groups := []string{"office", "lab", "home"}
	for i, group := range groups {
		info := protocol.ClientInfo{Group: group, Name: "edge", Hostname: group + "-host"}
		key := info.ClientKey()
		reported, err := json.Marshal(info)
		if err != nil {
			t.Fatal(err)
		}
		if err := bindings.SetClient(context.Background(), ClientBinding{ClientKey: key, Config: string(reported)}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddClientTraffic(context.Background(), key, int64(i+1), int64(i+11)); err != nil {
			t.Fatal(err)
		}
		if group != "home" {
			newTestIdentitySession(t, s, info)
		}
	}
	w, r := apiContext(http.MethodGet, "/", nil, nil)
	s.apiClients(w, r)
	var list struct {
		Data struct {
			Clients []apiClientDTO `json:"clients"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != http.StatusOK || len(list.Data.Clients) != len(groups) {
		t.Fatalf("client list: %d %s, %v", w.Code, w.Body.String(), err)
	}
	rows := make(map[string]apiClientDTO)
	for _, row := range list.Data.Clients {
		rows[row.Key] = row
	}
	for i, group := range groups {
		key := group + ":edge"
		row, ok := rows[key]
		if !ok || row.Group != group || row.Name != "edge" || row.Online != (group != "home") || row.UpBytes != int64(i+1) || row.DownBytes != int64(i+11) {
			t.Fatalf("wrong row for %s: %+v", key, row)
		}
		w, r := apiContext(http.MethodGet, "/", nil, map[string]string{"name": key})
		s.apiClientDetail(w, r)
		var detail struct {
			Data apiClientDetailDTO `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != http.StatusOK {
			t.Fatalf("client detail: %s, %v", w.Body.String(), err)
		}
		got := detail.Data
		if got.Key != key || got.Group != group || got.Name != "edge" || got.Online != row.Online || !got.HasStored {
			t.Fatalf("wrong detail for %s: %+v", key, got)
		}
		var reported protocol.ClientInfo
		if err := json.Unmarshal([]byte(got.ReportedJSON), &reported); err != nil || reported.Group != group || reported.Name != "edge" {
			t.Fatalf("wrong reported identity for %s: %+v, %v", key, reported, err)
		}
		w, r = apiContext(http.MethodGet, "/?q="+group, nil, nil)
		s.apiClients(w, r)
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Data.Clients) != 1 || list.Data.Clients[0].Key != key {
			t.Fatalf("group search for %s: %s, %v", group, w.Body.String(), err)
		}
	}
	options := s.clientOptions(r)
	if len(options) != len(groups) {
		t.Fatalf("client options: %+v", options)
	}
	for _, option := range options {
		if _, ok := rows[option.Name]; !ok {
			t.Fatalf("option lost its group: %+v", option)
		}
	}
}

func newTestIdentitySession(t *testing.T, s *AdminService, info protocol.ClientInfo) {
	t.Helper()
	serverConn, peerConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = peerConn.Close()
	})
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
	client := transport.NewSession(serverSession, "pipe", info)
	if err := s.clients.OnSessionOpen(&tcp.SessionInfo{Session: serverSession, Kind: tcp.KindClient, Name: info.ClientKey(), Meta: client}); err != nil {
		t.Fatal(err)
	}
}
