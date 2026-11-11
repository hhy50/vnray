package client

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/secure"
	"github.com/hhy50/vnray/internal/tcp"
)

type reportListener struct {
	handle func(tcp.Stream) error
}

func (l reportListener) Handshake(s tcp.Session) (tcp.SessionInfo, error) {
	stream, err := s.AcceptStream()
	if err != nil {
		return tcp.SessionInfo{}, err
	}
	defer stream.Close()
	if err := l.handle(stream); err != nil {
		return tcp.SessionInfo{}, err
	}
	return tcp.SessionInfo{Session: s}, nil
}

func (l reportListener) OnSessionOpen(*tcp.SessionInfo) error       { return nil }
func (l reportListener) OnSessionClose(*tcp.SessionInfo)            {}
func (l reportListener) OnStreamClose(*tcp.SessionInfo, tcp.Stream) {}
func (l reportListener) OnStreamOpen(_ *tcp.SessionInfo, stream tcp.Stream) error {
	return l.handle(stream)
}

func newReportClient(t *testing.T, handle func(tcp.Stream) error, opts DialOptions) *Client {
	t.Helper()
	opts.Auth.Key = authkey.Generate()
	key, _ := authkey.Parse(opts.Auth.Key)
	server := tcp.NewServer("127.0.0.1:0", reportListener{handle: handle}, tcp.WithConnectionHandshake(
		func(ctx context.Context, raw net.Conn) (net.Conn, error) {
			return secure.Server(ctx, raw, func(context.Context, authkey.ID) (authkey.Key, error) { return key, nil }, true)
		}))
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop(context.Background()) })
	c, err := Dial(server.Addr(), nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func readTestReport(stream tcp.Stream) (protocol.ClientInfo, error) {
	_ = stream.SetDeadline(time.Now().Add(2 * time.Second))
	frame, err := protocol.NewDecoder(stream).ReadFrame()
	if err != nil {
		return protocol.ClientInfo{}, err
	}
	if frame.Type != protocol.ConfigReport {
		return protocol.ClientInfo{}, fmt.Errorf("unexpected frame %d", frame.Type)
	}
	payload, err := protocol.ReadPayload(frame)
	if err != nil {
		return protocol.ClientInfo{}, err
	}
	report, err := protocol.ParseConfigReportRequest(payload)
	return report.Info, err
}

func TestPeriodicConfigReport(t *testing.T) {
	reports := make(chan protocol.ClientInfo, 16)
	updates := make(chan uint64, 16)
	var count atomic.Uint64
	var hostname atomic.Value
	hostname.Store("initial")
	c := newReportClient(t, func(stream tcp.Stream) error {
		info, err := readTestReport(stream)
		if err != nil {
			return err
		}
		reports <- info
		cfg := protocol.PushConfig{Revision: count.Add(1)}
		frame, err := protocol.NewReplyFrame(protocol.ConfigReportResult, protocol.Reply{OK: true, Config: &cfg})
		if err != nil {
			return err
		}
		return protocol.NewEncoder(stream).WriteFrame(frame)
	}, DialOptions{
		ReportInfo: func() protocol.ClientInfo {
			return protocol.ClientInfo{Group: "office", Name: "edge", Hostname: hostname.Load().(string)}
		},
		OnConfig: func(cfg protocol.PushConfig) { updates <- cfg.Revision },
	})
	if info := <-reports; info.Hostname != "initial" || info.Group != "office" || info.Name != "edge" {
		t.Fatalf("initial report: %+v", info)
	}
	if revision := <-updates; revision != 1 {
		t.Fatalf("initial server config = %d", revision)
	}
	hostname.Store("refreshed")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); c.runConfigReports(ctx, 10*time.Millisecond) }()
	for i := uint64(2); i <= 3; i++ {
		select {
		case info := <-reports:
			if info.Hostname != "refreshed" || info.Group != "office" || info.Name != "edge" {
				t.Fatalf("periodic report: %+v", info)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("periodic report not received")
		}
		select {
		case revision := <-updates:
			if revision != i {
				t.Fatalf("server config revision = %d, want %d", revision, i)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("periodic response not applied")
		}
	}
	_ = c.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("report loop did not stop on close")
	}
}

func TestPeriodicReportFailureAndClose(t *testing.T) {
	for _, mode := range []string{"rejected", "in-flight close"} {
		t.Run(mode, func(t *testing.T) {
			var count atomic.Int32
			inFlight := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			c := newReportClient(t, func(stream tcp.Stream) error {
				if _, err := readTestReport(stream); err != nil {
					return err
				}
				if count.Add(1) == 1 {
					protocol.WriteReply(stream, protocol.ConfigReportResult, protocol.Reply{OK: true})
					return nil
				}
				close(inFlight)
				if mode == "rejected" {
					protocol.WriteReply(stream, protocol.ConfigReportResult, protocol.Reply{Reason: "revoked"})
				} else {
					<-release
				}
				return nil
			}, DialOptions{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); c.runConfigReports(ctx, 10*time.Millisecond) }()
			select {
			case <-inFlight:
			case <-time.After(2 * time.Second):
				t.Fatal("periodic request not started")
			}
			if mode == "in-flight close" {
				_ = c.Close()
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("report loop did not stop")
			}
			if !c.session.IsClosed() {
				t.Fatal("report failure did not close session")
			}
		})
	}
}
