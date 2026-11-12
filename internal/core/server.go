package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/access"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/extension"
	"github.com/hhy50/vnray/internal/inbound"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/metrics"
	"github.com/hhy50/vnray/internal/monitor"
	"github.com/hhy50/vnray/internal/pipe"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/portmap"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/sqliteweb"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
)

// serverApp 是服务端进程的运行时组装。
type serverApp struct {
	instance     *Instance
	bindingStore store.Store
	admin        *admin.AdminService
	monitor      *monitor.Service
}

// Run 按序启动全部服务，阻塞到 app ctx 取消（SIGINT/SIGTERM）后逆序收尾，最后释放存储。
func (app *serverApp) Run() error {
	err := app.instance.Run()
	if app.admin != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		waitErr := app.admin.WaitDrained(ctx)
		cancel()
		if waitErr != nil {
			return errors.Join(err, waitErr)
		}
	}
	if app.monitor != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		waitErr := app.monitor.Drain(ctx)
		cancel()
		if waitErr != nil {
			return errors.Join(err, waitErr)
		}
	}
	return errors.Join(err, app.bindingStore.Close())
}

// newServerApp 组装服务端运行时：Instance 承载服务编排，store 等 Run 结束时才关闭。
func newServerApp(instance *Instance, st store.Store) *serverApp {
	return &serverApp{instance: instance, bindingStore: st}
}

// newBindings 打开用户绑定表（user_key → 客户端名，持久层由 storage 配置决定）；
// 管理面与用户注册表共用同一份。
func newBindings(st store.Store) *access.Bindings {
	return access.NewBindings(st)
}

// newAdminService 管理面服务；admin.enabled 为 false 时返回 nil（装配时跳过）。
// 管理库打不开视为装配失败：管理台是运维入口，带病启动不如显式报错。
func newAdminService(cfg *config.Server, sessions *transport.SessionManager, bindings *access.Bindings, plugins *plugin.Manager, st store.Store, pm *portmap.Manager, settings admin.ServiceSettings, rules admin.RuleSettings, monitoring admin.MonitoringReader) (*admin.AdminService, error) {
	if !cfg.Admin.Enabled {
		return nil, nil
	}
	return admin.NewAdminService(cfg.Admin.Addr(), sessions, admin.AdminOptions{
		Version:  version,
		Bindings: bindings,
		Plugins:  plugins,
		PortMaps: pm,
		Listen:   cfg.Server.Listen,
		Username: cfg.Admin.ResolvedUsername(),
		Password: cfg.Admin.Password,
		Store:    st,
		Monitor:  monitoring,
		Mappings: portmap.NewControl(st, pm),
		LogPath:  logFilePath(cfg),
		Settings: settings,
		Rules:    rules,
	})
}

// applyStartupLogging 把生效的日志级别与轮转参数推给 logging。logging.Init 早于 store 打开，
// 故这里再覆盖一次（毫秒级窗口内先按配置文件级别输出）；轮转仅在输出到文件时有意义。
func applyStartupLogging(cfg *config.Server, settings config.ServiceSettings) {
	if err := logging.SetLevel(settings.LogLevel); err != nil {
		logging.Warnf("service settings: set log level: %v", err)
	}
	if cfg.Log == nil || cfg.Log.IsStdOutput() {
		return
	}
	if err := logging.SetLogRotation(settings.LogRotation); err != nil {
		logging.Warnf("service settings: set log rotation: %v", err)
	}
}

// logFilePath 返回日志文件路径；输出到 stdout/stderr 时返回空（日志页会提示）。
func logFilePath(cfg *config.Server) string {
	if cfg.Log == nil {
		return ""
	}
	switch cfg.Log.Output {
	case "", "stdout", "stderr":
		return ""
	default:
		return cfg.Log.Output
	}
}

// newUserRegistry 把绑定表暴露为用户注册表：数据面只认接口，实现由装配层给定。
func newUserRegistry(userBindings *access.Bindings) transport.UserRegistry {
	return userBindings
}

