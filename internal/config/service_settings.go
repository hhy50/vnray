package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ServiceSettings 是可在管理台在线调整的服务端运行时配置。管理台保存的快照（DB 单行
// JSON）覆盖配置文件与内置默认值，改动即时热生效。
//
// 这里只放**运行期可热改**的参数；监听地址、管理端口、存储等需重启的配置不在其中。
type ServiceSettings struct {
	GlobalClient GlobalClientSettings `json:"global_client"`
	// PublicHost 同 Server.PublicHost：vmess:// 分享链接对外暴露的裸主机。
	PublicHost string    `json:"public_host"`
	Heartbeat  Heartbeat `json:"heartbeat"`
	// LogLevel 是全局日志级别；会话级（用户/客户端）日志级别另有其页，互不影响。
	LogLevel string `json:"log_level"`
	// TrafficFlushInterval 是流量增量落库的周期。
	TrafficFlushInterval Duration        `json:"traffic_flush_interval"`
	Session              SessionSettings `json:"session"`
	LogRotation          LogRotation     `json:"log_rotation"`
	// ServerProxy 是服务端本机出口的运行期配置（原先在配置文件里，现由管理台管理）。
	ServerProxy ServerProxySettings `json:"server_proxy"`
	// VLESS 是 vless 入站的私有参数：证书材料随服务端配置存库，不占插件表。
	VLESS VLESSSettings `json:"vless"`
}

// VLESSSettings 是 vless 入站的私有参数。
type VLESSSettings struct {
	TLS *VLESSTLS `json:"tls,omitempty"`
}

// VLESSTLS 是 vless 入站的 TLS 参数。nil 表示从未配置过（与 Enabled=false 的
// "显式关闭"不是一回事），因此证书材料在关闭后仍会保留。
type VLESSTLS struct {
	Enabled bool `json:"enabled"`
	// ServerName 是自签证书覆盖的域名（客户端 SNI 用它）；空取内置默认名。
	ServerName string `json:"server_name,omitempty"`
	// CertPEM/KeyPEM 是自签证书与私钥的 PEM 文本。
	CertPEM string `json:"cert_pem,omitempty"`
	KeyPEM  string `json:"key_pem,omitempty"`
}

// ServerProxySettings 是服务端本机出口（保留名 server:self）的运行期配置：
// 在进程内用 dispatcher 直接出站，不起回环客户端。
type ServerProxySettings struct {
	Enabled bool `json:"enabled"`
	// Public/Private 是出站目标网段白名单/黑名单，语义与客户端配置一致；空 = 不限制。
	Public  string `json:"public"`
	Private string `json:"private"`
	// Routing 是与客户端同构的出站路由（default + outbounds + rules）。
	Routing Routing `json:"routing"`
}

// Validate 校验本机出口配置；网段与路由按与客户端同一口径校验。
func (p ServerProxySettings) Validate() error {
	if _, err := ParseNetworkPatterns(p.Public); err != nil {
		return fmt.Errorf("server_proxy.public: %w", err)
	}
	if _, err := ParseNetworkPatterns(p.Private); err != nil {
		return fmt.Errorf("server_proxy.private: %w", err)
	}
	return p.Routing.Validate()
}

// SessionSettings 是多路复用会话参数，映射到 tcp.SessionConfig。**一条链路两端必须一致**，
// 只改服务端可能与客户端不对称；改动仅对之后建立的新连接生效。
type SessionSettings struct {
	// KeepAliveInterval 为 0 表示关闭 keepalive（不推荐：半开连接无法被发现）。
	KeepAliveInterval Duration `json:"keepalive_interval"`
	KeepAliveTimeout  Duration `json:"keepalive_timeout"`
	// MaxStreamWindow 是单流流控窗口（字节）；0 表示用后端默认，非 0 时不得小于 256KiB。
	MaxStreamWindow   int      `json:"max_stream_window"`
	AcceptBacklog     int      `json:"accept_backlog"`
	StreamOpenTimeout Duration `json:"stream_open_timeout"`
}

// LogRotation 是日志文件轮转参数，仅当日志输出到文件时生效。
type LogRotation struct {
	MaxSizeMB  int `json:"max_size_mb"`
	MaxBackups int `json:"max_backups"`
	MaxAgeDays int `json:"max_age_days"`
}

