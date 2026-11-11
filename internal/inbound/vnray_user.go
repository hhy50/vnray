package inbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/transport"
)

const pluginVNRayUser = "vnray-user"

// VNRayUserPlugin 提供独立的 vnray 用户入站。
type VNRayUserPlugin struct {
	runtime  plugin.Runtime
	servers  sync.Map
	store    store.Store
	listener tcp.EventListener
	users    transport.UserRegistry
	userCfg  UserConfigProvider
	clients  ClientSessionLookup
	cfg      *tcp.SessionConfig
	server   *tcp.Server
	port     int
}

// VNRayUserDependencies 声明独立用户入站的运行依赖。
type VNRayUserDependencies struct {
	Events        tcp.EventListener
	Users         transport.UserRegistry
	UserConfig    UserConfigProvider
	Clients       ClientSessionLookup
	SessionConfig *tcp.SessionConfig
}

// NewVNRayUserPlugin 创建由装配层注入依赖的用户入站。
func NewVNRayUserPlugin(st store.Store, deps VNRayUserDependencies) *VNRayUserPlugin {
	p := &VNRayUserPlugin{store: st, users: deps.Users, userCfg: deps.UserConfig, clients: deps.Clients, cfg: deps.SessionConfig, runtime: plugin.NewRuntime()}
	if deps.Events != nil {
		p.listener = newVNRayUserListener(deps.Events, p)
	}
	return p
}

// UserConfigProvider supplies per-user settings used while authenticating a user session.
type UserConfigProvider interface {
	UserConfig(context.Context, store.Protocol, string) (store.UserConfig, bool)
}

// ClientSessionLookup provides the live client details returned in a successful user auth reply.
type ClientSessionLookup interface {
	SessionByName(string) (*tcp.SessionInfo, bool)
}

type vnrayUserListener struct {
	tcp.EventListener
	plugin *VNRayUserPlugin
}

func (l *vnrayUserListener) Handshake(session tcp.Session) (tcp.SessionInfo, error) {
	if err := session.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return tcp.SessionInfo{}, err
	}
	defer func() { _ = session.SetDeadline(time.Time{}) }()
	stream, err := session.AcceptStream()
	if err != nil {
		return tcp.SessionInfo{}, err
	}
	defer func() { _ = stream.Close() }()
	if err := stream.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return tcp.SessionInfo{}, err
	}
	frame, err := protocol.NewDecoder(stream).ReadFrame()
	if err != nil {
		return tcp.SessionInfo{}, err
	}
	defer func() { _ = frame.CloseBody() }()
	if frame.Type != protocol.UserAuth {
		return tcp.SessionInfo{}, fmt.Errorf("%w: expected frame %d, got %d", protocol.ErrProtocol, protocol.UserAuth, frame.Type)
	}
	payload, err := protocol.ReadPayload(frame)
	if err != nil {
		return tcp.SessionInfo{}, err
	}
	req, err := protocol.ParseUserAuthRequest(payload)
	if err != nil {
		return tcp.SessionInfo{}, err
	}
	p := l.plugin
	if p.users == nil {
		return rejectUserAuth(stream, "user access is not enabled")
	}
	access, ok := p.users.UserFor(p.runtime.Context(), req.Key)
	if !ok {
		return rejectUserAuth(stream, "user key is not registered")
	}
	if !access.Active {
		return rejectUserAuth(stream, "user is disabled")
	}
	reply := protocol.Reply{OK: true, Client: &protocol.ClientInfo{Name: access.Client}}
	if group, name, found := strings.Cut(access.Client, ":"); found {
		reply.Client.Group, reply.Client.Name = group, name
	}
	if p.clients != nil {
		if info, exists := p.clients.SessionByName(access.Client); exists {
			if client, ok := info.Meta.(*transport.Session); ok {
				reported := client.Info()
				reply.Client = &reported
			}
		}
	}
	protocol.WriteReply(stream, protocol.AuthResult, reply)
	var level, userRule string
	if p.userCfg != nil {
		if cfg, ok := p.userCfg.UserConfig(p.runtime.Context(), store.ProtocolVNV1, req.Key); ok {
			level, userRule = cfg.LogLevel, cfg.Rule
		}
	}
	return tcp.SessionInfo{Session: session, Remote: session.RemoteAddr().String(), Kind: tcp.KindUser,
		Name: access.Client, Log: logging.NewScoped(level, "user", req.Key),
		Meta: transport.UserMeta{Key: req.Key, Name: access.Name, Rule: userRule, Info: req.Info}}, nil
}

// NewVNRayUserListener creates the protocol listener for the vnray-user inbound.
func NewVNRayUserListener(events tcp.EventListener, users transport.UserRegistry, userCfg UserConfigProvider, clients ClientSessionLookup) tcp.EventListener {
	return newVNRayUserListener(events, &VNRayUserPlugin{users: users, userCfg: userCfg, clients: clients, runtime: plugin.NewRuntime()})
}

func newVNRayUserListener(events tcp.EventListener, p *VNRayUserPlugin) tcp.EventListener {
	return &vnrayUserListener{EventListener: events, plugin: p}
}

func rejectUserAuth(stream tcp.Stream, reason string) (tcp.SessionInfo, error) {
	protocol.WriteReply(stream, protocol.AuthResult, protocol.Reply{Reason: reason})
	return tcp.SessionInfo{}, fmt.Errorf("plugin: %s", reason)
}

// Name 返回插件名。
func (p *VNRayUserPlugin) Name() string { return pluginVNRayUser }

