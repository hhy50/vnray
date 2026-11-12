package inbound

// ss 插件：Shadowsocks AEAD TCP 入站 + 密码 → 客户端 路由。
// 密码路由与其限速配置存于 vn_user_bindings 表（protocol = ss），经注入的 UserStore 读写；
// 插件自身的启停状态、监听端口与默认加密方法留在 vn_plugin 表。

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/shadowsocks"
)

// pluginSS 是 ss 插件在 vn_plugin 表里的 name。
const pluginSS = "ss"

// DefaultSSMethod 是未显式配置时的加密方法。
const DefaultSSMethod = shadowsocks.DefaultMethod

// SSRoute 是一条 ss 密码 → 客户端 的路由。
type SSRoute struct {
	Method   string `json:"method"`
	Password string `json:"password"`
	Client   string `json:"client"`
}

// SSPlugin 提供 Shadowsocks 入站：认证密码后按路由把流量经绑定客户端的隧道送出。
type SSPlugin struct {
	runtime   plugin.Runtime
	run       *plugin.InboundRun
	store     store.Store
	users     UserStore
	rules     RuleEvaluator
	exits     transport.ExitResolver
	limits    *ratelimit.Registry
	traffic   *traffic.Meter
	listener  net.Listener
	validator *shadowsocks.Validator
	port      int
	method    string
	running   bool
}

// SetTraffic 注入流量记账器；须在插件启动前调用。
func (p *SSPlugin) SetTraffic(t *traffic.Meter) { p.traffic = t }

// SetRules 注入用户出站规则；须在插件启动前调用。
func (p *SSPlugin) SetRules(rules RuleEvaluator) { p.rules = rules }

// BindDrainGate 在注册时接入管理器的收尾状态门。
func (p *SSPlugin) BindDrainGate(g *plugin.DrainGate) { p.runtime.BindDrainGate(g) }

// NewSSPlugin 创建由装配层注入依赖的 Shadowsocks 插件。
func NewSSPlugin(st store.Store, users UserStore, exits transport.ExitResolver, limits *ratelimit.Registry) *SSPlugin {
	method, _ := shadowsocks.ParseMethod(DefaultSSMethod)
	return &SSPlugin{
		store:     st,
		runtime:   plugin.NewRuntime(),
		users:     users,
		exits:     exits,
		limits:    limits,
		validator: shadowsocks.NewValidator(method),
		method:    DefaultSSMethod,
	}
}

// Name 返回插件名。
func (p *SSPlugin) Name() string { return pluginSS }

// Status 返回当前状态：监听端口与加密方法。
func (p *SSPlugin) Status() plugin.Status {
	return plugin.Status{
		Name:    pluginSS,
		Enabled: p.running, LastError: p.runtime.LastError(),
		Ports:  []plugin.PortKV{{Protocol: store.ProtocolSS, Port: p.port}},
		Params: map[string]string{"method": p.method},
	}
}

// SyncUser 把用户表的一条 ss 变更同步进运行中的验证器：管理台在用户页增删或启用/停用
// ss 用户时，入站认证必须立刻跟随，否则会出现"表里有、连不上"或"删了还能连"的偏斜。
// 其他协议的变更与本插件无关，直接忽略。
func (p *SSPlugin) SyncUser(proto store.Protocol, key, client string, status store.UserStatus, deleted bool) {
	if proto != store.ProtocolSS {
		return
	}
	// 停用与删除对验证器而言动作相同：摘掉，密码不再能试解通过。
	if deleted || status != store.UserEnabled {
		p.validator.Remove(key)
		return
	}
	method := p.method
	if u, ok := p.users.LookupUser(p.runtime.Context(), proto, key); ok && u.Config.SSMethod != "" {
		method = u.Config.SSMethod
	}
	if err := p.validator.AddWithMethod(key, client, method); err != nil {
		logging.Warn("ss: sync user failed", "err", err)
	}
}

// Restore 恢复启停状态、方法与密码路由，错误由宿主处理。
func (p *SSPlugin) Restore(ctx context.Context) error {
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	cur, ok, err := p.store.GetPlugin(ctx, pluginSS)
	if err != nil {
		return p.runtime.Record(err)
	}
	method := p.method
	if cur.Config.Method != "" {
		method = cur.Config.Method
	}
	prepared, err := p.prepareUsers(ctx, method)
	if err != nil {
		return p.runtime.Record(err)
	}
	p.validator.ReplaceUsers(prepared)
	p.method = method
	if port := cur.Config.Ports[store.ProtocolSS]; port > 0 {
		p.port = port
	}
	if !ok || cur.Status != store.PluginEnabled {
		return nil
	}
	if p.port <= 0 {
		return p.runtime.Record(fmt.Errorf("plugin: invalid ss port %d", p.port))
	}
	return p.runtime.Record(p.configure(ctx, p.port, p.method, false))
}

