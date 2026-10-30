package core

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/client"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
)

func TestGlobalClientLiveConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := testStore(t)
	b := admin.NewBindings(st)
	if err := b.SetClient(admin.ClientBinding{ClientKey: "edge", ServerConfig: `{"custom_hosts":[{"domain":"local.route","ip":"10.0.0.1"}],"ip_maps":[{"from":"186.12.12.1","to":"10.0.0.1"}]}`}); err != nil {
		t.Fatal(err)
	}
	limits := ratelimit.NewRegistry()
	cfg := config.DefaultServer()
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.Auth.Key = ""
	svc := NewConnectorService(&cfg, transport.NewSessionManager(), b, limits)
	svc.SetSessionConfig(b)
	initial := config.DefaultServiceSettings()
	initial.Heartbeat.Enabled = false
	initial.GlobalClient = config.GlobalClientSettings{
		CustomHosts: []config.CustomHost{{Domain: "global.route", IP: "192.168.60.1"}, {Domain: "local.route", IP: "192.168.60.2"}},
		IPMaps:      []config.IPMap{{From: "186.12.12.*", To: "192.168.60.*"}},
	}
	sc := newServiceConfig(st, svc, traffic.NewMeter(limits), nil, b, initial)
	authenticated := make(chan struct{})
	register := make(chan struct{})
	var firstAuth sync.Once
	auth := svc.sessions.Auth
	svc.sessions.Auth = func(session tcp.Session) (tcp.SessionInfo, error) {
		info, err := auth(session)
		if err == nil {
			firstAuth.Do(func() {
				close(authenticated)
				select {
				case <-register:
				case <-ctx.Done():
				}
			})
		}
		return info, err
	}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer drainCancel()
		if err := svc.Drain(drainCtx); err != nil {
			t.Error(err)
		}
	}()
	connect := func() (*client.Client, <-chan protocol.PushConfig) {
		t.Helper()
		updates := make(chan protocol.PushConfig, 32)
		c, err := client.Dial(svc.Addr(), nil, client.DialOptions{Info: protocol.ClientInfo{Name: "edge"}, OnConfig: func(v protocol.PushConfig) { updates <- v }})
		if err != nil {
			t.Fatal(err)
		}
		return c, updates
	}
	waitConfig := func(ch <-chan protocol.PushConfig, want protocol.PushConfig) {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case got := <-ch:
				if reflect.DeepEqual(got, want) {
					return
				}
			case <-timer.C:
				t.Fatalf("did not receive config: %+v", want)
			}
		}
	}
	c, updates := connect()
	defer c.Close()
	waitConfig(updates, b.PushConfig("edge"))
	// 强制保存发生在认证应答与会话登记之间，验证补发可消除这个窗口。
	select {
	case <-authenticated:
	case <-time.After(5 * time.Second):
		t.Fatal("authentication did not finish")
	}
	next := sc.Current()
	next.GlobalClient.CustomHosts[0].IP = "192.168.60.9"
	if sc.Current().GlobalClient.CustomHosts[0].IP != "192.168.60.1" {
		t.Fatal("Current leaked mutable config")
	}
	if err := sc.Apply(next); err != nil {
		t.Fatal(err)
	}
	close(register)
	done := make(chan error, 1)
	go func() { done <- c.Serve() }()
	defer func() {
		_ = c.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("client Serve did not stop")
		}
	}()
	waitConfig(updates, b.PushConfig("edge"))
	next.GlobalClient.CustomHosts[0].IP = "192.168.60.10"
	if sc.Current().GlobalClient.CustomHosts[0].IP != "192.168.60.9" {
		t.Fatal("Apply retained caller slice")
	}
	if err := sc.Apply(next); err != nil {
		t.Fatal(err)
	}
	waitConfig(updates, b.PushConfig("edge"))
	loaded := loadServiceSettings(ctx, st, &cfg)
	if !reflect.DeepEqual(loaded.GlobalClient, next.GlobalClient) {
		t.Fatalf("persisted config: %+v", loaded.GlobalClient)
	}
	_ = c.Close()
	reconnected, reconnectUpdates := connect()
	defer reconnected.Close()
	waitConfig(reconnectUpdates, b.PushConfig("edge"))
	reconnectDone := make(chan error, 1)
	go func() { reconnectDone <- reconnected.Serve() }()
	defer func() {
		_ = reconnected.Close()
		select {
		case err := <-reconnectDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("reconnected Serve did not stop")
		}
	}()
	next.GlobalClient = config.GlobalClientSettings{}
	if err := sc.Apply(next); err != nil {
		t.Fatal(err)
	}
	waitConfig(reconnectUpdates, protocol.PushConfig{ClientConfig: b.ClientPushConfig("edge")})
	if loaded := loadServiceSettings(ctx, st, &cfg); len(loaded.GlobalClient.CustomHosts) != 0 || len(loaded.GlobalClient.IPMaps) != 0 {
		t.Fatalf("clear did not persist: %+v", loaded.GlobalClient)
	}
}

