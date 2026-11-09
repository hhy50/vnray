package inbound

// v2ray 插件：VMess 与 VLESS 两个入站共用一个插件实例，各占一个监听端口。
// uuid 路由与其限速配置存于 vn_user_bindings 表（protocol = vmess / vless），经注入的
// UserStore 读写；插件自身的启停状态与端口留在 vn_plugin 表。

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/identity"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/vless"
	"github.com/hhy50/vnray/vmess"
)

// pluginV2Ray 是 v2ray 插件在 vn_plugin 表里的 name。
const pluginV2Ray = "v2ray"

// 出厂默认端口：未配置时 plugin.Status 与启用对话框都用它们预填。
const (
	defaultVMessPort = 11120
	defaultVLESSPort = 11121
)

// uuidSlots 是 v2ray 插件声明的入站槽位，顺序固定为 vmess → vless：plugin.Status.Ports 的
// 次序、管理台启用对话框里输入框的次序都按它排。
var uuidSlots = []struct {
	proto   store.Protocol
	defPort int
}{
	{store.ProtocolVMess, defaultVMessPort},
	{store.ProtocolVLESS, defaultVLESSPort},
}

// UserStore 提供只读的用户路由与配置查询。
type UserStore interface {
	LookupUser(ctx context.Context, p store.Protocol, key string) (store.UserBinding, bool)
	ListUsers(ctx context.Context) ([]store.UserBinding, error)
}

// UUIDRoute 是一条 uuid → 客户端 的路由。
type UUIDRoute struct {
	UUID     string         `json:"uuid"`
	Client   string         `json:"client"`
	Protocol store.Protocol `json:"protocol"`
}

// V2RayPlugin 提供 VMess 与 VLESS 两个入站：认证 uuid 后按路由把流量经绑定客户端的隧道送出。
type V2RayPlugin struct {
	runtime  plugin.Runtime
	store    store.Store
	users    UserStore
	rules    RuleEvaluator
	exits    transport.ExitResolver
	limits   *ratelimit.Registry
	traffic  *traffic.Meter
	inbounds []*uuidInbound
	routes   map[store.Protocol]*sync.Map
	tls      vlessTLS
	tlsStore plugin.VLESSTLSStore
}

// vlessTLS 是 vless 入站的 TLS 运行态：settings 是已落库的参数，cfg 为 nil 表示明文。
// 证书在 Enable/SetTLS/Restore 时签发或加载，运行期只读。
type vlessTLS struct {
	settings config.VLESSTLS
	cfg      *tls.Config
	cert     vless.Cert
}

// SetTraffic 注入流量记账器；须在插件启动前调用。
func (p *V2RayPlugin) SetTraffic(t *traffic.Meter) { p.traffic = t }

// SetRules 注入用户出站规则；须在插件启动前调用。
func (p *V2RayPlugin) SetRules(rules RuleEvaluator) { p.rules = rules }

// BindDrainGate 在注册时接入管理器的收尾状态门。
func (p *V2RayPlugin) BindDrainGate(g *plugin.DrainGate) { p.runtime.BindDrainGate(g) }

// SetVLESSTLSStore 注入 TLS 参数读写口；须在插件启动前调用。
func (p *V2RayPlugin) SetVLESSTLSStore(s plugin.VLESSTLSStore) { p.tlsStore = s }

// NewV2RayPlugin 创建由装配层注入依赖的 VMess/VLESS 插件。
func NewV2RayPlugin(st store.Store, users UserStore, exits transport.ExitResolver, limits *ratelimit.Registry) *V2RayPlugin {
	p := &V2RayPlugin{store: st, users: users, exits: exits, limits: limits, runtime: plugin.NewRuntime(),
		routes: make(map[store.Protocol]*sync.Map, len(uuidSlots))}
	for _, slot := range uuidSlots {
		p.inbounds = append(p.inbounds, p.newInbound(slot.proto))
		p.routes[slot.proto] = &sync.Map{}
	}
	return p
}

