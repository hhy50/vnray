package plugin

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/vmess"
)

// Plugin 是一个可开关的入站插件：启停与参数持久化由实现负责，
// Restore 在进程启动时被调用以恢复已启用的插件。
type Plugin interface {
	Name() string
	// Status 返回当前状态快照（管理台展示）。
	Status() Status
	// Restore 进程启动时恢复持久化的启停状态与参数。
	Restore(ctx context.Context)
	// Enable 以给定参数启用插件（持久化并立即生效）。
	Enable(ctx context.Context, params map[string]string) error
	// Disable 停用插件（持久化并立即生效）。
	Disable(ctx context.Context) error
	// Stop 运行中插件立即停止监听（进程收尾用，不改动持久化状态）。
	Stop()
}

// PortKV 是插件的一个监听端口：协议名 + 端口号。
type PortKV struct {
	Protocol store.Protocol `json:"protocol"`
	Port     int            `json:"port"`
}

// TLSInfo 是入站 TLS 的运行态快照（管理台展示与分享链接用）。证书材料存库，这里只给
// 指纹与到期时间，私钥永不外露。
type TLSInfo struct {
	Protocol   store.Protocol `json:"protocol"`
	Enabled    bool           `json:"enabled"`
	ServerName string         `json:"server_name"`
	// SelfSigned 表示证书由服务端自签，客户端须用指纹固定代替已移除的证书校验开关。
	SelfSigned  bool   `json:"self_signed"`
	NotAfter    string `json:"not_after,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Status 是插件的运行状态快照。Ports 按插件声明的槽位顺序给出——管理台据此渲染端口列
// 与启用对话框里的输入框（字段名 `<协议>_port`）。TLS 为该插件的 TLS 能力与当前参数，
// nil 表示这个插件不支持 TLS。
type Status struct {
	Name    string            `json:"name"`
	Enabled bool              `json:"enabled"`
	Ports   []PortKV          `json:"ports,omitempty"`
	Params  map[string]string `json:"params,omitempty"`
	TLS     *TLSInfo          `json:"tls,omitempty"`
}

// Manager 持有全部插件并作为服务接入生命周期：Start 恢复已启用插件，Drain 全部停止。
// 插件表只在构造期写入、此后只读，无需加锁。
type Manager struct {
	store   store.Store
	exits   transport.ExitResolver
	limits  *ratelimit.Registry
	traffic *traffic.Meter
	plugins map[string]Plugin
	// seedEnabled 是配置文件里的插件启用种子（仅首次：库中无记录时按内置默认参数启用）；
	// 只在装配期写入、Start 读取，无需加锁。
	seedEnabled []string
}

// trafficAware 是接受流量记账器的插件能力：客户端维度的字节由各入站插件记。
type trafficAware interface {
	setTraffic(*traffic.Meter)
}

// SetTraffic 把记账器转交每个插件；须在 Start 之前调用。
func (m *Manager) SetTraffic(t *traffic.Meter) {
	m.traffic = t
	for _, p := range m.plugins {
		if tp, ok := p.(trafficAware); ok {
			tp.setTraffic(t)
		}
	}
}

// SetVLESSTLS 注入 vless TLS 参数的读写口；须在 Start 之前调用。
func (m *Manager) SetVLESSTLS(s VLESSTLSStore) {
	if p, ok := m.plugins[pluginV2Ray].(*V2RayPlugin); ok {
		p.tlsStore = s
	}
}

// UserSyncer 是接收用户表变更的插件能力（v2ray 与 ss 实现它）：用户在用户页增删或
// 启用/停用后，入站认证必须随之更新，否则会出现"表里有、连不上"或"删了还能连"的偏斜。
type UserSyncer interface {
	SyncUser(p store.Protocol, key, client string, status store.UserStatus, deleted bool)
}

var _ UserSyncer = (*V2RayPlugin)(nil)
var _ TLSSetter = (*V2RayPlugin)(nil)
var _ UserSyncer = (*SSPlugin)(nil)

// MultiPortSetter 是支持运行中热切换多端口插件的各协议端口的能力（v2ray 实现它）：
// 管理台的端口对话框经它一次改掉两个入站的端口。
type MultiPortSetter interface {
	SetPorts(ctx context.Context, ports map[store.Protocol]int) error
}

// PortSetter 是支持运行中修改单个监听端口的插件能力（ss 实现它）：管理台"修改端口"
// 对话框经它热切换；运行中的插件换端口时旧监听上的存量连接不受影响。
type PortSetter interface {
	SetPort(ctx context.Context, port int) error
}

// TLSSetter 是支持运行中切换 TLS 的插件能力（v2ray 的 vless 入站实现它）：管理台
// 「TLS」对话框经它把明文入站切成 TLS，或切回明文。
type TLSSetter interface {
	SetTLS(ctx context.Context, enabled bool, serverName string) error
}

// BasicAuthProvider 是带 Basic Auth 访问口令的插件能力（sqlite-web 实现它）：
// 管理台据此在启用对话框里渲染口令输入框。
type BasicAuthProvider interface {
	SupportsBasicAuth() bool
}

// VLESSTLSStore 是 vless TLS 参数的唯一读写口（装配层注入 serviceConfig）：参数存在
// 服务端配置表里，插件表只留端口。Get 返回 nil 表示从未配置过。
type VLESSTLSStore interface {
	VLESSTLS() *config.VLESSTLS
	SetVLESSTLS(config.VLESSTLS) error
}

// ValidateUserKey 校验用户密钥格式：vmess/vless 用户的密钥是 uuid，须为合法 UUID；
// ss 用户的密钥是连接密码，vn-v1 用户是共享密钥，都只要求非空（无用户名，密钥即身份）。
// 添加用户时调用，避免"表里有、连不上"。
func ValidateUserKey(p store.Protocol, key string) error {
	switch {
	case p.UsesUUID():
		return vmess.ValidateUUID(key)
	case p == store.ProtocolSS:
		if key == "" {
			return errors.New("plugin: ss password must not be empty")
		}
		return nil
	default:
		return nil
	}
}

// SyncUser 把一条用户变更推给全部实现了 UserSyncer 的插件（各插件自行按协议过滤），
// 装配层不必逐个点名。
func (m *Manager) SyncUser(p store.Protocol, key, client string, status store.UserStatus, deleted bool) {
	for _, pl := range m.plugins {
		if s, ok := pl.(UserSyncer); ok {
			s.SyncUser(p, key, client, status, deleted)
		}
	}
}

// NewManager 创建插件管理器：store 用于持久化插件启停状态与参数，
// users 是用户的路由与配置入口（绑定表），exits 用于把流量路由到已接入的出口
// （远程客户端会话或服务端本机出口），limits 用于按账号限速（可为 nil）。
func NewManager(st store.Store, users UserStore, exits transport.ExitResolver, limits *ratelimit.Registry) *Manager {
	m := &Manager{
		store:   st,
		exits:   exits,
		limits:  limits,
		plugins: make(map[string]Plugin),
	}
	m.register(newV2RayPlugin(st, users, exits, limits))
	m.register(newSSPlugin(st, users, exits, limits))
	m.register(newSQLiteWebPlugin(st))
	return m
}

// register 登记一个插件；同名重复登记视为装配错误。
func (m *Manager) register(p Plugin) {
	if _, ok := m.plugins[p.Name()]; ok {
		panic(fmt.Sprintf("plugin: duplicate plugin %q", p.Name()))
	}
	m.plugins[p.Name()] = p
}

// Name 返回服务名，用于启动日志与诊断。
func (m *Manager) Name() string { return "PluginManager" }

// Start 恢复持久化状态：已启用的插件立即开始监听；随后按配置种子启用尚未配置过的插件。
func (m *Manager) Start(ctx context.Context) error {
	for _, p := range m.plugins {
		p.Restore(ctx)
	}
	return m.seed(ctx)
}

// SetSeedEnabled 设置配置文件的插件启用种子；须在 Start 之前调用。
func (m *Manager) SetSeedEnabled(names []string) { m.seedEnabled = names }

// seed 按配置种子启用插件：仅当该插件在库里没有任何记录时才用内置默认参数启用，
// 已有记录（含管理台停用）一律以库为准。未知插件名视为配置错误，启动即失败。
func (m *Manager) seed(ctx context.Context) error {
	for _, name := range m.seedEnabled {
		p, ok := m.plugins[name]
		if !ok {
			return fmt.Errorf("plugin: unknown plugin %q in config", name)
		}
		if _, exists, err := m.store.GetPlugin(ctx, name); err != nil {
			return fmt.Errorf("plugin: read %q state: %w", name, err)
		} else if exists {
			continue
		}
		params := defaultEnableParams(p)
		if params == nil {
			logging.Warn("plugin: skip config seed: no default port, configure it in the admin panel",
				"plugin", name)
			continue
		}
		if err := p.Enable(ctx, params); err != nil {
			return fmt.Errorf("plugin: seed enable %q: %w", name, err)
		}
		logging.Info("plugin: enabled from config seed", "plugin", name)
	}
	return nil
}

// defaultEnableParams 取插件的内置默认启用参数（仅监听端口）：某槽位没有默认端口时
// 返回 nil（如 ss 无内置默认端口，需在管理台配置端口后再启用）。字段名与管理台对话框
// 一致：单端口用 port，多端口用 <协议>_port。
func defaultEnableParams(p Plugin) map[string]string {
	ports := p.Status().Ports
	params := make(map[string]string)
	for _, port := range ports {
		if port.Port <= 0 {
			return nil
		}
		key := string(port.Protocol) + "_port"
		if len(ports) == 1 {
			key = "port"
		}
		params[key] = strconv.Itoa(port.Port)
	}
	if len(params) == 0 {
		return nil
	}
	return params
}

// Drain 停止全部运行中插件。
func (m *Manager) Drain(context.Context) error {
	for _, p := range m.plugins {
		p.Stop()
	}
	return nil
}

// List 返回全部插件的当前状态。
func (m *Manager) List() []Status {
	out := make([]Status, 0, len(m.plugins))
	for _, p := range m.plugins {
		out = append(out, p.Status())
	}
	return out
}

// Get 按名字取插件；不存在时返回错误。
func (m *Manager) Get(name string) (Plugin, error) {
	p, ok := m.plugins[name]
	if !ok {
		return nil, fmt.Errorf("plugin: unknown plugin %q", name)
	}
	return p, nil
}