// TestClientResolvesPushedLayers 断言下发的两层配置真的决定客户端的解析结果，而不只是
// 原样到达：独立层优先、全局层兜底，全局热更新与清空独立层都立即反映到解析上。
// 这条链路是「推送 → 共享表 → 拨号前改写」，此前只有 dispatch 包的单测覆盖后半段。
func TestClientResolvesPushedLayers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := testStore(t)
	b := admin.NewBindings(st)
	if err := b.SetClient(admin.ClientBinding{ClientKey: "edge", ServerConfig: `{"custom_hosts":[{"domain":"vpn.route","ip":"10.0.0.9"}],"ip_maps":[{"from":"186.12.12.1","to":"10.0.0.1"}]}`}); err != nil {
		t.Fatal(err)
	}
	limits := ratelimit.NewRegistry()
	cfg := config.DefaultServer()
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.Auth.Key = ""
	svc := NewConnectorService(&cfg, transport.NewSessionManager(), b, limits)
	svc.SetSessionConfig(b)
	initial := config.DefaultServiceSettings()
	initial.Heartbeat.Enabled = false
	initial.GlobalClient = config.GlobalClientSettings{
		CustomHosts: []config.CustomHost{
			{Domain: "vpn.route", IP: "192.168.60.1"},
			{Domain: "global.route", IP: "192.168.60.2"},
		},
		IPMaps: []config.IPMap{{From: "186.12.12.*", To: "192.168.60.*"}},
	}
	sc := newServiceConfig(st, svc, traffic.NewMeter(limits), nil, b, initial)
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer drainCancel()
		if err := svc.Drain(drainCtx); err != nil {
			t.Error(err)
		}
	}()

	// 与生产装配一致（internal/cli/client_app.go）：两层交给共享表，拨号前查表改写。
	hosts, ipMaps := dispatch.NewHosts(), dispatch.NewIPMaps()
	c, err := client.Dial(svc.Addr(), nil, client.DialOptions{
		Info: protocol.ClientInfo{Name: "edge"},
		OnConfig: func(v protocol.PushConfig) {
			hosts.UpdateLayers(v.ClientConfig.CustomHosts, v.GlobalConfig.CustomHosts)
			ipMaps.UpdateLayers(v.ClientConfig.IPMaps, v.GlobalConfig.IPMaps)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	done := make(chan error, 1)
	go func() { done <- c.Serve() }()
	defer func() {
		_ = c.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("client Serve did not stop")
		}
	}()

	waitHost := func(domain, want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			ip, ok := hosts.LookupHost(domain)
			if ok && ip == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s resolved to %q (matched %v), want %q", domain, ip, ok, want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitMap := func(target, want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			ip, ok := ipMaps.Rewrite(target)
			if ok && ip == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s rewritten to %q (matched %v), want %q", target, ip, ok, want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// 认证应答即带快照：独立层压过同名全局项，独立层缺项回落全局；段映射同理。
	waitHost("vpn.route", "10.0.0.9")
	waitHost("global.route", "192.168.60.2")
	waitMap("186.12.12.1", "10.0.0.1")
	waitMap("186.12.12.99", "192.168.60.99")

	// 全局热更新：在线推送后新值立刻参与解析，独立层不受影响。
	next := sc.Current()
	next.GlobalClient.CustomHosts[1].IP = "192.168.60.7"
	next.GlobalClient.IPMaps[0].To = "192.168.61.*"
	if err := sc.Apply(next); err != nil {
		t.Fatal(err)
	}
	waitHost("global.route", "192.168.60.7")
	waitMap("186.12.12.99", "192.168.61.99")
	if ip, _ := hosts.LookupHost("vpn.route"); ip != "10.0.0.9" {
		t.Fatalf("global update leaked into the independent layer: %s", ip)
	}

	// 清空独立层后同名规则回落到全局层，全局规则保持热更新后的值。
	if err := b.SetClient(admin.ClientBinding{ClientKey: "edge"}); err != nil {
		t.Fatal(err)
	}
	pushed := false
	for _, s := range svc.sessions.ClientSessions() {
		if s.Info().Name != "edge" {
			continue
		}
		if err := b.PushClientConfig(s); err != nil {
			t.Fatal(err)
		}
		pushed = true
		break
	}
	if !pushed {
		t.Fatal("client session is not registered")
	}
	waitHost("vpn.route", "192.168.60.1")
	waitMap("186.12.12.1", "192.168.61.1")
}
