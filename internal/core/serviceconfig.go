package core

import (
	"context"
	"reflect"
	"slices"
	"sync/atomic"
	"time"

	"github.com/hhy50/vnray/internal/access"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/metrics"
	"github.com/hhy50/vnray/internal/monitor"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
)

// applyTimeout 是运行时配置落库的等待上限。
const applyTimeout = 5 * time.Second

// defaultServiceSettings 合成启动基线：日志级别取配置文件（管理台可覆盖），
// 心跳与分享主机不在配置文件里、取内置默认值（心跳启用 5s/3s，分享主机留空）。
func defaultServiceSettings(cfg *config.Server) config.ServiceSettings {
	s := config.DefaultServiceSettings()
	if cfg.Log != nil && cfg.Log.Level != "" {
		s.LogLevel = cfg.Log.Level
	}
	return s
}

// loadServiceSettings 只读取已初始化的服务配置；解析失败时回退基线。
func loadServiceSettings(ctx context.Context, st store.Store, cfg *config.Server) config.ServiceSettings {
	base := defaultServiceSettings(cfg)
	kv, err := st.GetServiceSettings(ctx)
	if err != nil {
		logging.Warnf("service settings: load failed, using defaults: %v", err)
		return base
	}
	if len(kv) == 0 {
		return base
	}
	out, err := config.DecodeServiceSettings(kv, base)
	if err != nil {
		logging.Warnf("service settings: decode failed, using defaults: %v", err)
		return base
	}
	if err := out.Validate(); err != nil {
		logging.Warnf("service settings: stored value invalid, using defaults: %v", err)
		return base
	}
	return out
}

// heartbeatIntervalOf 返回生效的心跳间隔：未启用时返回 0（整体关闭）。
func heartbeatIntervalOf(h config.Heartbeat) time.Duration {
	if !h.Enabled {
		return 0
	}
	return h.Interval.Std()
}

// sessionConfigFromSettings 把配置值对象翻成多路复用后端参数：0 一律表示"用后端默认"，
// 因此从 DefaultSessionConfig 起手再逐项覆盖（0 会关掉 keepalive，不能直接透传）。
func sessionConfigFromSettings(s config.SessionSettings) *tcp.SessionConfig {
	cfg := tcp.DefaultSessionConfig()
	if s.KeepAliveInterval > 0 {
		cfg.KeepAliveInterval = s.KeepAliveInterval.Std()
	}
	if s.KeepAliveTimeout > 0 {
		cfg.KeepAliveTimeout = s.KeepAliveTimeout.Std()
	}
	if s.MaxStreamWindow > 0 {
		cfg.MaxStreamWindow = uint32(s.MaxStreamWindow)
	}
	if s.AcceptBacklog > 0 {
		cfg.AcceptBacklog = s.AcceptBacklog
	}
	if s.StreamOpenTimeout > 0 {
		cfg.StreamOpenTimeout = s.StreamOpenTimeout.Std()
	}
	return cfg
}

// serviceConfig 是运行时服务配置的持有者与分发中心，实现 admin.ServiceSettings。
// 它是管理台与运行时组件之间的唯一写口：管理台提交 → 校验 → 落库 → 热分发。
type serviceConfig struct {
	monitor  *monitor.Service
	cur      atomic.Pointer[config.ServiceSettings]
	settings store.Store
	svc      *ConnectorService
	meter    *traffic.Meter
	sessions sessionConfigApplier
	// resolver 与 bindings 用于热改本机出口：原子换 local 出口、增删 client_bindings 行。
	resolver *exitResolver
	bindings *access.Bindings
}

type sessionConfigApplier interface {
	ApplySessionConfig(*tcp.SessionConfig)
}

// newServiceConfig 构造分发中心；initial 是启动解析出的生效值。
func newServiceConfig(st store.Store, svc *ConnectorService, meter *traffic.Meter, resolver *exitResolver, bindings *access.Bindings, initial config.ServiceSettings) *serviceConfig {
	svc.ApplyTransportEncryption(initial.TransportEncryption)
	metrics.SetEnabled(initial.MetricsEnabled)
	initial = cloneServiceSettings(initial)
	if bindings != nil {
		bindings.SetGlobalClientConfig(initial.GlobalClient)
		svc.sessions.ClientReady = func(client *transport.Session) error {
			return bindings.PushClientConfig(svc.context(), client)
		}
	}
	c := &serviceConfig{settings: st, svc: svc, meter: meter, resolver: resolver, bindings: bindings}
	c.cur.Store(&initial)
	return c
}

func (c *serviceConfig) setSessionConfigApplier(applier sessionConfigApplier) {
	c.sessions = applier
}

// Current 返回当前生效的运行时配置快照。
func (c *serviceConfig) Current() config.ServiceSettings {
	return cloneServiceSettings(*c.cur.Load())
}

func cloneServiceSettings(current config.ServiceSettings) config.ServiceSettings {
	current.GlobalClient = current.GlobalClient.Clone()
	if current.VLESS.TLS != nil {
		tls := *current.VLESS.TLS
		current.VLESS.TLS = &tls
	}
	routing := &current.ServerProxy.Routing
	routing.Outbounds = slices.Clone(routing.Outbounds)
	routing.Rules = slices.Clone(routing.Rules)
	for i := range routing.Rules {
		match := &routing.Rules[i].Match
		match.Host = slices.Clone(match.Host)
		match.DomainPrefix = slices.Clone(match.DomainPrefix)
		match.DomainSuffix = slices.Clone(match.DomainSuffix)
		match.DomainKeyword = slices.Clone(match.DomainKeyword)
		match.Port = slices.Clone(match.Port)
	}
	return current
}

