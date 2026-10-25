package plugin

// v2ray 插件：VMess 与 VLESS 两个入站共用一个插件实例，各占一个监听端口。
// uuid 路由与其限速配置存于 vn_user_bindings 表（protocol = vmess / vless），经注入的
// UserStore 读写；插件自身的启停状态与端口留在 vn_plugin 表。

import (
	"context"
	"crypto/tls"
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
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/vless"
	"github.com/hhy50/vnray/vmess"
)

// pluginV2Ray 是 v2ray 插件在 vn_plugin 表里的 name。
const pluginV2Ray = "v2ray"

// 出厂默认端口：未配置时 Status 与启用对话框都用它们预填。
const (
	defaultVMessPort = 11120
	defaultVLESSPort = 11121
)

// uuidSlots 是 v2ray 插件声明的入站槽位，顺序固定为 vmess → vless：Status.Ports 的
// 次序、管理台启用对话框里输入框的次序都按它排。
var uuidSlots = []struct {
	proto   store.Protocol
	defPort int
}{
	{store.ProtocolVMess, defaultVMessPort},
	{store.ProtocolVLESS, defaultVLESSPort},
}

// UserStore 是用户的路由与配置入口；admin.Bindings 结构化满足它，由装配层直接注入
// （消费方定义接口，避免 plugin 反向依赖 admin）。经它写入的记录会一并进入绑定表
// 镜像并通知观察者，因此验证器与限速桶都能随之更新。
type UserStore interface {
	PutUser(u store.UserBinding) error
	DeleteUser(p store.Protocol, key string) (bool, error)
	LookupUser(p store.Protocol, key string) (store.UserBinding, bool)
	ListUsers() []store.UserBinding
}

// UUIDRoute 是一条 uuid → 客户端 的路由。
type UUIDRoute struct {
	UUID     string         `json:"uuid"`
	Client   string         `json:"client"`
	Protocol store.Protocol `json:"protocol"`
}

// V2RayPlugin 提供 VMess 与 VLESS 两个入站：认证 uuid 后按路由把流量经绑定客户端的隧道送出。
type V2RayPlugin struct {
	mu       sync.Mutex
	store    store.Store
	users    UserStore
	exits    transport.ExitResolver
	limits   *ratelimit.Registry
	traffic  *traffic.Meter
	inbounds []*uuidInbound
	tls      vlessTLS
	tlsStore VLESSTLSStore
}

// vlessTLS 是 vless 入站的 TLS 运行态：settings 是已落库的参数，cfg 为 nil 表示明文。
// 证书在 Enable/SetTLS/Restore 时签发或加载，运行期只读。
type vlessTLS struct {
	settings config.VLESSTLS
	cfg      *tls.Config
	cert     vless.Cert
}

// setTraffic 注入流量记账器（插件管理器在装配时调用）。
func (p *V2RayPlugin) setTraffic(t *traffic.Meter) { p.traffic = t }