// newInbound 按协议构造入站：验证器与 Serve 是两种协议唯一的差异，其余生命周期共用。
func (p *V2RayPlugin) newInbound(proto store.Protocol) *uuidInbound {
	in := &uuidInbound{proto: proto}
	switch proto {
	case store.ProtocolVMess:
		v := vmess.NewValidator()
		in.validator = v
		in.newServer = func(ctx context.Context) plugin.Server {
			return vmess.NewServer(v, func(id identity.Identity, target vmess.Target, network string, conn *vmess.Conn) {
				p.serveConn(ctx, proto, id, target.Host, target.Port, network, conn)
			})
		}
	case store.ProtocolVLESS:
		v := vless.NewValidator()
		in.validator = v
		in.newServer = func(ctx context.Context) plugin.Server {
			return vless.NewServer(v, func(id identity.Identity, target vless.Target, network string, conn net.Conn) {
				p.serveConn(ctx, proto, id, target.Host, target.Port, network, conn)
			})
		}
	}
	return in
}

// inbound 按协议取槽位；该协议不属于本插件时返回 nil。
func (p *V2RayPlugin) inbound(proto store.Protocol) *uuidInbound {
	for _, in := range p.inbounds {
		if in.proto == proto {
			return in
		}
	}
	return nil
}

// Name 返回插件名。
func (p *V2RayPlugin) Name() string { return pluginV2Ray }

// Status 返回当前状态：两个入站各自的端口、全部 uuid 路由与 vless 的 TLS 参数。
func (p *V2RayPlugin) Status() plugin.Status {
	params := make(map[string]string)
	for _, r := range p.uuidRoutes() {
		params[r.UUID] = r.Client
	}
	ports := make([]plugin.PortKV, 0, len(uuidSlots))
	for _, slot := range uuidSlots {
		ports = append(ports, plugin.PortKV{
			Protocol: slot.proto,
			Port:     p.inbound(slot.proto).effectivePort(slot.defPort),
		})
	}
	return plugin.Status{
		Name: pluginV2Ray, Enabled: p.running(), LastError: p.runtime.LastError(),
		Ports: ports, Params: params, TLS: p.tlsInfo(),
	}
}

func (p *V2RayPlugin) tlsInfo() *plugin.TLSInfo {
	info := &plugin.TLSInfo{
		Protocol:   store.ProtocolVLESS,
		Enabled:    p.tls.settings.Enabled,
		SelfSigned: p.tls.cert.Leaf != nil,
	}
	if p.tls.cert.Leaf == nil {
		info.ServerName = p.tls.settings.ServerName
		return info
	}
	info.ServerName = p.tls.cert.ServerName
	info.NotAfter = p.tls.cert.NotAfter.Format(time.RFC3339)
	info.Fingerprint = p.tls.cert.Fingerprint
	return info
}

// SyncUser 把用户表的一条 uuid 变更同步进对应协议的验证器：管理台在用户页增删或
// 启用/停用 uuid 用户时，入站认证必须立刻跟随，否则会出现"表里有、连不上"或
// "删了还能连"的偏斜。非 uuid 协议的变更与本插件无关，直接忽略。
//
// 协议是用户身份的一部分，同一个 key 可以同时存在于 vmess 与 vless 两行，因此这里
// 只动该协议自己的验证器：往另一侧增删都会破坏另一行用户的接入。
func (p *V2RayPlugin) SyncUser(proto store.Protocol, key, client string, status store.UserStatus, deleted bool) {
	in := p.inbound(proto)
	if in == nil {
		return
	}
	// 停用与删除都表示"这个 uuid 不该再能被认证"，对验证器而言动作相同：摘掉。
	if deleted || status != store.UserEnabled {
		in.validator.Remove(key)
		p.routes[in.proto].Delete(key)
		return
	}
	if err := in.validator.Add(key, client); err != nil {
		logging.Warn("v2ray: sync uuid user failed", "protocol", proto, "uuid", key, "err", err)
		return
	}
	p.routes[in.proto].Store(key, client)
}

// Restore 恢复持久化状态，配置或监听失败时返回错误。
func (p *V2RayPlugin) Restore(ctx context.Context) error {
	return p.runtime.Record(p.restore(ctx))
}

func (p *V2RayPlugin) restore(ctx context.Context) error {
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	if err := p.seedValidators(ctx); err != nil {
		return err
	}
	cur, ok, err := p.store.GetPlugin(ctx, pluginV2Ray)
	if err != nil || !ok {
		return err
	}
	for _, slot := range uuidSlots {
		if port := cur.Config.Ports[slot.proto]; port > 0 {
			p.inbound(slot.proto).port = port
		}
	}
	if stored := p.savedTLS(); stored != nil {
		next, err := buildTLS(*stored)
		if err != nil {
			return err
		}
		if next.settings != *stored {
			if err := p.persistTLS(ctx, next.settings); err != nil {
				return err
			}
		}
		p.tls = next
	}
	if cur.Status != store.PluginEnabled {
		return nil
	}
	return p.apply(ctx, p.ports(), p.tls, false)
}