// VLESSTLS 返回 vless 的 TLS 参数（副本）；从未配置过时返回 nil。
func (c *serviceConfig) VLESSTLS() *config.VLESSTLS {
	current := c.cur.Load()
	if current.VLESS.TLS == nil {
		return nil
	}
	cp := *current.VLESS.TLS
	return &cp
}

// SetVLESSTLS 保存 VLESS TLS 配置。
func (c *serviceConfig) SetVLESSTLS(ctx context.Context, t config.VLESSTLS) error {
	raw, err := config.EncodeVLESSSettings(config.VLESSSettings{TLS: &t})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	if err := c.settings.SetServiceSettings(ctx, map[string]string{config.SettingVLESS: raw}); err != nil {
		return err
	}
	for {
		current := c.cur.Load()
		next := cloneServiceSettings(*current)
		next.VLESS.TLS = &t
		if c.cur.CompareAndSwap(current, &next) {
			break
		}
	}
	return nil
}

// Apply 全量提交运行时配置，VLESS 证书由专用写口维护。
func (c *serviceConfig) Apply(ctx context.Context, next config.ServiceSettings) error {
	return c.Update(ctx, func(current *config.ServiceSettings) error {
		next.VLESS = current.VLESS
		*current = next
		return nil
	})
}

// Update 保存配置变更并下发到运行态。
func (c *serviceConfig) Update(ctx context.Context, update func(*config.ServiceSettings) error) error {
	next := c.Current()
	before := next.GlobalClient.Clone()
	if err := update(&next); err != nil {
		return err
	}
	if err := c.apply(ctx, next); err != nil {
		return err
	}
	if !reflect.DeepEqual(before, next.GlobalClient) {
		c.pushClientConfigs(ctx)
	}
	return nil
}

func (c *serviceConfig) apply(ctx context.Context, next config.ServiceSettings) error {
	next = cloneServiceSettings(next)
	if err := next.Validate(); err != nil {
		return err
	}
	sess := sessionConfigFromSettings(next.Session)
	if err := tcp.ValidateSessionConfig(sess); err != nil {
		return err
	}
	// 先构建本机出口：构建即校验 ACL/routing，失败则不落库（避免存下无法生效的值）。
	var local *localExit
	if next.ServerProxy.Enabled {
		l, err := buildLocalExit(next.ServerProxy)
		if err != nil {
			return err
		}
		local = l
	}
	kv, err := config.EncodeServiceSettings(next)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	if err := c.settings.SetServiceSettings(ctx, kv); err != nil {
		return err
	}
	c.dispatch(ctx, next, sess, local)
	return nil
}

// dispatch 把生效值推给各运行时组件并更新本地快照。日志级别非法不可能发生（已被校验）。
func (c *serviceConfig) dispatch(ctx context.Context, next config.ServiceSettings, sess *tcp.SessionConfig, local *localExit) {
	c.svc.ApplyTransportEncryption(next.TransportEncryption)
	if c.monitor != nil {
		c.monitor.Configure(monitoringSettings(next))
	} else {
		metrics.SetEnabled(next.MetricsEnabled)
	}
	globalChanged := !reflect.DeepEqual(c.Current().GlobalClient, next.GlobalClient)
	c.svc.ApplyHeartbeat(heartbeatIntervalOf(next.Heartbeat), next.Heartbeat.Timeout.Std())
	c.svc.ApplySessionConfig(sess)
	if c.sessions != nil {
		c.sessions.ApplySessionConfig(sess)
	}
	c.meter.SetFlushInterval(next.TrafficFlushInterval.Std())
	if err := logging.SetLevel(next.LogLevel); err != nil {
		logging.Warnf("service settings: set log level: %v", err)
	}
	// 日志未输出到文件时轮转参数无意义，只告警不改状态。
	if err := logging.SetLogRotation(next.LogRotation); err != nil {
		logging.Warnf("service settings: set log rotation: %v", err)
	}
	c.applyServerProxy(ctx, next.ServerProxy, local)
	c.cur.Store(&next)
	if c.bindings != nil && globalChanged {
		c.bindings.SetGlobalClientConfig(next.GlobalClient)
	}
}

func (c *serviceConfig) pushClientConfigs(ctx context.Context) {
	if c.bindings == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), applyTimeout)
	defer cancel()
	for _, client := range c.svc.sessions.ClientSessions() {
		if ctx.Err() != nil {
			return
		}
		if err := c.bindings.PushClientConfigContext(ctx, client); err != nil {
			logging.Warn("service settings: push client config failed", "client", client.Info().ClientKey(), "err", err)
		}
	}
}

// applyServerProxy 热改本机出口：替换 resolver 的本机出口，并让 client_bindings 的
// 「本机」行随开关增删。停用不影响在途隧道（已发出的出口由调用方持有）。
func (c *serviceConfig) applyServerProxy(ctx context.Context, sp config.ServerProxySettings, local *localExit) {
	if c.resolver != nil {
		c.resolver.SetLocal(local)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), applyTimeout)
	defer cancel()
	syncLocalClient(ctx, c.settings, c.bindings, sp.Enabled)
}
