package core

import (
	"context"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/portmap"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
)

// serverApp 是服务端进程的运行时组装。
type serverApp struct {
	instance     *Instance
	bindingStore store.Store
}

// Run 按序启动全部服务，阻塞到 app ctx 取消（SIGINT/SIGTERM）后逆序收尾，最后释放存储。
func (app *serverApp) Run() error {
	err := app.instance.Run()
	if cerr := app.bindingStore.Close(); cerr != nil {
		logging.Warnf("close binding store: %v", cerr)
	}
	return err
}

// newServerApp 组装服务端运行时：Instance 承载服务编排，store 等 Run 结束时才关闭。
func newServerApp(appctx context.Context, instance *Instance, st store.Store) *serverApp {
	return &serverApp{instance: instance, bindingStore: st}
}

// newBindingStore 按配置构造绑定表持久层（sqlite/mysql）；internal/config 不依赖
// 数据面包，驱动翻译放在 config，这里只透传。
func newBindingStore(ctx context.Context, cfg *config.Server) (store.Store, error) {
	return store.Open(ctx, cfg.Storage.ResolvedDriver(), cfg.Storage.DSN)
}

// newBindings 打开用户绑定表（user_key → 客户端名，持久层由 storage 配置决定）；
// 管理面与用户注册表共用同一份。
func newBindings(st store.Store) *admin.Bindings {
	return admin.NewBindings(st)
}

// seedAuthKeys 把配置文件的共享密钥迁移成范围密钥：仅当密钥表为空且配置了
// server.auth.key 时种子一条 scope=* 的记录——老部署升级后不用去管理台补配；
// 表里已有条目则以管理台为准，配置文件里的密钥不再生效。
func seedAuthKeys(b *admin.Bindings, cfg *config.Server) {
	key := cfg.Server.Auth.Key
	if key == "" || len(b.AuthKeySnapshot()) > 0 {
		return
	}
	if err := b.SetAuthKey(key, "*"); err != nil {
		logging.Warnf("seed auth key from config: %v", err)
		return
	}
	logging.Info("seeded scope=* auth key from server.auth.key; manage keys in the admin panel")
}

// newAdminService 管理面服务；admin.enabled 为 false 时返回 nil（newInstance 跳过）。
// 管理库打不开视为装配失败：管理台是运维入口，带病启动不如显式报错。
func newAdminService(cfg *config.Server, sessions *transport.SessionManager, bindings *admin.Bindings, plugins *plugin.Manager, st store.Store, pm *portmap.Manager, settings admin.ServiceSettings) (*admin.AdminService, error) {
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
		DB:       bindings.GORM(),
		Store:    st,
		LogPath:  logFilePath(cfg),
		Settings: settings,
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
func newUserRegistry(userBindings *admin.Bindings) transport.UserRegistry {
	return userBindings
}

// initServerApp 手写组装服务端进程：绑定表存储 → 绑定表 → 会话管理器 →
// 数据面服务（+ 管理面，可选）→ Instance；字段间依赖关系一目了然，
func initServerApp(appctx context.Context, cfg *config.Server) (*serverApp, error) {
	if cfg.Diagnostics.PprofListen != "" {
		startPprof(appctx, cfg.Diagnostics.PprofListen)
	}
	sessions := transport.NewSessionManager()
	st, err := newBindingStore(appctx, cfg)
	if err != nil {
		return nil, err
	}
	// 运行时服务配置（管理台可在线改）先于组件构造解析：会话参数与心跳在此定基线。
	settings := loadServiceSettings(appctx, st, cfg)
	bindings := newBindings(st)
	seedAuthKeys(bindings, cfg)
	limits := ratelimit.NewRegistry()
	meter := traffic.NewMeter(limits)
	// 出口解析器：settings 里本机出口启用时附带它（保留名 server:self）。
	exits, err := buildExitResolver(settings, sessions)
	if err != nil {
		return nil, err
	}
	// 本机出口登记随开关：启用登记、停用移除（保留流量）。
	{
		ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
		syncLocalClient(ctx, st, bindings, settings.ServerProxy.Enabled)
		cancel()
	}
	plugins := plugin.NewManager(st, bindings, exits, limits)
	plugins.SetSeedEnabled(cfg.Plugin.Enabled)
	plugins.SetTraffic(meter)
	// 用户表是唯一写口：限速推给注册表，凭据路由推给各入站插件（按类型过滤）。
	bindings.Watch(func(c admin.UserChange) {
		propagateUserChange(c, limits, plugins)
	})
	seedLimits(bindings, limits)
	svc := NewConnectorServiceWith(cfg, sessions, newUserRegistry(bindings), limits,
		tcp.WithSessionConfig(sessionConfigFromSettings(settings.Session)))
	svc.ApplyHeartbeat(heartbeatIntervalOf(settings.Heartbeat), settings.Heartbeat.Timeout.Std())
	svc.SetSessionConfig(bindings)
	svc.SetExitResolver(exits)
	svc.SetTraffic(meter)
	// 端口映射：规则读自 port_mappings 表，管理台改表后热更新监听与限速。
	portmaps := portmap.NewManager(sessions, st, meter, limits)
	meter.SetFlushInterval(settings.TrafficFlushInterval.Std())
	applyStartupLogging(cfg, settings)
	runtimeCfg := newServiceConfig(st, svc, meter, exits, bindings, settings)
	webAdmin, err := newAdminService(cfg, sessions, bindings, plugins, st, portmaps, runtimeCfg)
	if err != nil {
		return nil, err
	}
	// 流量增量随进程生命周期落库：ctx 结束时会补最后一次。
	meter.Run(appctx, st)
	instance := newInstance(appctx, svc, webAdmin, plugins, portmaps)
	return newServerApp(appctx, instance, st), nil
}

// propagateUserChange 把一条用户变更分发给运行时消费者：限速注册表（各类用户都限速）
// 与全部实现了 UserSyncer 的插件（各自按类型过滤）。
func propagateUserChange(c admin.UserChange, limits *ratelimit.Registry, plugins *plugin.Manager) {
	identity := ratelimit.Identity(c.Protocol, c.Key)
	if c.Deleted {
		limits.Forget(identity)
	} else {
		limits.Apply(identity, c.Config.UpLimit, c.Config.DownLimit)
		limits.Concurrency(identity, c.Config.Concurrency)
	}
	plugins.SyncUser(c.Protocol, c.Key, c.Client, c.Status, c.Deleted)
}

// seedLimits 把用户表里的限速与并发配置全量灌进限速注册表（启动时的一次性投影）。
func seedLimits(b *admin.Bindings, limits *ratelimit.Registry) {
	for _, u := range b.Snapshot() {
		limits.Apply(ratelimit.Identity(u.Protocol, u.UserKey), u.Config.UpLimit, u.Config.DownLimit)
		limits.Concurrency(ratelimit.Identity(u.Protocol, u.UserKey), u.Config.Concurrency)
	}
}
