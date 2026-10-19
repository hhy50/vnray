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
	// 与 logging 里 lumberjack 的默认值一致。
	DefaultLogRotateMaxSizeMB  = 500
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

// EncodeServiceSettings 序列化整份运行时配置（时长以 "5s" 文本表达）。
func EncodeServiceSettings(s ServiceSettings) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode service settings: %w", err)
	}
	return string(b), nil
}

// serviceSettingsPatch 是解码用的指针视图：只有出现过的键才覆盖 base，缺键保留默认，
// 保证后续给 ServiceSettings 新增字段时，库里的旧记录不会把新字段清零。
type serviceSettingsPatch struct {
	PublicHost           *string           `json:"public_host"`
	Heartbeat            *heartbeatPatch   `json:"heartbeat"`
	LogLevel             *string           `json:"log_level"`
	TrafficFlushInterval *string           `json:"traffic_flush_interval"`
	Session              *sessionPatch     `json:"session"`
	LogRotation          *logRotationPatch `json:"log_rotation"`
	ServerProxy          *serverProxyPatch `json:"server_proxy"`
}

type heartbeatPatch struct {
	Enabled  *bool   `json:"enabled"`
	Interval *string `json:"interval"`
	Timeout  *string `json:"timeout"`
}

type sessionPatch struct {
	KeepAliveInterval *string `json:"keepalive_interval"`
	KeepAliveTimeout  *string `json:"keepalive_timeout"`
	MaxStreamWindow   *int    `json:"max_stream_window"`
	AcceptBacklog     *int    `json:"accept_backlog"`
	StreamOpenTimeout *string `json:"stream_open_timeout"`
}

type logRotationPatch struct {
	MaxSizeMB  *int `json:"max_size_mb"`
	MaxBackups *int `json:"max_backups"`
	MaxAgeDays *int `json:"max_age_days"`
}

type serverProxyPatch struct {
	Enabled *bool    `json:"enabled"`
	Public  *string  `json:"public"`
	Private *string  `json:"private"`
	Routing *Routing `json:"routing"`
}

// DecodeServiceSettings 把管理台保存的 JSON 快照覆盖到 base 上；raw 为空时返回 base。
func DecodeServiceSettings(raw string, base ServiceSettings) (ServiceSettings, error) {
	if strings.TrimSpace(raw) == "" {
		return base, nil
	}
	var p serviceSettingsPatch
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return ServiceSettings{}, fmt.Errorf("decode service settings: %w", err)
	}
	out := base
	if p.PublicHost != nil {
		out.PublicHost = *p.PublicHost
	}
	if p.LogLevel != nil {
		out.LogLevel = *p.LogLevel
	}
	if p.Heartbeat != nil {
		if p.Heartbeat.Enabled != nil {
			out.Heartbeat.Enabled = *p.Heartbeat.Enabled
		}
		if err := overlayDuration(&out.Heartbeat.Interval, p.Heartbeat.Interval); err != nil {
			return ServiceSettings{}, err
		}
		if err := overlayDuration(&out.Heartbeat.Timeout, p.Heartbeat.Timeout); err != nil {
			return ServiceSettings{}, err
		}
	}
	if err := overlayDuration(&out.TrafficFlushInterval, p.TrafficFlushInterval); err != nil {
		return ServiceSettings{}, err
	}
	if p.Session != nil {
		if err := overlayDuration(&out.Session.KeepAliveInterval, p.Session.KeepAliveInterval); err != nil {
			return ServiceSettings{}, err
		}
		if err := overlayDuration(&out.Session.KeepAliveTimeout, p.Session.KeepAliveTimeout); err != nil {
			return ServiceSettings{}, err
		}
		if err := overlayDuration(&out.Session.StreamOpenTimeout, p.Session.StreamOpenTimeout); err != nil {
			return ServiceSettings{}, err
		}
		if p.Session.MaxStreamWindow != nil {
			out.Session.MaxStreamWindow = *p.Session.MaxStreamWindow
		}
		if p.Session.AcceptBacklog != nil {
			out.Session.AcceptBacklog = *p.Session.AcceptBacklog
		}
	}
	if p.LogRotation != nil {
		if p.LogRotation.MaxSizeMB != nil {
			out.LogRotation.MaxSizeMB = *p.LogRotation.MaxSizeMB
		}
		if p.LogRotation.MaxBackups != nil {
			out.LogRotation.MaxBackups = *p.LogRotation.MaxBackups
		}
		if p.LogRotation.MaxAgeDays != nil {
			out.LogRotation.MaxAgeDays = *p.LogRotation.MaxAgeDays
		}
	}
	if p.ServerProxy != nil {
		if p.ServerProxy.Enabled != nil {
			out.ServerProxy.Enabled = *p.ServerProxy.Enabled
		}
		if p.ServerProxy.Public != nil {
			out.ServerProxy.Public = *p.ServerProxy.Public
		}
		if p.ServerProxy.Private != nil {
			out.ServerProxy.Private = *p.ServerProxy.Private
		}
		if p.ServerProxy.Routing != nil {
			out.ServerProxy.Routing = *p.ServerProxy.Routing
		}
	}
	return out, nil
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