const (
	// DefaultTrafficFlushInterval 是流量增量落库的默认周期，与 traffic.FlushInterval 一致。
	DefaultTrafficFlushInterval = 10 * time.Second
	// DefaultLogRotateMaxSizeMB/DefaultLogRotateMaxBackups/DefaultLogRotateMaxAgeDays
	// 是日志文件轮转的默认参数。
	DefaultLogRotateMaxSizeMB  = 10
	DefaultLogRotateMaxBackups = 3
	DefaultLogRotateMaxAgeDays = 28
	// minStreamWindow 是 yamux 对单流窗口的硬性下限。
	minStreamWindow = 256 * 1024
)

// DefaultSessionSettings 返回内置会话参数，取值须与 tcp.DefaultSessionConfig 保持一致
// （由测试断言）。
func DefaultSessionSettings() SessionSettings {
	return SessionSettings{
		KeepAliveInterval: Duration(10 * time.Second),
		KeepAliveTimeout:  Duration(30 * time.Second),
		MaxStreamWindow:   minStreamWindow,
		AcceptBacklog:     256,
		StreamOpenTimeout: Duration(75 * time.Second),
	}
}

// DefaultServiceSettings 返回内置运行时配置；心跳与日志级别取各自默认值。
func DefaultServiceSettings() ServiceSettings {
	return ServiceSettings{
		Heartbeat:            DefaultHeartbeat(),
		LogLevel:             DefaultLogLevel,
		TrafficFlushInterval: Duration(DefaultTrafficFlushInterval),
		Session:              DefaultSessionSettings(),
		LogRotation: LogRotation{
			MaxSizeMB:  DefaultLogRotateMaxSizeMB,
			MaxBackups: DefaultLogRotateMaxBackups,
			MaxAgeDays: DefaultLogRotateMaxAgeDays,
		},
		ServerProxy: ServerProxySettings{Routing: DefaultServerProxyRouting()},
	}
}

// Validate 校验全部运行时配置项。
func (s ServiceSettings) Validate() error {
	if err := s.GlobalClient.Validate(); err != nil {
		return err
	}
	if err := s.Heartbeat.Validate(); err != nil {
		return err
	}
	if err := validatePublicHostValue(s.PublicHost); err != nil {
		return err
	}
	if !validLevels[s.LogLevel] {
		return fmt.Errorf("log_level: %q is invalid, expect one of debug/info/warn/error", s.LogLevel)
	}
	if s.TrafficFlushInterval.Std() <= 0 {
		return fmt.Errorf("traffic_flush_interval: must be > 0, got %q", s.TrafficFlushInterval)
	}
	if err := s.Session.Validate(); err != nil {
		return err
	}
	if err := s.LogRotation.Validate(); err != nil {
		return err
	}
	return s.ServerProxy.Validate()
}

// Validate 校验会话参数；0 一律表示"用后端默认"，非 0 才做范围检查。
func (s SessionSettings) Validate() error {
	if s.KeepAliveInterval < 0 {
		return fmt.Errorf("session.keepalive_interval: must be >= 0, got %q", s.KeepAliveInterval)
	}
	if s.KeepAliveTimeout < 0 {
		return fmt.Errorf("session.keepalive_timeout: must be >= 0, got %q", s.KeepAliveTimeout)
	}
	if s.MaxStreamWindow != 0 && s.MaxStreamWindow < minStreamWindow {
		return fmt.Errorf("session.max_stream_window: must be 0 (default) or >= %d bytes, got %d", minStreamWindow, s.MaxStreamWindow)
	}
	if s.AcceptBacklog < 0 {
		return fmt.Errorf("session.accept_backlog: must be >= 0, got %d", s.AcceptBacklog)
	}
	if s.StreamOpenTimeout < 0 {
		return fmt.Errorf("session.stream_open_timeout: must be >= 0, got %q", s.StreamOpenTimeout)
	}
	return nil
}

// Validate 校验日志轮转参数；仅当输出到文件时才真正生效。
func (l LogRotation) Validate() error {
	if l.MaxSizeMB <= 0 {
		return fmt.Errorf("log_rotation.max_size_mb: must be > 0, got %d", l.MaxSizeMB)
	}
	if l.MaxBackups < 0 {
		return fmt.Errorf("log_rotation.max_backups: must be >= 0, got %d", l.MaxBackups)
	}
	if l.MaxAgeDays < 0 {
		return fmt.Errorf("log_rotation.max_age_days: must be >= 0, got %d", l.MaxAgeDays)
	}
	return nil
}