// newV2RayPlugin 创建 v2ray 插件；两个入站的验证器在构造期即建立，用户页随时增删的
// uuid 不会丢。
func newV2RayPlugin(st store.Store, users UserStore, exits transport.ExitResolver, limits *ratelimit.Registry) *V2RayPlugin {
	p := &V2RayPlugin{store: st, users: users, exits: exits, limits: limits}
	for _, slot := range uuidSlots {
		p.inbounds = append(p.inbounds, p.newInbound(slot.proto))
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
		in.serve = func(ln net.Listener) error {
			return vmess.NewServer(v, func(id identity.Identity, target vmess.Target, network string, conn *vmess.Conn) {
				p.serveConn(proto, id, target.Host, target.Port, network, conn)
			}).Serve(ln)
		}
	case store.ProtocolVLESS:
		v := vless.NewValidator()
		in.validator = v
		in.serve = func(ln net.Listener) error {
			return vless.NewServer(v, func(id identity.Identity, target vless.Target, network string, conn net.Conn) {
				p.serveConn(proto, id, target.Host, target.Port, network, conn)
			}).Serve(ln)
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
func (p *V2RayPlugin) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	params := make(map[string]string)
	for _, r := range p.uuidRoutes() {
		params[r.UUID] = r.Client
	}
	ports := make([]PortKV, 0, len(uuidSlots))
	for _, slot := range uuidSlots {
		ports = append(ports, PortKV{
			Protocol: slot.proto,
			Port:     p.inbound(slot.proto).effectivePort(slot.defPort),
		})
	}
	return Status{
		Name: pluginV2Ray, Enabled: p.runningLocked(),
		Ports: ports, Params: params, TLS: p.tlsInfoLocked(),
	}
}

// tlsInfoLocked 返回 vless 入站的 TLS 快照。调用方必须持锁。
func (p *V2RayPlugin) tlsInfoLocked() *TLSInfo {
	info := &TLSInfo{
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
	p.mu.Lock()
	defer p.mu.Unlock()
	// 停用与删除都表示"这个 uuid 不该再能被认证"，对验证器而言动作相同：摘掉。
	if deleted || status != store.UserEnabled {
		in.validator.Remove(key)
		return
	}
	if err := in.validator.Add(key, client); err != nil {
		logging.Warn("v2ray: sync uuid user failed", "protocol", proto, "uuid", key, "err", err)
	}
}

// Restore 进程启动时从存储恢复启停状态、端口与 uuid 路由。
func (p *V2RayPlugin) Restore(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.restoreLocked(ctx)
}

func (p *V2RayPlugin) restoreLocked(ctx context.Context) {
	p.seedValidators()
	cur, ok, _ := p.store.GetPlugin(ctx, pluginV2Ray)
	if !ok {
		return
	}
	// 端口与启停分开恢复：停用态也要认上次配的端口，否则重启后管理台显示默认值，
	// 用户页的分享链接也跟着变——端口本来就在库里，没理由丢。
	for _, slot := range uuidSlots {
		if port := cur.Config.Ports[slot.proto]; port > 0 {
			p.inbound(slot.proto).port = port
		}
	}
	stored := p.savedTLS()
	if stored == nil {
		// 从未配置过 TLS：保持明文。默认开启只对"新启用"生效，否则升级会把既有
		// 明文客户端的连接全部打断。
		p.tls = vlessTLS{}
	} else if next, err := buildTLS(*stored); err != nil {
		// 证书签不出来不该让 vless 端口消失：降级明文，留一条告警等人工处理。
		logging.Warn("v2ray: vless tls unavailable, falling back to plaintext", "err", err)
	} else {
		p.tls = next
		if next.settings != *stored {
			// 续签或补齐了证书材料：回写，否则每次启动都会重签、指纹跟着跳。
			if err := p.persistTLS(next.settings); err != nil {
				logging.Warn("v2ray: persist vless tls cert failed", "err", err)
			}
		}
	}
	if cur.Status != store.PluginEnabled {
		return
	}
	if err := p.startLocked(p.portsLocked()); err != nil {
		logging.Warn("v2ray: restore listener failed", "err", err)
	}
}

// seedValidators 把用户表里两个 uuid 协议的路由分别灌进各自的验证器——它是唯一真源。
// uuidRoutes 只含启用中的用户，因此停用的 uuid 不会在重启后被重新装回。
func (p *V2RayPlugin) seedValidators() {
	for _, in := range p.inbounds {
		for _, r := range p.uuidRoutes() {
			if r.Protocol != in.proto {
				continue
			}
			if err := in.validator.Add(r.UUID, r.Client); err != nil {
				logging.Warn("v2ray: restore uuid failed", "protocol", in.proto, "uuid", r.UUID, "err", err)
			}
		}
	}
}

// Enable 启用插件：校验并持久化两个端口与 vless 的 TLS 参数，然后两个入站都开始监听。
func (p *V2RayPlugin) Enable(ctx context.Context, params map[string]string) error {
	ports := make(map[store.Protocol]int, len(uuidSlots))
	for _, slot := range uuidSlots {
		raw := params[string(slot.proto)+"_port"]
		port, err := strconv.Atoi(raw)
		if err != nil || port <= 0 || port >= 65536 {
			return fmt.Errorf("plugin: invalid v2ray %s port %q", slot.proto, raw)
		}
		ports[slot.proto] = port
	}
	// 先签发再写库：证书失败时不留脏配置。
	p.mu.Lock()
	defer p.mu.Unlock()
	// 新部署默认加密：库里从未配置过 TLS 时按"开启"起手。
	prev := config.VLESSTLS{Enabled: true}
	if saved := p.savedTLS(); saved != nil {
		prev = *saved
	}
	next, err := buildTLS(tlsParams(params, prev))
	if err != nil {
		return err
	}
	if err := p.saveLocked(ctx, store.PluginEnabled, func(c *store.PluginConfig) { c.Ports = ports }); err != nil {
		return err
	}
	if err := p.persistTLS(next.settings); err != nil {
		return err
	}
	if p.runningLocked() && p.samePortsLocked(ports) && p.tls.settings == next.settings {
		return nil
	}
	p.stopLocked()
	p.tls = next
	return p.startLocked(ports)
}

// Disable 停用插件：停止两个入站并持久化状态（端口留在 Config 里，下次启用照用）。
func (p *V2RayPlugin) Disable(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.saveLocked(ctx, store.PluginDisabled, nil); err != nil {
		return err
	}
	p.stopLocked()
	return nil
}

// SetPorts 修改两个入站端口并持久化：停用时只改待启用端口（下次启用生效）；运行中则
// 热切换——先用立即关闭的探针逐个预检新端口可绑，全通过后停旧监听、起新监听，失败
// 时旧监听原样保留。旧监听上的存量连接不随监听关闭而断开（连接各有协程），只有新
// 连接走新端口。未在 ports 里给出的协议沿用当前值。
func (p *V2RayPlugin) SetPorts(ctx context.Context, ports map[store.Protocol]int) error {
	for proto, port := range ports {
		if port <= 0 || port >= 65536 {
			return fmt.Errorf("plugin: invalid v2ray %s port %d", proto, port)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	next := p.portsLocked()
	for proto, port := range ports {
		next[proto] = port
	}
	if p.runningLocked() && p.samePortsLocked(next) {
		return nil
	}
	if !p.runningLocked() {
		if err := p.saveLocked(ctx, "", func(c *store.PluginConfig) { c.Ports = next }); err != nil {
			return err
		}
		for _, in := range p.inbounds {
			in.port = next[in.proto]
		}
		logging.Info("v2ray: ports updated", "ports", portsText(next), "running", false)
		return nil
	}
	for _, in := range p.inbounds {
		probe, err := net.Listen("tcp", fmt.Sprintf(":%d", next[in.proto]))
		if err != nil {
			return fmt.Errorf("plugin: v2ray listen :%d: %w", next[in.proto], err)
		}
		_ = probe.Close()
	}
	old := p.portsLocked()
	p.stopLocked()
	if err := p.startLocked(next); err != nil {
		// 预检通过仍可能竞态失败：回滚到旧端口，别把在用的服务弄丢。
		if rerr := p.startLocked(old); rerr != nil {
			logging.Error("v2ray: rollback to previous ports failed", "ports", portsText(old), "err", rerr)
		}
		return err
	}
	if err := p.saveLocked(ctx, store.PluginEnabled, func(c *store.PluginConfig) { c.Ports = next }); err != nil {
		return err
	}
	logging.Info("v2ray: ports updated", "ports", portsText(next), "running", true)
	return nil
}

// SetTLS 切换 vless 入站的 TLS 并热生效：只重启 vless 这一个监听（vmess 与已建立的
// 连接不受影响），证书出错时直接返回、现状不变。
func (p *V2RayPlugin) SetTLS(ctx context.Context, enabled bool, serverName string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	settings := config.VLESSTLS{Enabled: true}
	if saved := p.savedTLS(); saved != nil {
		settings = *saved
	}
	settings.Enabled = enabled
	settings.ServerName = strings.TrimSpace(serverName)
	next, err := buildTLS(settings)
	if err != nil {
		return err
	}
	if err := p.persistTLS(next.settings); err != nil {
		return err
	}
	prev := p.tls
	in := p.inbound(store.ProtocolVLESS)
	p.tls = next
	if !in.running {
		return nil
	}
	port := in.effectivePort(defaultVLESSPort)
	in.stop()
	if err := in.start(port, p.tlsConfig(store.ProtocolVLESS)); err != nil {
		p.tls = prev
		if rerr := in.start(port, p.tlsConfig(store.ProtocolVLESS)); rerr != nil {
			logging.Error("v2ray: vless listener rollback failed", "err", rerr)
		}
		return err
	}
	logging.Info("v2ray: vless tls switched", "enabled", enabled, "server_name", next.settings.ServerName)
	return nil
}

// Stop 运行中插件立即停止监听（进程收尾用，不改动持久化状态）。
func (p *V2RayPlugin) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
}

// startLocked 依次启动两个入站；任一个失败就把已起的停掉再返回错误。调用方必须持锁。
func (p *V2RayPlugin) startLocked(ports map[store.Protocol]int) error {
	started := make([]*uuidInbound, 0, len(p.inbounds))
	for _, slot := range uuidSlots {
		in := p.inbound(slot.proto)
		if err := in.start(ports[slot.proto], p.tlsConfig(slot.proto)); err != nil {
			for _, done := range started {
				done.stop()
			}
			return err
		}
		started = append(started, in)
	}
	return nil
}

// tlsConfig 返回该协议入站应使用的 TLS 配置；只有 vless 支持 TLS，其余恒为明文。
func (p *V2RayPlugin) tlsConfig(proto store.Protocol) *tls.Config {
	if proto != store.ProtocolVLESS {
		return nil
	}
	return p.tls.cfg
}

// stopLocked 停止两个入站；调用方必须持锁。
func (p *V2RayPlugin) stopLocked() {
	for _, in := range p.inbounds {
		in.stop()
	}
}

// runningLocked 报告两个入站是否都在监听；调用方必须持锁。
func (p *V2RayPlugin) runningLocked() bool {
	for _, in := range p.inbounds {
		if !in.running {
			return false
		}
	}
	return true
}

// portsLocked 返回两个入站当前的有效端口；调用方必须持锁。
func (p *V2RayPlugin) portsLocked() map[store.Protocol]int {
	out := make(map[store.Protocol]int, len(uuidSlots))
	for _, slot := range uuidSlots {
		out[slot.proto] = p.inbound(slot.proto).effectivePort(slot.defPort)
	}
	return out
}

// samePortsLocked 报告两个入站当前监听端口是否就是 next；调用方必须持锁。
func (p *V2RayPlugin) samePortsLocked(next map[store.Protocol]int) bool {
	for _, in := range p.inbounds {
		if in.port != next[in.proto] {
			return false
		}
	}
	return true
}

// saveLocked 把插件配置写进 vn_plugin：status 为空串表示沿用库中原值（只改端口或
// 证书时不要顺手把"停用"写成"启用"），mutate 就地改 Config。调用方必须持锁。
func (p *V2RayPlugin) saveLocked(ctx context.Context, status store.PluginStatus, mutate func(*store.PluginConfig)) error {
	cur, ok, _ := p.store.GetPlugin(ctx, pluginV2Ray)
	if !ok {
		cur = store.PluginState{Name: pluginV2Ray}
	}
	cur.Name = pluginV2Ray
	if status != "" {
		cur.Status = status
	}
	if mutate != nil {
		mutate(&cur.Config)
	}
	return p.store.SetPlugin(ctx, cur)
}

// savedTLS 返回已保存的 TLS 参数；未注入读写口或从未配置时返回 nil。
func (p *V2RayPlugin) savedTLS() *config.VLESSTLS {
	if p.tlsStore == nil {
		return nil
	}
	return p.tlsStore.VLESSTLS()
}

// persistTLS 把参数写回服务端配置表；未注入读写口时只留在内存（测试用）。
func (p *V2RayPlugin) persistTLS(s config.VLESSTLS) error {
	if p.tlsStore == nil {
		return nil
	}
	return p.tlsStore.SetVLESSTLS(s)
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

// portsText 把端口表压成一行可读文本（日志用）。
func portsText(ports map[store.Protocol]int) string {
	out := make([]string, 0, len(ports))
	for _, slot := range uuidSlots {
		out = append(out, fmt.Sprintf("%s=%d", slot.proto, ports[slot.proto]))
	}
	return strings.Join(out, " ")
}

// uuidRoutes 返回当前全部启用中的 uuid → 客户端 路由（本插件负责的两个协议）。
// 停用的用户不是路由，这里就不列出——它同时决定了重启后验证器的灌入范围。
func (p *V2RayPlugin) uuidRoutes() []UUIDRoute {
	all := p.users.ListUsers()
	out := make([]UUIDRoute, 0, len(all))
	for _, u := range all {
		if !u.Protocol.UsesUUID() || u.Status != store.UserEnabled {
			continue
		}
		out = append(out, UUIDRoute{UUID: u.Key, Client: u.Client, Protocol: u.Protocol})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Protocol != out[j].Protocol {
			return out[i].Protocol < out[j].Protocol
		}
		return out[i].UUID < out[j].UUID
	})
	return out
}

// UUIDRoutes 返回按 (协议, uuid) 排序的路由列表（管理台展示用）。
func (p *V2RayPlugin) UUIDRoutes() []UUIDRoute { return p.uuidRoutes() }

// AccountLimits 返回 uuid 当前的上下行限速（字节/秒；0 = 不限速）。
func (p *V2RayPlugin) AccountLimits(proto store.Protocol, account string) (up, down int64) {
	in := p.inbound(proto)
	if in == nil || p.users == nil {
		return 0, 0
	}
	u, ok := p.users.LookupUser(proto, account)
	if !ok {
		return 0, 0
	}
	return u.Config.UpLimit, u.Config.DownLimit
}

// deps 汇总数据面装配件；proto 决定限速与流量统计的身份命名空间。
func (p *V2RayPlugin) deps(proto store.Protocol) relayDeps {
	return relayDeps{users: p.users, exits: p.exits, limits: p.limits, traffic: p.traffic, proto: proto}
}

// serveConn 是两条入站共用的数据面入口：目标为 mux 标记域名 v1.mux.cool 的主连接
// 交给 mux 服务端，复用到多条隧道；普通请求开一条隧道直接搬运。network 是隧道数据
// 类型（tcp/udp）。两条路径的数据面装配见 tunnel.go。
func (p *V2RayPlugin) serveConn(proto store.Protocol, id identity.Identity, host string, port int, network string, conn net.Conn) {
	log := p.userLog(proto, id.Key)
	if host == vmess.MuxCoolHost {
		serveMuxConn(p.deps(proto), log, id, conn)
		return
	}
	relaySingle(p.deps(proto), log, id,
		dispatch.Destination{Host: host, Port: port, Network: network}, network, conn)
}

// userLog 返回该 uuid 的会话日志器：级别取用户配置里的 log_level（默认 info），
// 并带上 uuid 与管理台显示名——隧道日志因此不再逐处重复传 user。
func (p *V2RayPlugin) userLog(proto store.Protocol, user string) *logging.Scoped {
	level, name := "", ""
	if p.users != nil {
		if u, ok := p.users.LookupUser(proto, user); ok {
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