// seedValidators 把用户表里两个 uuid 协议的路由分别灌进各自的验证器——它是唯一真源。
// 只取启用中的用户，因此停用的 uuid 不会在重启后被重新装回；同一趟写入内存路由投影，
// 之后 plugin.Status 与同步都只读内存，不再回表。
func (p *V2RayPlugin) seedValidators(ctx context.Context) error {
	if p.users == nil {
		return nil
	}
	all, err := p.users.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("v2ray: list users: %w", err)
	}
	for _, in := range p.inbounds {
		for _, u := range all {
			if u.Protocol != in.proto || !u.Protocol.UsesUUID() || u.Status != store.UserEnabled {
				continue
			}
			if err := in.validator.Add(u.Key, u.Client); err != nil {
				logging.Warn("v2ray: restore uuid failed", "protocol", in.proto, "uuid", u.Key, "err", err)
				continue
			}
			p.routes[in.proto].Store(u.Key, u.Client)
		}
	}
	return nil
}

// Enable 校验并准备监听，持久化成功后切换运行态。
func (p *V2RayPlugin) Enable(ctx context.Context, params map[string]string) error {
	ports := make(map[store.Protocol]int, len(uuidSlots))
	for _, slot := range uuidSlots {
		port, err := strconv.Atoi(params[string(slot.proto)+"_port"])
		if err != nil || port <= 0 || port > 65535 {
			return fmt.Errorf("plugin: invalid v2ray %s port", slot.proto)
		}
		ports[slot.proto] = port
	}
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	prev := config.VLESSTLS{Enabled: true}
	if saved := p.savedTLS(); saved != nil {
		prev = *saved
	}
	next, err := buildTLS(tlsParams(params, prev))
	if err != nil {
		return p.runtime.Record(err)
	}
	return p.runtime.Record(p.apply(ctx, ports, next, true))
}

// Disable 停用插件：停止两个入站并持久化状态（端口留在 Config 里，下次启用照用）。
func (p *V2RayPlugin) Disable(ctx context.Context) error {
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	if err := p.save(ctx, store.PluginDisabled, nil); err != nil {
		return err
	}
	p.stop()
	return nil
}

// SetPorts 只预建变化的监听，写库失败时保留旧端口。
func (p *V2RayPlugin) SetPorts(ctx context.Context, ports map[store.Protocol]int) error {
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	next := p.ports()
	for proto, port := range ports {
		if p.inbound(proto) == nil || port <= 0 || port > 65535 {
			return fmt.Errorf("plugin: invalid v2ray %s port %d", proto, port)
		}
		next[proto] = port
	}
	if next[store.ProtocolVMess] == next[store.ProtocolVLESS] {
		return fmt.Errorf("plugin: inbound ports must differ")
	}
	if !p.running() {
		if err := p.save(ctx, "", func(c *store.PluginConfig) { c.Ports = next }); err != nil {
			return p.runtime.Record(err)
		}
		for _, in := range p.inbounds {
			in.port = next[in.proto]
		}
		return p.runtime.Record(nil)
	}
	return p.runtime.Record(p.apply(ctx, next, p.tls, true))
}

// SetTLS 更新新连接使用的 TLS 配置。
func (p *V2RayPlugin) SetTLS(ctx context.Context, enabled bool, serverName string) error {
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	settings := config.VLESSTLS{Enabled: true}
	if saved := p.savedTLS(); saved != nil {
		settings = *saved
	}
	settings.Enabled, settings.ServerName = enabled, strings.TrimSpace(serverName)
	next, err := buildTLS(settings)
	if err != nil {
		return p.runtime.Record(err)
	}
	if err := p.persistTLS(ctx, next.settings); err != nil {
		return p.runtime.Record(err)
	}
	p.tls = next
	if in := p.inbound(store.ProtocolVLESS); in.listener != nil {
		in.listener.config.Store(next.cfg)
	}
	return p.runtime.Record(nil)
}