// 运行时配置在 vn_service_settings 里的段名（setting_key）：标量段存原文，复合段存 JSON。
const (
	// SettingGlobalClient 保存全局客户端的 Host 与 IP 段映射。
	SettingGlobalClient = "global_client"
	SettingPublicHost   = "public_host"
	SettingHeartbeat    = "heartbeat"
	SettingLogLevel     = "log_level"
	SettingTrafficFlush = "traffic_flush_interval"
	SettingSession      = "session"
	SettingLogRotation  = "log_rotation"
	SettingServerProxy  = "server_proxy"
	SettingVLESS        = "vless"
)

// EncodeServiceSettings 把整份运行时配置摊成按段的 KV。
func EncodeServiceSettings(s ServiceSettings) (map[string]string, error) {
	out := map[string]string{
		SettingPublicHost:   s.PublicHost,
		SettingLogLevel:     s.LogLevel,
		SettingTrafficFlush: s.TrafficFlushInterval.String(),
	}
	sections := map[string]any{
		SettingGlobalClient: s.GlobalClient,
		SettingHeartbeat:    s.Heartbeat,
		SettingSession:      s.Session,
		SettingLogRotation:  s.LogRotation,
		SettingServerProxy:  s.ServerProxy,
		SettingVLESS:        s.VLESS,
	}
	for key, section := range sections {
		b, err := json.Marshal(section)
		if err != nil {
			return nil, fmt.Errorf("encode service setting %s: %w", key, err)
		}
		out[key] = string(b)
	}
	return out, nil
}

// DecodeServiceSettings 把按段存放的 KV 覆盖到 base 上：只覆盖出现过的段，缺段保留基线；
// 未知段忽略（留给版本回滚）。任何一段解析失败都返回错误。
func DecodeServiceSettings(kv map[string]string, base ServiceSettings) (ServiceSettings, error) {
	out := base
	for key, raw := range kv {
		var err error
		switch key {
		case SettingGlobalClient:
			err = json.Unmarshal([]byte(raw), &out.GlobalClient)
		case SettingPublicHost:
			out.PublicHost = raw
		case SettingLogLevel:
			out.LogLevel = raw
		case SettingTrafficFlush:
			out.TrafficFlushInterval, err = parseDurationText(key, raw)
		case SettingHeartbeat:
			err = json.Unmarshal([]byte(raw), &out.Heartbeat)
		case SettingSession:
			err = json.Unmarshal([]byte(raw), &out.Session)
		case SettingLogRotation:
			err = json.Unmarshal([]byte(raw), &out.LogRotation)
		case SettingServerProxy:
			err = json.Unmarshal([]byte(raw), &out.ServerProxy)
		case SettingVLESS:
			err = json.Unmarshal([]byte(raw), &out.VLESS)
		default:
			continue
		}
		if err != nil {
			return ServiceSettings{}, fmt.Errorf("decode service setting %s: %w", key, err)
		}
	}
	return out, nil
}

// EncodeVLESSSettings 把 vless 段编码成落库值（插件只写这一段，不动其它段）。
func EncodeVLESSSettings(v VLESSSettings) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode service setting %s: %w", SettingVLESS, err)
	}
	return string(b), nil
}

// parseDurationText 解析时长段（库里存 "10s" 这样的原文）。
func parseDurationText(key, raw string) (Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("decode service setting %s: %w", key, err)
	}
	return Duration(d), nil
}

// EncodeRoutingText 把路由配置渲染成 YAML 文本（管理台文本框用）。
func EncodeRoutingText(rc Routing) (string, error) {
	b, err := yaml.Marshal(rc)
	if err != nil {
		return "", fmt.Errorf("encode routing: %w", err)
	}
	return string(b), nil
}

// DecodeRoutingText 解析管理台文本框里的路由文本（YAML；JSON 亦可，YAML 1.2 是其超集）。
// 空文本回落内置默认路由；未知键报错而非静默忽略。
func DecodeRoutingText(text string) (Routing, error) {
	if strings.TrimSpace(text) == "" {
		return DefaultServerProxyRouting(), nil
	}
	var rc Routing
	dec := yaml.NewDecoder(strings.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(&rc); err != nil {
		return Routing{}, fmt.Errorf("parse routing: %w", err)
	}
	return rc, nil
}

// overlayDuration 在 patch 给出文本时解析覆盖目标；nil 保持不动。
func overlayDuration(dst *Duration, text *string) error {
	if text == nil {
		return nil
	}
	parsed, err := time.ParseDuration(*text)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", *text, err)
	}
	*dst = Duration(parsed)
	return nil
}