func (p *SSPlugin) save(ctx context.Context, status store.PluginStatus, port int, method string) error {
	return plugin.SaveState(ctx, p.store, pluginSS, status, func(c *store.PluginConfig) {
		c.Ports[store.ProtocolSS] = port
		c.Method = method
	})
}

// Enable 启用插件：校验并持久化端口与方法，然后立即开始监听。
// params 里 port 必填，method 可选（缺省沿用已存值或默认方法）。
func (p *SSPlugin) Enable(ctx context.Context, params map[string]string) error {
	port, err := strconv.Atoi(params["port"])
	if err != nil || port <= 0 || port >= 65536 {
		return fmt.Errorf("plugin: invalid ss port %q", params["port"])
	}
	method := params["method"]
	if method != "" {
		if _, err := shadowsocks.ParseMethod(method); err != nil {
			return fmt.Errorf("plugin: %w", err)
		}
	}
	if method == "" {
		method = p.method
	}
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	return p.runtime.Record(p.configure(ctx, port, method, true))
}

// Disable 停用插件：停止监听并持久化状态。
func (p *SSPlugin) Disable(ctx context.Context) error {
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	if err := p.save(ctx, store.PluginDisabled, p.port, p.method); err != nil {
		return err
	}
	p.stop()
	return nil
}

// SetPort 准备新监听并持久化成功后切换；失败保留旧运行态。
func (p *SSPlugin) SetPort(ctx context.Context, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("plugin: invalid ss port %d", port)
	}
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	if !p.running {
		if err := p.save(ctx, "", port, p.method); err != nil {
			return p.runtime.Record(err)
		}
		p.port = port
		return p.runtime.Record(nil)
	}
	return p.runtime.Record(p.configure(ctx, port, p.method, true))
}

func (p *SSPlugin) configure(ctx context.Context, port int, method string, persist bool) error {
	var prepared *shadowsocks.Validator
	if method != p.method {
		var err error
		prepared, err = p.prepareUsers(ctx, method)
		if err != nil {
			return err
		}
	}
	var ln net.Listener
	if !p.running || p.port != port {
		var err error
		ln, err = net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			return fmt.Errorf("plugin: ss listen :%d: %w", port, err)
		}
	}
	if persist {
		if err := p.save(ctx, store.PluginEnabled, port, method); err != nil {
			if ln != nil {
				_ = ln.Close()
			}
			return err
		}
	}
	if prepared != nil {
		p.validator.ReplaceUsers(prepared)
		p.method = method
	}
	if ln != nil {
		p.stop()
		p.listener, p.port, p.running = ln, port, true
		run, err := p.runtime.StartInbound("plugin.ss.accept", ln, func(ctx context.Context) plugin.Server {
			return shadowsocks.NewServer(p.validator, func(password, client string, target shadowsocks.Target, conn *shadowsocks.Conn) {
				p.handle(ctx, password, client, target, conn)
			})
		}, func(err error) {
			if p.listener == ln {
				p.running = false
				p.runtime.Record(err)
			}
		})
		p.run = run
		if err != nil {
			p.running = false
			return err
		}
	}
	return nil
}

// Drain 终止所有监听代次的存量连接并等待退出。
func (p *SSPlugin) Drain(ctx context.Context) error {
	runs := p.runtime.BeginDrain()
	p.stop()
	return plugin.DrainRuns(ctx, runs)
}

// Stop 停止监听，既有连接由 Drain 或对端结束。
func (p *SSPlugin) Stop() {
	p.stop()
}

func (p *SSPlugin) prepareUsers(ctx context.Context, method string) (*shadowsocks.Validator, error) {
	m, err := shadowsocks.ParseMethod(method)
	if err != nil {
		return nil, err
	}
	routes, err := p.ssRoutes(ctx, method)
	if err != nil {
		return nil, err
	}
	prepared := shadowsocks.NewValidator(m)
	for _, r := range routes {
		if err := prepared.AddWithMethod(r.Password, r.Client, r.Method); err != nil {
			return nil, err
		}
	}
	return prepared, nil
}

func (p *SSPlugin) stop() {
	if p.run != nil {
		p.run.Stop()
	}
	p.running = false
}