func (p *V2RayPlugin) apply(ctx context.Context, ports map[store.Protocol]int, next vlessTLS, persist bool) error {
	if ports[store.ProtocolVMess] == ports[store.ProtocolVLESS] {
		return fmt.Errorf("plugin: inbound ports must differ")
	}
	prepared := make(map[*uuidInbound]*tlsListener)
	defer func() {
		for _, ln := range prepared {
			_ = ln.Close()
		}
	}()
	for _, in := range p.inbounds {
		if in.running && in.port == ports[in.proto] {
			continue
		}
		var cfg *tls.Config
		if in.proto == store.ProtocolVLESS {
			cfg = next.cfg
		}
		ln, err := in.prepare(ports[in.proto], cfg)
		if err != nil {
			return err
		}
		prepared[in] = ln
	}
	if persist {
		old, exists, err := p.store.GetPlugin(ctx, pluginV2Ray)
		if err != nil {
			return err
		}
		if err := p.save(ctx, store.PluginEnabled, func(c *store.PluginConfig) { c.Ports = ports }); err != nil {
			return err
		}
		if err := p.persistTLS(ctx, next.settings); err != nil {
			if !exists {
				old = store.PluginState{Name: pluginV2Ray, Status: store.PluginDisabled, Config: store.PluginConfig{Ports: p.ports()}}
			}
			restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return errors.Join(err, p.store.SetPlugin(restoreCtx, old))
		}
	}
	p.tls = next
	for _, in := range p.inbounds {
		if ln, ok := prepared[in]; ok {
			in.stop()
			in.listener, in.port, in.running = ln, ports[in.proto], true
			run, err := p.runtime.StartInbound("plugin.v2ray."+string(in.proto), ln, in.newServer, func(err error) {
				if in.listener == ln {
					in.running = false
					p.runtime.Record(err)
				}
			})
			in.run = run
			if err != nil {
				in.running = false
				return err
			}
			delete(prepared, in)
		} else if in.proto == store.ProtocolVLESS && in.listener != nil {
			in.listener.config.Store(next.cfg)
		}
	}
	return nil
}

// Drain 终止所有入站代次并等待连接处理完成。
func (p *V2RayPlugin) Drain(ctx context.Context) error {
	runs := p.runtime.BeginDrain()
	p.stop()
	return plugin.DrainRuns(ctx, runs)
}

// Stop 停止监听，既有连接由 Drain 或对端结束。
func (p *V2RayPlugin) Stop() {
	p.stop()
}

func (p *V2RayPlugin) stop() {
	for _, in := range p.inbounds {
		in.stop()
	}
}

func (p *V2RayPlugin) running() bool {
	for _, in := range p.inbounds {
		if !in.running {
			return false
		}
	}
	return true
}

func (p *V2RayPlugin) ports() map[store.Protocol]int {
	out := make(map[store.Protocol]int, len(uuidSlots))
	for _, slot := range uuidSlots {
		out[slot.proto] = p.inbound(slot.proto).effectivePort(slot.defPort)
	}
	return out
}

func (p *V2RayPlugin) save(ctx context.Context, status store.PluginStatus, mutate func(*store.PluginConfig)) error {
	return plugin.SaveState(ctx, p.store, pluginV2Ray, status, mutate)
}

// savedTLS 返回已保存的 TLS 参数；未注入读写口或从未配置时返回 nil。
func (p *V2RayPlugin) savedTLS() *config.VLESSTLS {
	if p.tlsStore == nil {
		return nil
	}
	return p.tlsStore.VLESSTLS()
}

// persistTLS 把参数写回服务端配置表；未注入读写口时只留在内存（测试用）。
func (p *V2RayPlugin) persistTLS(ctx context.Context, s config.VLESSTLS) error {
	if p.tlsStore == nil {
		return nil
	}
	return p.tlsStore.SetVLESSTLS(ctx, s)
}

// buildTLS 解析/签发自签证书，并把当前生效的 PEM 回填进 settings 供调用方落库。
func buildTLS(s config.VLESSTLS) (vlessTLS, error) {
	if !s.Enabled {
		// 关闭时保留证书材料：再打开沿用同一张证书，指纹不跳。
		return vlessTLS{settings: s}, nil
	}
	cert, err := vless.Load(s.ServerName, []byte(s.CertPEM), []byte(s.KeyPEM))
	if err != nil {
		return vlessTLS{}, err
	}
	s.ServerName = cert.ServerName
	s.CertPEM = string(cert.CertPEM)
	s.KeyPEM = string(cert.KeyPEM)
	logging.Info("v2ray: vless tls ready",
		"server_name", cert.ServerName,
		"not_after", cert.NotAfter.Format(time.RFC3339),
		"fingerprint", cert.Fingerprint,
		"renewed", cert.Renewed)
	return vlessTLS{settings: s, cfg: cert.TLSConfig(), cert: cert}, nil
}

