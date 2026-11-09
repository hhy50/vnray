package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/traffic"
)

// Plugin 是一个可开关的入站插件：启停与参数持久化由实现负责，
// Restore 在进程启动时被调用以恢复已启用的插件。
type Plugin interface {
	Name() string
	// Status 返回当前状态快照（管理台展示）。
	Status() Status
	// Restore 进程启动时恢复持久化的启停状态与参数。
	Restore(ctx context.Context) error
	// Enable 以给定参数启用插件（持久化并立即生效）。
	Enable(ctx context.Context, params map[string]string) error
	// Disable 停用插件（持久化并立即生效）。
	Disable(ctx context.Context) error
	// Stop 停止监听，保留存量连接与持久化状态。
	Stop()
	// Drain 终止存量工作并等待全部运行代次退出。
	Drain(ctx context.Context) error
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
	AcceptsUsers bool              `json:"accepts_users,omitempty"`
	Name         string            `json:"name"`
	Enabled      bool              `json:"enabled"`
	Ports        []PortKV          `json:"ports,omitempty"`
	Params       map[string]string `json:"params,omitempty"`
	TLS          *TLSInfo          `json:"tls,omitempty"`
	LastError    string            `json:"last_error,omitempty"`
}

// Manager 持有全部插件并作为服务接入生命周期：Start 恢复已启用插件，Drain 全部停止。
// 插件表只在构造期写入、此后只读，无需加锁。
type Manager struct {
	plugins map[string]Plugin
	gate    *DrainGate
	cancel  context.CancelFunc
}

// TrafficConsumer 接收入站流量记账器。
type TrafficConsumer interface {
	SetTraffic(*traffic.Meter)
}

// VLESSTLSConsumer 接收 TLS 参数的持久化依赖。
type VLESSTLSConsumer interface {
	SetVLESSTLSStore(VLESSTLSStore)
}

// SessionConfigConsumer 接收新会话参数的运行时更新。
type SessionConfigConsumer interface {
	ApplySessionConfig(*tcp.SessionConfig)
}

// DrainGateBinder 在注册时接入管理器的共享收尾状态门。
type DrainGateBinder interface {
	BindDrainGate(*DrainGate)
}

// SetTraffic 把记账器转交每个插件；须在 Start 之前调用。
func (m *Manager) SetTraffic(t *traffic.Meter) {
	for _, p := range m.plugins {
		if tp, ok := p.(TrafficConsumer); ok {
			tp.SetTraffic(t)
		}
	}
}

// SetVLESSTLS 注入 vless TLS 参数的读写口；须在 Start 之前调用。
func (m *Manager) SetVLESSTLS(s VLESSTLSStore) {
	for _, p := range m.plugins {
		if consumer, ok := p.(VLESSTLSConsumer); ok {
			consumer.SetVLESSTLSStore(s)
		}
	}
}

// ApplySessionConfig 更新用户入站的新会话参数（运行时配置热分发的一个落点）。
func (m *Manager) ApplySessionConfig(cfg *tcp.SessionConfig) {
	for _, p := range m.plugins {
		if consumer, ok := p.(SessionConfigConsumer); ok {
			consumer.ApplySessionConfig(cfg)
		}
	}
}

// UserSyncer 是接收用户表变更的插件能力（v2ray 与 ss 实现它）：用户在用户页增删或
// 启用/停用后，入站认证必须随之更新，否则会出现"表里有、连不上"或"删了还能连"的偏斜。
type UserSyncer interface {
	SyncUser(p store.Protocol, key, client string, status store.UserStatus, deleted bool)
}

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
	SetVLESSTLS(context.Context, config.VLESSTLS) error
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

// NewRegistry 创建已由装配层构造的插件注册表。
func NewRegistry(ctx context.Context, plugins ...Plugin) *Manager {
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{plugins: make(map[string]Plugin), gate: &DrainGate{ctx: ctx}, cancel: cancel}
	for _, p := range plugins {
		m.register(p)
	}
	return m
}

// register 登记一个插件；同名重复登记视为装配错误。
func (m *Manager) register(p Plugin) {
	if _, ok := m.plugins[p.Name()]; ok {
		panic(fmt.Sprintf("plugin: duplicate plugin %q", p.Name()))
	}
	if aware, ok := p.(DrainGateBinder); ok {
		aware.BindDrainGate(m.gate)
	}
	m.plugins[p.Name()] = p
}

// Name 返回服务名，用于启动日志与诊断。
func (m *Manager) Name() string { return "PluginManager" }

// Start 恢复数据库中已启用的插件。
func (m *Manager) Start(ctx context.Context) error {
	if m.gate.IsDraining() {
		return ErrDraining
	}
	for _, name := range m.names() {
		if err := m.plugins[name].Restore(ctx); err != nil {
			return fmt.Errorf("restore plugin %s: %w", name, err)
		}
	}
	return nil
}

// Drain 停止全部运行中插件。
func (m *Manager) Drain(ctx context.Context) error {
	m.gate.begin()
	m.cancel()
	var errs []error
	for _, name := range m.names() {
		if err := m.plugins[name].Drain(ctx); err != nil {
			errs = append(errs, fmt.Errorf("drain plugin %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) names() []string {
	names := make([]string, 0, len(m.plugins))
	for name := range m.plugins {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// List 按插件名称升序返回全部插件的当前状态。
func (m *Manager) List() []Status {
	out := make([]Status, 0, len(m.plugins))
	for _, p := range m.plugins {
		out = append(out, p.Status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get 按名字取插件；不存在时返回错误。
func (m *Manager) Get(name string) (Plugin, error) {
	if m.gate.IsDraining() {
		return nil, ErrDraining
	}
	p, ok := m.plugins[name]
	if !ok {
		return nil, fmt.Errorf("plugin: unknown plugin %q", name)
	}
	return p, nil
}
