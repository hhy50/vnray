package store

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hhy50/vnray/internal/protocol"
)

// Protocol 是用户的接入协议；与 Key 一起构成用户身份，同名 Key 在不同协议下是不同用户
// （同一个 uuid 可以同时存在于 vmess 与 vless 两行）。
type Protocol string

const (
	// ProtocolVMess 是 VMess 用户：以 uuid 作凭据，进 v2ray 插件的 vmess 入站。
	ProtocolVMess Protocol = "vmess"
	// ProtocolVLESS 是 VLESS 用户：以 uuid 作凭据，进 v2ray 插件的 vless 入站。
	ProtocolVLESS Protocol = "vless"
	// ProtocolVNV1 是自有协议用户：以 UserAuth 帧出示共享密钥接入。
	ProtocolVNV1 Protocol = "vn-v1"
	// ProtocolSS 是 shadowsocks 用户：以连接密码作凭据。
	ProtocolSS Protocol = "ss"
)

// Valid 报告 Protocol 是否为已知协议。
func (p Protocol) Valid() bool {
	switch p {
	case ProtocolVMess, ProtocolVLESS, ProtocolVNV1, ProtocolSS:
		return true
	default:
		return false
	}
}

// UsesUUID 报告该协议的凭据是否为 UUID（vmess 与 vless 是，其余是共享密钥或密码）。
func (p Protocol) UsesUUID() bool { return p == ProtocolVMess || p == ProtocolVLESS }

// UserStatus 是用户的启停状态：1 = 启用，0 = 停用；停用的用户在认证阶段即被拒绝。
// 零值即停用，因此新增用户时调用方必须显式给出（管理台表单默认选启用）。
type UserStatus int

const (
	// UserDisabled 表示用户已停用：四个入站都不再接受它的凭据。
	UserDisabled UserStatus = 0
	// UserEnabled 表示用户已启用。
	UserEnabled UserStatus = 1
)

// Valid 报告 UserStatus 是否为已知取值。
func (s UserStatus) Valid() bool { return s == UserDisabled || s == UserEnabled }

// ClientType 是客户端种类；当前只有一种自有代理客户端，字段留作扩展。
type ClientType string

// ClientTypeProxy 是自有协议的代理客户端。
const ClientTypeProxy ClientType = "proxy"

// ClientTypeLocal 是服务端本机出口（server_proxy）登记的本机客户端。
const ClientTypeLocal ClientType = "local"

// UserConfig 是用户记录 Config 列的 JSON 结构：上下行限速（字节/秒，0 = 不限）、
// 该用户会话的日志级别（空视为 info）、最大并发隧道数（0 = 不限）与出站规则名。
type UserConfig struct {
	// SSMethod 为空时沿用 SS 插件的默认加密方法。
	SSMethod  string `json:"ss_method,omitempty"`
	UpLimit   int64  `json:"up_limit,omitempty"`
	DownLimit int64  `json:"down_limit,omitempty"`
	LogLevel  string `json:"log_level,omitempty"`
	// Concurrency 是该用户同时存活的转发隧道上限；0 = 不限。
	Concurrency int `json:"concurrency,omitempty"`
	// Rule 是该用户使用的出站规则名（服务端 rule.<name>）；空视为预置的 default。
	Rule string `json:"rule,omitempty"`
}

// ParseUserConfig 解析用户配置 JSON；空串或非法 JSON 一律返回零值（视为不限速）。
func ParseUserConfig(raw string) UserConfig {
	var c UserConfig
	if raw == "" {
		return c
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return UserConfig{}
	}
	return c
}