// initServerApp 手写组装服务端进程：绑定表存储 → 绑定表 → 会话管理器 →
// 数据面服务（+ 管理面，可选）→ Instance；字段间依赖关系一目了然，
func initServerApp(appctx context.Context, cfg *config.Server) (*serverApp, error) {
	// pipe 保持零项目内依赖，缓冲指标经观察者注入；须在数据面启动前设置。
	pipe.SetBufferObserver(metrics.GaugeFor("pipe_buffered_bytes").Add)
	st, err := newBindingStore(appctx, cfg)
	if err != nil {
		return nil, err
	}
	assembled := false
	defer func() {
		if !assembled {
			_ = st.Close()
		}
	}()
	sessions := transport.NewSessionManager()
	// 运行时服务配置（管理台可在线改）先于组件构造解析：会话参数与心跳在此定基线。
	settings := loadServiceSettings(appctx, st, cfg)
	// 指标采集开关来自运行时配置：须在数据面开工前生效。
	metrics.SetEnabled(settings.MetricsEnabled)
	// 用户出站规则同样先解析：数据面按规则名判定，管理台经同一实例读写。
	rules := loadRuleSet(appctx, st)
	bindings := newBindings(st)
	if err := bindings.ReloadAuthKeys(appctx); err != nil {
		return nil, err
	}
	limits := ratelimit.NewRegistry()
	meter := traffic.NewMeter(limits)
	// 出口解析器：settings 里本机出口启用时附带它（保留名 server:self）。
	exits, err := buildExitResolver(settings, sessions)
	if err != nil {
		return nil, err
	}
	// 本机出口登记随开关：启用登记、停用移除（保留流量）。
	{
		ctx, cancel := context.WithTimeout(appctx, applyTimeout)
		syncLocalClient(ctx, st, bindings, settings.ServerProxy.Enabled)
		cancel()
	}
	userInbound := inbound.NewVNRayUserPlugin(st, inbound.VNRayUserDependencies{
		Events: sessions, Users: newUserRegistry(bindings), UserConfig: bindings,
		Clients: sessions, SessionConfig: sessionConfigFromSettings(settings.Session),
	})
	v2ray := inbound.NewV2RayPlugin(st, bindings, exits, limits)
	v2ray.SetRules(rules)
	ss := inbound.NewSSPlugin(st, bindings, exits, limits)
	ss.SetRules(rules)
	plugins := plugin.NewRegistry(appctx,
		v2ray,
		ss,
		sqliteweb.New(st),
		userInbound,
	)
	plugins.SetTraffic(meter)
	// 用户表是唯一写口：限速推给注册表，凭据路由推给各入站插件（按类型过滤）。
	bindings.Watch(func(c access.UserChange) {
		propagateUserChange(c, limits, plugins, meter)
	})
	if err := seedLimits(appctx, st, limits, meter); err != nil {
		return nil, err
	}
	monitoring := newMonitoring(st, meter, settings)
	bindings.Watch(func(access.UserChange) { monitoring.InvalidateUsers() })
	svc := NewConnectorServiceWith(cfg, sessions, newUserRegistry(bindings), limits,
		tcp.WithSessionConfig(sessionConfigFromSettings(settings.Session)))
	svc.ApplyHeartbeat(heartbeatIntervalOf(settings.Heartbeat), settings.Heartbeat.Timeout.Std())
	svc.SetSessionConfig(bindings)
	svc.SetExitResolver(exits)
	svc.SetTraffic(meter)
	svc.SetRules(rules)
	// 端口映射：规则读自 port_mappings 表，管理台改表后热更新监听与限速。
	portmaps := portmap.NewManager(appctx, sessions, st, meter, limits)
	meter.SetFlushInterval(settings.TrafficFlushInterval.Std())
	applyStartupLogging(cfg, settings)
	runtimeCfg := newServiceConfig(st, svc, meter, exits, bindings, settings)
	runtimeCfg.monitor = monitoring
	runtimeCfg.setSessionConfigApplier(plugins)
	// vless 的 TLS 参数存在服务端配置表里，插件经这一个写口读写。
	plugins.SetVLESSTLS(runtimeCfg)
	webAdmin, err := newAdminService(cfg, sessions, bindings, plugins, st, portmaps, runtimeCfg, rules, monitoring)
	if err != nil {
		return nil, err
	}
	services := []extension.Service{traffic.NewService(meter, st), svc, plugins, portmaps, monitoring}
	if cfg.Diagnostics.PprofListen != "" {
		services = append(services, newPprofService(cfg.Diagnostics.PprofListen))
	}
	// 管理面最后启动、最先停止，退出时不再接受新的配置写入。
	if webAdmin != nil {
		services = append(services, webAdmin)
	}
	instance := newInstance(appctx, services...)
	assembled = true
	app := newServerApp(instance, st)
	app.admin = webAdmin
	app.monitor = monitoring
	return app, nil
}

// propagateUserChange 把一条用户变更分发给运行时消费者：限速注册表与流量记账器
// （各类用户都限速、都计数）以及全部实现了 UserSyncer 的插件（各自按类型过滤）。
func propagateUserChange(c access.UserChange, limits *ratelimit.Registry, plugins *plugin.Manager, meter *traffic.Meter) {
	identity := ratelimit.Identity(c.Protocol, c.Key)
	if c.Deleted {
		limits.Forget(identity)
		if meter != nil {
			meter.ForgetUser(c.Protocol, c.Key)
		}
	} else {
		if meter != nil {
			meter.RestoreUser(c.Protocol, c.Key)
		}
		limits.Apply(identity, c.Config.UpLimit, c.Config.DownLimit)
		limits.Concurrency(identity, c.Config.Concurrency)
	}
	plugins.SyncUser(c.Protocol, c.Key, c.Client, c.Status, c.Deleted)
}

type userSeedStore interface {
	ListUsers(context.Context) ([]store.UserBinding, error)
}

// seedLimits 同步初始化用户限速与记账登记；读库失败不能启动缺少投影的服务。
func seedLimits(ctx context.Context, st userSeedStore, limits *ratelimit.Registry, meter *traffic.Meter) error {
	ctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	users, err := st.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("load user runtime state: %w", err)
	}
	for _, u := range users {
		if meter != nil {
			meter.RestoreUser(u.Protocol, u.Key)
		}
		limits.Apply(ratelimit.Identity(u.Protocol, u.Key), u.Config.UpLimit, u.Config.DownLimit)
		limits.Concurrency(ratelimit.Identity(u.Protocol, u.Key), u.Config.Concurrency)
	}
	return nil
}