// tlsParams 从启用参数里取 TLS 参数：vless_tls=on|off、vless_tls_sni=<域名>；
// 缺省的键沿用 prev，管理台只传改动的字段。
func tlsParams(params map[string]string, prev config.VLESSTLS) config.VLESSTLS {
	out := prev
	if raw, ok := params["vless_tls"]; ok {
		out.Enabled = isOn(raw)
	}
	if raw, ok := params["vless_tls_sni"]; ok {
		out.ServerName = strings.TrimSpace(raw)
	}
	return out
}

// isOn 解析管理台开关：只有明确的 on/true/1/yes 才算开启，其余一律视为关闭。
func isOn(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "on", "true", "1", "yes":
		return true
	}
	return false
}

// uuidRoutes 从内存投影返回当前启用中的 uuid → 客户端 路由（本插件负责的两个协议）。
// 停用的用户不是路由，SyncUser 已经把它们的条目摘掉。
func (p *V2RayPlugin) uuidRoutes() []UUIDRoute {
	out := make([]UUIDRoute, 0)
	for _, slot := range uuidSlots {
		p.routes[slot.proto].Range(func(key, client any) bool {
			out = append(out, UUIDRoute{UUID: key.(string), Client: client.(string), Protocol: slot.proto})
			return true
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Protocol != out[j].Protocol {
			return out[i].Protocol < out[j].Protocol
		}
		return out[i].UUID < out[j].UUID
	})
	return out
}

// UUIDRoutes 返回按 (协议, uuid) 排序的路由列表（诊断与测试的观察口，读内存投影）。
func (p *V2RayPlugin) UUIDRoutes() []UUIDRoute {
	return p.uuidRoutes()
}

// AccountLimits 返回 uuid 当前的上下行限速（字节/秒；0 = 不限速）。
func (p *V2RayPlugin) AccountLimits(proto store.Protocol, account string) (up, down int64) {
	in := p.inbound(proto)
	if in == nil || p.users == nil {
		return 0, 0
	}
	u, ok := p.users.LookupUser(p.runtime.Context(), proto, account)
	if !ok {
		return 0, 0
	}
	return u.Config.UpLimit, u.Config.DownLimit
}

// deps 汇总数据面装配件；proto 决定限速与流量统计的身份命名空间。
func (p *V2RayPlugin) deps(ctx context.Context, proto store.Protocol) relayDeps {
	return relayDeps{ctx: ctx, users: p.users, rules: p.rules, exits: p.exits, limits: p.limits, traffic: p.traffic, proto: proto}
}

// serveConn 是两条入站共用的数据面入口：目标为 mux 标记域名 v1.mux.cool 的主连接
// 交给 mux 服务端，复用到多条隧道；普通请求开一条隧道直接搬运。network 是隧道数据
// 类型（tcp/udp）。两条路径的数据面装配见 tunnel.go。
func (p *V2RayPlugin) serveConn(ctx context.Context, proto store.Protocol, id identity.Identity, host string, port int, network string, conn net.Conn) {
	log := p.userLog(proto, id.Key)
	if host == vmess.MuxCoolHost {
		serveMuxConn(p.deps(ctx, proto), log, id, conn)
		return
	}
	relaySingle(p.deps(ctx, proto), log, id,
		dispatch.Destination{Host: host, Port: port, Network: network}, network, conn)
}

// userLog 返回该 uuid 的会话日志器：级别取用户配置里的 log_level（默认 info），
// 并带上 uuid 与管理台显示名——隧道日志因此不再逐处重复传 user。
func (p *V2RayPlugin) userLog(proto store.Protocol, user string) *logging.Scoped {
	level, name := "", ""
	if p.users != nil {
		if u, ok := p.users.LookupUser(p.runtime.Context(), proto, user); ok {
			level = u.Config.LogLevel
			name = u.Name
		}
	}
	attrs := []any{"user", user}
	if name != "" {
		attrs = append(attrs, "name", name)
	}
	return logging.NewScoped(level, attrs...)
}