// ssRoutes 返回当前全部启用中的密码 → 客户端 路由（从用户表读取）。停用的用户不是路由，
// 这里就不列出——它同时决定了重启后验证器的灌入范围。
func (p *SSPlugin) ssRoutes(ctx context.Context, defaultMethod string) ([]SSRoute, error) {
	if p.users == nil {
		return nil, nil
	}
	all, err := p.users.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("ss: list users: %w", err)
	}
	out := make([]SSRoute, 0, len(all))
	for _, u := range all {
		if u.Protocol != store.ProtocolSS || u.Status != store.UserEnabled {
			continue
		}
		method := u.Config.SSMethod
		if method == "" {
			method = defaultMethod
		}
		out = append(out, SSRoute{Password: u.Key, Client: u.Client, Method: method})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Password < out[j].Password })
	return out, nil
}

// SSRoutes 返回按密码排序的路由列表（诊断与测试的观察口，读用户表）。
func (p *SSPlugin) SSRoutes() ([]SSRoute, error) {
	return p.ssRoutes(p.runtime.Context(), p.method)
}

// AccountLimits 返回 ss 账号当前的上下行限速（字节/秒；0 = 不限速）。
func (p *SSPlugin) AccountLimits(account string) (up, down int64) {
	u, ok := p.users.LookupUser(p.runtime.Context(), store.ProtocolSS, account)
	if !ok {
		return 0, 0
	}
	return u.Config.UpLimit, u.Config.DownLimit
}

// handle 把解码完成的 Shadowsocks 连接经绑定客户端的隧道送达目标。
func (p *SSPlugin) handle(ctx context.Context, password, client string, target shadowsocks.Target, conn *shadowsocks.Conn) {
	stats := p.traffic.UserStats(store.ProtocolSS, password)
	defer stats.OpenTCP().CloseTCP()
	defer stats.OpenStream().CloseStream()
	stats.Request()
	log := p.userLog(password)
	policy := userPolicy{users: p.users, rules: p.rules, proto: store.ProtocolSS, key: password}
	direct, err := policy.check(ctx, target.Host)
	if err != nil {
		log.Info("ss: destination denied", "client", client, "target", target.String(), "err", err)
		_ = conn.Close()
		return
	}
	exit, ok := p.exits.Resolve(client)
	if !ok {
		if client == transport.LocalClientName {
			log.Warn("ss: server:self bound but server_proxy is disabled, closing ss conn",
				"client", client, "target", target.String())
		} else {
			log.Warn("ss: bound client offline, closing ss conn",
				"client", client, "target", target.String())
		}
		_ = conn.Close()
		return
	}

	// 用户级并发：占一个并发位再建隧道，超限排队。
	if p.limits != nil {
		gate := p.limits.Gate(ratelimit.Identity(store.ProtocolSS, password))
		if err := acquireUserGate(ctx, gate); err != nil {
			log.Warn("ss: user concurrency limit reached", "client", client, "target", target.String(), "err", err)
			_ = conn.Close()
			return
		}
		defer gate.Release()
	}
	tunnel, err := transport.OpenTunnel(ctx, exit,
		dispatch.Destination{Host: target.Host, Port: target.Port},
		protocol.TunnelRequest{Origin: "ss", Direct: direct})
	if err != nil {
		log.Warn("ss: open tunnel failed",
			"client", client, "target", target.String(), "err", err)
		_ = conn.Close()
		return
	}
	log.Info("ss: tunnel established", "client", client, "target", target.String())
	// 按密码限速与统计：上行限 ss 连接的 reader（用户发多快），
	// 下行限隧道 reader（客户端送多快）；字节按密码与 client 两个维度各记一份。
	o := transport.ProxyOptions{Context: ctx}
	if p.limits != nil {
		pair := p.limits.Pair(ratelimit.Identity(store.ProtocolSS, password))
		o.UpLimit, o.DownLimit, o.Stats = pair.Up, pair.Down, pair.Stats
	}
	if p.traffic != nil {
		o.Stats = stats
		o.ClientStats = p.traffic.ClientStats(client)
	}
	transport.ProxyBidirectional(conn, tunnel, "plugin.ss.tunnel", o)
	_ = conn.Close()
	_ = tunnel.Close()
	log.Info("ss: tunnel closed", "client", client, "target", target.String())
}

// userLog 返回该 ss 账号的会话日志器：级别取用户配置里的 log_level（默认 info），
// user 字段用掩码后的密码，另带上管理台显示名——隧道日志不再逐处重复传 user。
func (p *SSPlugin) userLog(password string) *logging.Scoped {
	level, name := "", ""
	if p.users != nil {
		if u, ok := p.users.LookupUser(p.runtime.Context(), store.ProtocolSS, password); ok {
			level = u.Config.LogLevel
			name = u.Name
		}
	}
	attrs := []any{"user", shadowsocks.MaskPassword(password)}
	if name != "" {
		attrs = append(attrs, "name", name)
	}
	return logging.NewScoped(level, attrs...)
}
