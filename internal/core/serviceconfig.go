package core

import (
	"context"
	"reflect"
	"sync"
	"time"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/traffic"
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

// loadServiceSettings 读取管理台保存的运行时配置段并覆盖基线。首次启动（库里没有初始化
// 标记）时把生效基线整份落库，此后一律以库为准。任何读取/解析/校验失败都只告警并回退
// 基线，不拒绝启动——坏配置不该拦住服务进程。
func loadServiceSettings(ctx context.Context, st store.Store, cfg *config.Server) config.ServiceSettings {
	base := defaultServiceSettings(cfg)
	kv, err := st.GetServiceSettings(ctx)
	if err != nil {
		logging.Warnf("service settings: load failed, using defaults: %v", err)
		return base
	}
	if _, initialized := kv[store.ServiceSettingsInitializedKey]; !initialized {
		seed := kv
		if len(seed) == 0 {
			if seed, err = config.EncodeServiceSettings(base); err != nil {
				logging.Warnf("service settings: encode defaults: %v", err)
				return base
			}
			logging.Info("service settings: first start, seeding defaults")
		}
		// 已有段但没有标记（老库）：原样写回一次补标记，不覆盖用户数据。
		seed[store.ServiceSettingsInitializedKey] = "true"
		if err := st.SetServiceSettings(ctx, seed); err != nil {
			logging.Warnf("service settings: seed failed: %v", err)
		}
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
	applyMu  sync.Mutex
	mu       sync.RWMutex
	cur      config.ServiceSettings
	settings store.Store
	svc      *ConnectorService
	meter    *traffic.Meter
	// resolver 与 bindings 用于热改本机出口：原子换 local 出口、增删 client_bindings 行。
	resolver *exitResolver
	bindings *admin.Bindings
}

// newServiceConfig 构造分发中心；initial 是启动解析出的生效值。
func newServiceConfig(st store.Store, svc *ConnectorService, meter *traffic.Meter, resolver *exitResolver, bindings *admin.Bindings, initial config.ServiceSettings) *serviceConfig {
	initial.GlobalClient = initial.GlobalClient.Clone()
	if bindings != nil {
		bindings.SetGlobalClientConfig(initial.GlobalClient)
		svc.sessions.ClientReady = bindings.PushClientConfig
	}
	return &serviceConfig{cur: initial, settings: st, svc: svc, meter: meter, resolver: resolver, bindings: bindings}
}

// Current 返回当前生效的运行时配置快照。
func (c *serviceConfig) Current() config.ServiceSettings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	current := c.cur
	current.GlobalClient = current.GlobalClient.Clone()
	return current
}

// VLESSTLS 返回 vless 的 TLS 参数（副本）；从未配置过时返回 nil。
func (c *serviceConfig) VLESSTLS() *config.VLESSTLS {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cur.VLESS.TLS == nil {
		return nil
	}
	cp := *c.cur.VLESS.TLS
	return &cp
}

// SetVLESSTLS 只写 vless 这一段：证书由插件签发，不触发其它组件的热分发，也不碰其它
// 段（并发改日志级别之类的写入不会互相覆盖）。本地快照同步更新，否则管理台以快照为
// 基线保存服务配置时会把证书冲掉。
func (c *serviceConfig) SetVLESSTLS(t config.VLESSTLS) error {
	raw, err := config.EncodeVLESSSettings(config.VLESSSettings{TLS: &t})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
	defer cancel()
	if err := c.settings.SetServiceSettings(ctx, map[string]string{config.SettingVLESS: raw}); err != nil {
		return err
	}
	c.mu.Lock()
	c.cur.VLESS.TLS = &t
	c.mu.Unlock()
	return nil
}

// Apply 校验并落库新的运行时配置；落库失败不改运行态，成功后热分发到各组件。
func (c *serviceConfig) Apply(next config.ServiceSettings) error {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	next.GlobalClient = next.GlobalClient.Clone()
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
	ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
	defer cancel()
	if err := c.settings.SetServiceSettings(ctx, kv); err != nil {
		return err
	}
	c.dispatch(next, sess, local)
	return nil
}

// dispatch 把生效值推给各运行时组件并更新本地快照。日志级别非法不可能发生（已被校验）。
func (c *serviceConfig) dispatch(next config.ServiceSettings, sess *tcp.SessionConfig, local *localExit) {
	globalChanged := !reflect.DeepEqual(c.Current().GlobalClient, next.GlobalClient)
	c.svc.ApplyHeartbeat(heartbeatIntervalOf(next.Heartbeat), next.Heartbeat.Timeout.Std())
	c.svc.ApplySessionConfig(sess)
	c.meter.SetFlushInterval(next.TrafficFlushInterval.Std())
	if err := logging.SetLevel(next.LogLevel); err != nil {
		logging.Warnf("service settings: set log level: %v", err)
	}
	// 日志未输出到文件时轮转参数无意义，只告警不改状态。
	if err := logging.SetLogRotation(next.LogRotation); err != nil {
		logging.Warnf("service settings: set log rotation: %v", err)
	}
	c.applyServerProxy(next.ServerProxy, local)
	c.mu.Lock()
	c.cur = next
	c.mu.Unlock()
	if c.bindings != nil && globalChanged {
		c.bindings.SetGlobalClientConfig(next.GlobalClient)
		for _, client := range c.svc.sessions.ClientSessions() {
			if err := c.bindings.PushClientConfig(client); err != nil {
				logging.Warn("service settings: push client config failed", "client", client.Info().Name, "err", err)
			}
		}
	}
}

// applyServerProxy 热改本机出口：替换 resolver 的本机出口，并让 client_bindings 的
// 「本机」行随开关增删。停用不影响在途隧道（已发出的出口由调用方持有）。
func (c *serviceConfig) applyServerProxy(sp config.ServerProxySettings, local *localExit) {
	if c.resolver != nil {
		c.resolver.SetLocal(local)
	}
	ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
	defer cancel()
	syncLocalClient(ctx, c.settings, c.bindings, sp.Enabled)
}
