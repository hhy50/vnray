package core

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
)

// SessionConfig 是握手期要用的运维配置来源（由 admin.Bindings 实现）：
// 用户与客户端的日志级别在这里取，客户端上报的配置快照与接入密钥的范围校验也经它；
// 为 nil 时用默认日志级别、不持久化上报快照、按配置文件密钥校验。
type SessionConfig interface {
	UserConfig(p store.Protocol, key string) (store.UserConfig, bool)
	ClientLogLevel(clientKey string) string
	ClientConcurrency(clientKey string) int
	SaveClientReportedConfig(clientKey, reported string) error
	// PushConfig 返回服务端下发给该客户端的全量配置快照（随认证应答携带）。
	PushConfig(clientKey string) protocol.PushConfig
	// MatchClientAuthKey 按范围密钥表判定客户端认证；configured 为 false 表示
	// 表为空（未配置任何密钥），调用方回退到配置文件的共享密钥。
	MatchClientAuthKey(clientName, key string) (configured, ok bool)
}

// ConnectorService 是数据面服务：客户端与用户共用主端口，按握手首帧区分角色。
// 它把认证与流处理委托给会话管理器（认证成功后由 SessionManager 登记），并驱动心跳、
// 把用户的转发流桥接到其绑定的客户端。会话登记表由 SessionManager 持有，本服务不另存副本。
type ConnectorService struct {
	server   *tcp.Server
	sessions *transport.SessionManager
	bindings transport.UserRegistry
	limits   *ratelimit.Registry
	authKey  string
	hub      *forwardHub
	// heartbeat 是当前心跳策略，运行期可经 ApplyHeartbeat 热改；heartbeatReload 唤醒调度协程。
	heartbeat       atomic.Pointer[HeartbeatOptions]
	heartbeatReload chan struct{}
	// sessionCfg 与 traffic 由装配层注入；为 nil 时用默认日志级别、不落库流量。
	sessionCfg SessionConfig
	traffic    *traffic.Meter
	// rules 是用户出站规则表；nil 时不做规则判定。
	rules *ruleSet
}

// SetSessionConfig 注入用户/客户端的运维配置来源（日志级别）。
func (s *ConnectorService) SetSessionConfig(src SessionConfig) { s.sessionCfg = src }

// SetRules 注入用户出站规则表；须在 Start 前调用，nil 表示不做规则判定。
func (s *ConnectorService) SetRules(rules *ruleSet) {
	s.rules = rules
	if s.hub != nil {
		s.hub.rules = rules
	}
}

// ApplyHeartbeat 热更新心跳策略；interval 或 timeout 非正数表示关闭心跳。
func (s *ConnectorService) ApplyHeartbeat(interval, timeout time.Duration) {
	s.heartbeat.Store(&HeartbeatOptions{Interval: interval, Timeout: timeout})
	select {
	case s.heartbeatReload <- struct{}{}:
	default:
	}
}

// ApplySessionConfig 热更新多路复用会话参数：仅对之后建立的新连接生效。
func (s *ConnectorService) ApplySessionConfig(cfg *tcp.SessionConfig) {
	s.server.SetSessionConfig(cfg)
}

// SetExitResolver 覆盖出口解析器（含本机出口时由装配层注入）；须在 Start 前调用。
func (s *ConnectorService) SetExitResolver(r transport.ExitResolver) {
	if s.hub != nil {
		s.hub.exits = r
	}
}

// SetTraffic 注入流量记账器；hub 已建好时一并转交。
func (s *ConnectorService) SetTraffic(m *traffic.Meter) {
	s.traffic = m
	if s.hub != nil {
		s.hub.traffic = m
	}
}

// NewConnectorService 创建数据面服务；sessions 为会话管理器（登记与查询入口），
// bindings 为 nil 时拒绝用户接入，limits 为 nil 时不启用限速。
func NewConnectorService(cfg *config.Server, sessions *transport.SessionManager, bindings transport.UserRegistry, limits *ratelimit.Registry) *ConnectorService {
	return NewConnectorServiceWith(cfg, sessions, bindings, limits)
}

// NewConnectorServiceWith 同 NewConnectorService，并额外接受接入层选项（供测试注入多路复用参数）。
// 心跳初始为内置默认策略（启用、5s/3s），由装配层按运行时服务配置经 ApplyHeartbeat 覆盖。
func NewConnectorServiceWith(cfg *config.Server, sessions *transport.SessionManager, bindings transport.UserRegistry, limits *ratelimit.Registry, opts ...tcp.Option) *ConnectorService {
	s := &ConnectorService{
		sessions:        sessions,
		bindings:        bindings,
		limits:          limits,
		authKey:         cfg.Server.Auth.Key,
		heartbeatReload: make(chan struct{}, 1),
	}
	hb := config.DefaultHeartbeat()
	s.heartbeat.Store(&HeartbeatOptions{Interval: hb.Interval.Std(), Timeout: hb.Timeout.Std()})
	// exits 默认为会话管理器（仅远程会话）；装配层可经 SetExitResolver 附带本机出口。
	if bindings != nil {
		s.hub = &forwardHub{exits: sessions, limits: limits}
	}
	// 认证与流处理是业务决策，注入给管理器；登记/注销/索引由管理器负责。
	sessions.Auth = s.Handshake
	sessions.Streams = s.onStreamOpen
	s.server = tcp.NewServer(cfg.Server.Listen, sessions, opts...)
	return s
}

// Name 返回服务名，用于启动日志与诊断。
func (s *ConnectorService) Name() string { return "ConnectorService" }

// Addr 返回实际监听地址；启动前返回构造时传入的地址（传 ":0" 时 Start 后取真实端口）。
func (s *ConnectorService) Addr() string { return s.server.Addr() }

// Sessions 返回会话管理器：会话登记表与查询的唯一入口。
func (s *ConnectorService) Sessions() *transport.SessionManager { return s.sessions }

// Start 启动接入，并以组件 ctx 驱动心跳调度。
func (s *ConnectorService) Start(ctx context.Context) error {
	if err := s.server.Start(ctx); err != nil {
		return err
	}
	routine.Go("core.connector.heartbeat", func() { s.runHeartbeatScheduler(ctx) })
	return nil
}

// Drain 实现 extension.Drainer：关监听，再由管理器关掉全部已授权会话并停止登记。
func (s *ConnectorService) Drain(ctx context.Context) error {
	err := s.server.Stop(ctx)
	s.sessions.Close()
	return err
}

// clientOf 取出会话携带的客户端视图；只有客户端会话有。
func clientOf(info *tcp.SessionInfo) *transport.Session {
	client, _ := info.Meta.(*transport.Session)
	return client
}

// onStreamOpen 是注入给会话管理器的流处理回调：用户会话的流交给转发桥（阻塞该流
// 整个生命周期）；客户端会话不应有入站流，记告警后让事件循环关掉它。
func (s *ConnectorService) onStreamOpen(info *tcp.SessionInfo, stream tcp.Stream) error {
	if info.Kind != tcp.KindUser {
		logging.Warnf("unexpected inbound stream, remote=%s", stream.RemoteAddr().String())
		return nil
	}
	if s.hub == nil {
		logging.Warnf("user stream without forward hub, remote=%s", stream.RemoteAddr().String())
		return nil
	}
	// 用户会话的 Meta 是握手时记下的 UserMeta：限速与审计按 user_key 标识用户，
	// 出站规则按其中记下的规则名判定。
	meta, _ := info.Meta.(transport.UserMeta)
	s.hub.handleUserStream(info.Log, meta.Key, meta.Rule, info.Name, stream, info.Session.CloseChan())
	return nil
}