// Encode 序列化用户配置；全零返回空串，避免库里堆积空对象。
func (c UserConfig) Encode() string {
	if c.UpLimit == 0 && c.DownLimit == 0 && c.LogLevel == "" && c.Concurrency == 0 &&
		c.SSMethod == "" && c.Rule == "" {
		return ""
	}
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

// Value 实现 driver.Valuer：把配置写进 text 列。
func (c UserConfig) Value() (driver.Value, error) { return c.Encode(), nil }

// Scan 实现 sql.Scanner：把 text 列读回配置；空串或非法 JSON 一律落成零值。
func (c *UserConfig) Scan(value any) error {
	*c = UserConfig{}
	b, err := configColumn(value)
	if err != nil || len(b) == 0 {
		return err
	}
	if err := json.Unmarshal(b, c); err != nil {
		*c = UserConfig{}
	}
	return nil
}

// ClientConfig 是客户端记录 Config 列里服务端认识的字段；其余键由运维自由书写，
// 这里只取该客户端会话的日志级别、自定义 host 表、IP 段映射与并发上限。
type ClientConfig struct {
	LogLevel string `json:"log_level,omitempty"`
	// CustomHosts 是服务端下发的自定义域名映射，随认证应答下发给客户端。
	CustomHosts []protocol.CustomHost `json:"custom_hosts,omitempty"`
	// IPMaps 是服务端下发的 IPv4 段映射，随认证应答下发给客户端。
	IPMaps []protocol.IPMap `json:"ip_maps,omitempty"`
	// Concurrency 是同时路由到这个客户端的隧道流上限；0 = 不限。仅服务端生效，
	// 不出现在 PushConfig 里。
	Concurrency int `json:"concurrency,omitempty"`
}

// ParseClientConfig 解析客户端配置 JSON；空串或非法 JSON 返回零值（日志级别取默认）。
func ParseClientConfig(raw string) ClientConfig {
	var c ClientConfig
	if raw == "" {
		return c
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return ClientConfig{}
	}
	return c
}

// AuthKey 是一条服务端接入密钥：Key 是客户端握手指纹里的共享密钥（唯一，覆盖即改值），
// Scope 是范围 key，约束哪些客户端名可用此密钥——"*" 全部放行、"ns:*" 放行某 namespace、
// 其余为精确客户端名（namespace:group:name）。
type AuthKey struct {
	ID        uint64
	Key       string
	Scope     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ValidateAuthScope 校验范围 key 的三种合法形态："*"、"ns:*"（namespace 单段、
// 不含通配符）或不含 "*" 的精确客户端名。空串非法。
func ValidateAuthScope(scope string) error {
	switch {
	case scope == "*":
		return nil
	case strings.HasSuffix(scope, ":*"):
		ns := strings.TrimSuffix(scope, ":*")
		if ns == "" || strings.ContainsAny(ns, "*:") {
			return fmt.Errorf("store: auth scope %q: namespace part must be a single segment without wildcards", scope)
		}
		return nil
	default:
		if scope == "" || strings.Contains(scope, "*") {
			return fmt.Errorf(`store: auth scope must be "*", "namespace:*" or an exact client name, got %q`, scope)
		}
		return nil
	}
}

// ScopeMatches 报告范围 key 是否覆盖一个客户端名："*" 全覆盖；"ns:*" 比对
// 客户端名的 namespace 段；其余按精确名比对。名字不含 ':' 时 namespace 为空，
// 只能被 "*" 或同名精确范围命中。
func ScopeMatches(scope, name string) bool {
	switch {
	case scope == "*":
		return true
	case strings.HasSuffix(scope, ":*"):
		ns, _, found := strings.Cut(name, ":")
		if !found {
			return false
		}
		return ns+":*" == scope
	default:
		return scope == name
	}
}

// validateUser 校验用户记录的必要字段。
func validateUser(b UserBinding) error {
	if b.Key == "" {
		return fmt.Errorf("store: user binding requires key")
	}
	if !b.Protocol.Valid() {
		return fmt.Errorf("store: user binding requires a known protocol, got %q", b.Protocol)
	}
	if b.Name == "" {
		return fmt.Errorf("store: user binding requires name")
	}
	if !b.Status.Valid() {
		return fmt.Errorf("store: user binding requires status 0 or 1, got %d", int(b.Status))
	}
	if b.Client == "" {
		return fmt.Errorf("store: user binding requires client")
	}
	return nil
}

// validateClient 校验客户端记录的必要字段。
func validateClient(b ClientBinding) error {
	if b.Key == "" {
		return fmt.Errorf("store: client binding requires key")
	}
	if b.Type == "" {
		return fmt.Errorf("store: client binding requires type")
	}
	return nil
}

// UserTraffic 是一条用户的累计流量：Up/Down 是历次转发字节的总和，只增不减。
type UserTraffic struct {
	Protocol  Protocol
	Key       string
	Up        int64
	Down      int64
	UpdatedAt time.Time
}

// ClientTraffic 是一条客户端的累计流量，语义同 UserTraffic。
type ClientTraffic struct {
	Key       string
	Up        int64
	Down      int64
	UpdatedAt time.Time
}

// PortConfig 是端口映射记录 Config 列的 JSON 结构；限速键与 UserConfig 一致，
// 便于运维按同一套口径理解与脚本读写。
type PortConfig struct {
	UpLimit   int64  `json:"up_limit,omitempty"`
	DownLimit int64  `json:"down_limit,omitempty"`
	CertPEM   string `json:"cert_pem,omitempty"`
	KeyPEM    string `json:"key_pem,omitempty"`
}

// ParsePortConfig 解析端口映射配置 JSON；空串或非法 JSON 返回零值（视为不限速）。
func ParsePortConfig(raw string) PortConfig {
	var c PortConfig
	if raw == "" {
		return c
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return PortConfig{}
	}
	return c
}

// Encode 序列化端口映射配置；全零返回空串，避免库里堆积空对象。
func (c PortConfig) Encode() string {
	if c.UpLimit == 0 && c.DownLimit == 0 && c.CertPEM == "" && c.KeyPEM == "" {
		return ""
	}
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

// Value 实现 driver.Valuer：把配置写进 text 列。
func (c PortConfig) Value() (driver.Value, error) { return c.Encode(), nil }

// Scan 实现 sql.Scanner：把 text 列读回配置；空串或非法 JSON 一律落成零值。
func (c *PortConfig) Scan(value any) error {
	*c = PortConfig{}
	b, err := configColumn(value)
	if err != nil || len(b) == 0 {
		return err
	}
	if err := json.Unmarshal(b, c); err != nil {
		*c = PortConfig{}
	}
	return nil
}

// configColumn 把驱动交回的列值（nil / []byte / string）规整成字节。
func configColumn(value any) ([]byte, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	default:
		return nil, fmt.Errorf("store: cannot scan config column of type %T", value)
	}
}

// validatePortMapping 校验映射规则的必要字段与取值范围。
func validatePortMapping(m PortMapping) error {
	if m.Client == "" {
		return fmt.Errorf("store: port mapping requires client")
	}
	if len(m.Client) > 255 {
		return fmt.Errorf("store: port mapping client name too long (max 255)")
	}
	if m.RemotePort < 1 || m.RemotePort > 65535 {
		return fmt.Errorf("store: port mapping remote port must be in 1..65535, got %d", m.RemotePort)
	}
	if m.LocalHost == "" {
		return fmt.Errorf("store: port mapping requires local host")
	}
	if len(m.LocalHost) > 255 {
		return fmt.Errorf("store: port mapping local host too long (max 255)")
	}
	if m.LocalPort < 1 || m.LocalPort > 65535 {
		return fmt.Errorf("store: port mapping local port must be in 1..65535, got %d", m.LocalPort)
	}
	if m.Config.UpLimit < 0 || m.Config.DownLimit < 0 {
		return fmt.Errorf("store: port mapping limits must be non-negative")
	}
	if (m.Config.CertPEM == "") != (m.Config.KeyPEM == "") {
		return fmt.Errorf("store: port mapping TLS certificate and key must be provided together")
	}
	return nil
}

// Store 是用户绑定与客户端注册两张表的持久化契约；实现必须并发安全，Close 可重复调用。
type Store interface {
	// SetUser 新增或按 (Protocol, Key) 覆盖一条用户记录；覆盖时保留 ID 与 CreatedAt。
	SetUser(ctx context.Context, b UserBinding) error
	// GetUser 按 (Protocol, Key) 读取一条用户记录；不存在时 ok 为 false。
	GetUser(ctx context.Context, p Protocol, key string) (UserBinding, bool, error)
	// DeleteUser 删除一条用户记录；不存在时返回 false。
	DeleteUser(ctx context.Context, p Protocol, key string) (bool, error)
	// ListUsers 返回全部用户记录，按 (Protocol, Key) 升序。
	ListUsers(ctx context.Context) ([]UserBinding, error)

	// SetClient 新增或按 Key 覆盖一条客户端记录；覆盖时保留 ID 与 CreatedAt。
	SetClient(ctx context.Context, b ClientBinding) error
	// GetClient 按 Key 读取一条客户端记录；不存在时 ok 为 false。
	GetClient(ctx context.Context, key string) (ClientBinding, bool, error)
	// DeleteClient 删除一条客户端记录；不存在时返回 false。
	DeleteClient(ctx context.Context, key string) (bool, error)
	// ListClients 返回全部客户端记录，按 Key 升序。
	ListClients(ctx context.Context) ([]ClientBinding, error)

	// SetAuthKey 新增或按 Key 覆盖一条接入密钥；覆盖时保留 ID 与 CreatedAt。
	SetAuthKey(ctx context.Context, k AuthKey) error
	// GetAuthKey 按 Key 读取一条接入密钥；不存在时 ok 为 false。
	GetAuthKey(ctx context.Context, key string) (AuthKey, bool, error)
	// DeleteAuthKey 删除一条接入密钥；不存在时返回 false。
	DeleteAuthKey(ctx context.Context, key string) (bool, error)
	// ListAuthKeys 返回全部接入密钥，按 Key 升序。
	ListAuthKeys(ctx context.Context) ([]AuthKey, error)

	// GetPlugin 读取一个插件的持久状态；不存在时 ok 为 false。
	GetPlugin(ctx context.Context, name string) (PluginState, bool, error)
	// SetPlugin 新增或按 Name 覆盖一个插件的状态。
	SetPlugin(ctx context.Context, s PluginState) error
	// ListPlugins 返回全部插件的状态，按 Name 升序。
	ListPlugins(ctx context.Context) ([]PluginState, error)

	// GetServiceSettings 读取全部运行时配置段：段名 → 原文（标量存原文，复合段存 JSON），
	// 含 ServiceSettingsInitializedKey 这一行初始化标记。
	GetServiceSettings(ctx context.Context) (map[string]string, error)
	// SetServiceSettings 按段写入运行时配置：只动给定段，其它段保持原值；空值删除该段。
	SetServiceSettings(ctx context.Context, values map[string]string) error

	// AddUserTraffic 把一次增量累加进用户流量表（无则新建）；两个增量都为 0 时不写库。
	AddUserTraffic(ctx context.Context, p Protocol, key string, up, down int64) error
	// ListUserTraffic 返回全部用户流量记录。
	ListUserTraffic(ctx context.Context) ([]UserTraffic, error)
	// DeleteUserTraffic 删除一条用户流量记录；不存在时返回 false。
	DeleteUserTraffic(ctx context.Context, p Protocol, key string) (bool, error)

	// AddClientTraffic 把一次增量累加进客户端流量表（无则新建）。
	AddClientTraffic(ctx context.Context, key string, up, down int64) error
	// ListClientTraffic 返回全部客户端流量记录。
	ListClientTraffic(ctx context.Context) ([]ClientTraffic, error)
	// DeleteClientTraffic 删除一条客户端流量记录；不存在时返回 false。
	DeleteClientTraffic(ctx context.Context, key string) (bool, error)

	// SetPortMapping 新增或按 ID 覆盖一条端口映射规则；覆盖时保留 CreatedAt。
	// remote_port 全表唯一：与本条之外的其他行冲突时报错，由调用方提示运维。
	SetPortMapping(ctx context.Context, m PortMapping) error
	// GetPortMapping 按 ID 读取一条端口映射规则；不存在时 ok 为 false。
	GetPortMapping(ctx context.Context, id uint64) (PortMapping, bool, error)
	// DeletePortMapping 删除一条端口映射规则；不存在时返回 false。
	DeletePortMapping(ctx context.Context, id uint64) (bool, error)
	// ListPortMappings 返回全部端口映射规则，按 remote_port 升序。
	ListPortMappings(ctx context.Context) ([]PortMapping, error)

	// Close 释放底层资源。
	Close() error
}