// BindDrainGate 在注册时接入管理器的收尾状态门。
func (p *VNRayUserPlugin) BindDrainGate(g *plugin.DrainGate) { p.runtime.BindDrainGate(g) }

// Status 返回用户入站的运行状态。
func (p *VNRayUserPlugin) Status() plugin.Status {
	return plugin.Status{Name: pluginVNRayUser, Enabled: p.server != nil, AcceptsUsers: true, LastError: p.runtime.LastError(),
		Ports: []plugin.PortKV{{Protocol: store.ProtocolVNV1, Port: p.port}}}
}

// Restore 恢复监听端口与启用状态，失败由宿主处理。
func (p *VNRayUserPlugin) Restore(ctx context.Context) error {
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	state, ok, err := p.store.GetPlugin(ctx, pluginVNRayUser)
	if err != nil || !ok {
		return p.runtime.Record(err)
	}
	p.port = state.Config.Ports[store.ProtocolVNV1]
	if state.Status == store.PluginEnabled && p.server == nil {
		server, err := p.start(p.port)
		if err != nil {
			return p.runtime.Record(err)
		}
		p.server = server
	}
	return p.runtime.Record(nil)
}

func (p *VNRayUserPlugin) save(ctx context.Context, enabled bool, port int) error {
	status := store.PluginDisabled
	if enabled {
		status = store.PluginEnabled
	}
	return plugin.SaveState(ctx, p.store, pluginVNRayUser, status, func(c *store.PluginConfig) {
		c.Ports[store.ProtocolVNV1] = port
	})
}

// Enable 启用独立用户端口。
func (p *VNRayUserPlugin) Enable(ctx context.Context, params map[string]string) error {
	port, err := strconv.Atoi(params["port"])
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("plugin: invalid vnray-user port %q", params["port"])
	}
	return p.runtime.Record(p.configure(ctx, port, true))
}

// SetPort 修改待启用端口或热切换运行中的监听。
func (p *VNRayUserPlugin) SetPort(ctx context.Context, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("plugin: invalid vnray-user port %d", port)
	}
	return p.runtime.Record(p.configure(ctx, port, p.server != nil))
}

func (p *VNRayUserPlugin) configure(ctx context.Context, port int, enabled bool) error {
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	var next *tcp.Server
	var ln net.Listener
	if enabled && (p.server == nil || port != p.port) {
		var err error
		next, ln, err = p.prepare(port)
		if err != nil {
			return err
		}
	}
	if err := p.save(ctx, enabled, port); err != nil {
		if ln != nil {
			_ = ln.Close()
		}
		return err
	}
	if next != nil {
		if err := next.StartListener(p.runtime.Context(), ln); err != nil {
			return err
		}
		if p.server != nil {
			_ = p.server.Stop(p.runtime.Context())
		}
		p.server = next
		p.trackServer(next)
	}
	p.port = port
	return nil
}

func (p *VNRayUserPlugin) prepare(port int) (*tcp.Server, net.Listener, error) {
	if p.listener == nil {
		return nil, nil, fmt.Errorf("plugin: vnray-user listener is not configured")
	}
	if port <= 0 || port > 65535 {
		return nil, nil, fmt.Errorf("plugin: invalid vnray-user port %d", port)
	}
	addr := fmt.Sprintf(":%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("plugin: vnray-user listen %s: %w", addr, err)
	}
	server := tcp.NewServer(addr, p.listener, tcp.WithSessionConfig(p.cfg))
	return server, ln, nil
}

func (p *VNRayUserPlugin) start(port int) (*tcp.Server, error) {
	server, ln, err := p.prepare(port)
	if err != nil {
		return nil, err
	}
	if err := server.StartListener(p.runtime.Context(), ln); err != nil {
		return nil, err
	}
	p.trackServer(server)
	return server, nil
}

func (p *VNRayUserPlugin) trackServer(server *tcp.Server) {
	done := make(chan struct{})
	p.servers.Store(server, done)
	routine.Go("plugin.vnray-user.reap", func() {
		defer close(done)
		<-server.Done()
		p.servers.Delete(server)
	})
}

// Disable 停止监听并持久化停用状态。
func (p *VNRayUserPlugin) Disable(ctx context.Context) error {
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	if err := p.save(ctx, false, p.port); err != nil {
		return err
	}
	p.stop()
	return nil
}

// Stop 停止监听，保持持久化状态不变。
func (p *VNRayUserPlugin) Stop() {
	p.stop()
}

func (p *VNRayUserPlugin) stop() {
	if p.server != nil {
		_ = p.server.Stop(p.runtime.Context())
		p.server = nil
	}
}

// ApplySessionConfig 更新新会话的配置。
func (p *VNRayUserPlugin) ApplySessionConfig(cfg *tcp.SessionConfig) {
	p.cfg = cfg
	if p.server != nil {
		p.server.SetSessionConfig(cfg)
	}
}

// Drain 关闭当前及历史监听代次的全部会话并等待退出。
func (p *VNRayUserPlugin) Drain(ctx context.Context) error {
	p.runtime.BeginDrain()
	servers := make(map[*tcp.Server]chan struct{})
	p.servers.Range(func(key, value any) bool {
		servers[key.(*tcp.Server)] = value.(chan struct{})
		return true
	})
	p.stop()
	var errs []error
	for server, done := range servers {
		if err := server.Drain(ctx); err != nil {
			errs = append(errs, err)
		}
		select {
		case <-done:
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
		}
	}
	return errors.Join(errs...)
}
