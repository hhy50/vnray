package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// 默认管理台凭据：账号/审计/指标与转发绑定表共用 storage 指向的同一个库。
// 默认口令是弱口令，但管理台只监听 127.0.0.1；正式部署请在配置里设 admin.password。
const (
	DefaultAdminUsername = "admin"
	DefaultAdminPassword = "admin"
)

// AdminConfig 是 Web 管理端配置；监听地址固定为 127.0.0.1:<port>。
type AdminConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
	// Username 是管理台登录账号；留空取 DefaultAdminUsername。
	Username string `yaml:"username"`
	// Password 是登录口令：启动时种子/更新进管理库（bcrypt 存储，配置文件里的明文只在种子时使用）。
	// 留空且库里尚无账号时，首次启动生成随机口令并打一次日志。
	Password string `yaml:"password"`
}

// ResolvedUsername 返回归一化后的登录账号（空视为默认值）。
func (a AdminConfig) ResolvedUsername() string {
	if a.Username == "" {
		return DefaultAdminUsername
	}
	return a.Username
}

// Addr 返回管理端监听地址。
func (a AdminConfig) Addr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(a.Port))
}

// Validate 校验管理端配置；禁用时端口不做要求。
func (a AdminConfig) Validate() error {
	if a.Enabled && (a.Port <= 0 || a.Port >= 65536) {
		return fmt.Errorf("admin.port: %d must be in 1..65535 when admin is enabled", a.Port)
	}
	return nil
}

// Diagnostics 是诊断端点配置：PprofListen 非空时在该地址（host:port）暴露
// /debug/pprof（CPU/内存/协程画像）与 /debug/runtime（MemStats JSON 快照），
// 供运维与性能排查取数。监听建议保持回环地址，不要暴露到公网。
type Diagnostics struct {
	// PprofListen 是诊断 HTTP 端点监听地址；空（默认）= 关闭。
	PprofListen string `yaml:"pprof_listen"`
}

// Validate 校验诊断配置；关闭时不做要求。
func (d Diagnostics) Validate() error {
	if d.PprofListen == "" {
		return nil
	}
	if _, _, err := net.SplitHostPort(d.PprofListen); err != nil {
		return fmt.Errorf("diagnostics.pprof_listen: %q (want host:port)", d.PprofListen)
	}
	return nil
}

// Heartbeat 是服务端对每条客户端会话的心跳策略。它由运行时服务配置（管理台「服务配置」
// 页）管理，不在配置文件里。
//
// 心跳由**服务端主动发起**：Ping 带上本端毫秒时间戳，客户端原样回显 Pong，
// 服务端因此能在自己一个时钟上算出往返时延（不受两端时钟差影响），
// 结果出现在管理台的"延迟"列。存活检测仍由多路复用 keepalive 负责，两者互不影响。
type Heartbeat struct {
	Enabled  bool     `yaml:"enabled" json:"enabled"`
	Interval Duration `yaml:"interval" json:"interval"`
	Timeout  Duration `yaml:"timeout" json:"timeout"`
}

// DefaultHeartbeat 返回默认心跳策略：启用，5s 探一次，每次最多等 3s。
func DefaultHeartbeat() Heartbeat {
	return Heartbeat{
		Enabled:  true,
		Interval: Duration(5 * time.Second),
		Timeout:  Duration(3 * time.Second),
	}
}

// Validate 校验心跳策略；关闭时两个时长不参与校验。
func (h Heartbeat) Validate() error {
	if !h.Enabled {
		return nil
	}
	if h.Interval.Std() <= 0 {
		return fmt.Errorf("heartbeat.interval: must be > 0, got %q", h.Interval)
	}
	if h.Timeout.Std() <= 0 {
		return fmt.Errorf("heartbeat.timeout: must be > 0, got %q", h.Timeout)
	}
	return nil
}

// StorageConfig 是绑定表的存储配置：driver 取 sqlite|mysql（空为 sqlite），
// dsn 的语义由驱动决定——sqlite 是文件路径（空默认 bindings.db），mysql 是标准 DSN。
type StorageConfig struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

// DefaultDriver 是绑定表存储的默认驱动名。
const DefaultDriver = "sqlite"

// driver 返回归一化后的驱动名（空视为 sqlite）；供装配层构造存储用。
func (s StorageConfig) ResolvedDriver() string {
	if s.Driver == "" {
		return DefaultDriver
	}
	return s.Driver
}

// Validate 校验用户接入配置；未启用时不允许残留 bindings/storage，避免以为配了却不生效。
func (f StorageConfig) Validate() error {
	switch f.ResolvedDriver() {
	case DefaultDriver:
	case "mysql":
		if f.DSN == "" {
			return fmt.Errorf("forward.storage.dsn: required when driver is mysql")
		}
	default:
		return fmt.Errorf("forward.storage.driver: %q (want sqlite or mysql)", f.Driver)
	}
	return nil
}

// ServerOptions 是挂在 server 键下的接入配置：监听地址与握手认证。
// 其余配置段（admin/storage/heartbeat/log 等）与 server 同级。
type ServerOptions struct {
	Listen string `yaml:"listen"`
	Auth   Auth   `yaml:"auth"`
}

// DefaultServerProxyRouting 返回本机出口的缺省路由：direct 直连 + reject 兜底，无规则。
func DefaultServerProxyRouting() Routing {
	return Routing{
		Default: "direct",
		Outbounds: []Outbound{
			{Tag: "direct", Type: "direct"},
			{Tag: "reject", Type: "reject", Reason: "blocked by server proxy route"},
		},
	}
}

// PluginConfig 是入站插件的启动种子配置：仅当某插件在库中没有任何记录时，才按
// Enabled 列表用内置默认参数启用它；已有记录（含管理台停用）一律以库为准。
type PluginConfig struct {
	Enabled []string `yaml:"enabled"`
}

// Validate 校验种子列表：每一项都不得为空。
func (p PluginConfig) Validate() error {
	for i, name := range p.Enabled {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("plugin.enabled[%d]: plugin name must not be empty", i)
		}
	}
	return nil
}

// Server 是服务端配置。心跳与分享主机等运行期可热改的项不在其中，它们由管理台
// 「服务配置」页管理（见 ServiceSettings）。
type Server struct {
	Server      ServerOptions `yaml:"server"`
	Plugin      PluginConfig  `yaml:"plugin"`
	Storage     StorageConfig `yaml:"storage"`
	Admin       AdminConfig   `yaml:"admin"`
	Diagnostics Diagnostics   `yaml:"diagnostics"`
	Log         *Log          `yaml:"log"`
}

// DefaultServer 返回内置默认配置。
func DefaultServer() Server {
	return Server{
		Server:  ServerOptions{Listen: "127.0.0.1:9000"},
		Storage: StorageConfig{Driver: DefaultDriver},
		Admin: AdminConfig{
			Enabled:  true,
			Port:     8080,
			Username: DefaultAdminUsername,
		},
		Log: DefaultLog(),
	}
}

// Validate 校验服务端配置。
func (s Server) Validate() error {
	if err := validateAddress("server.listen", s.Server.Listen); err != nil {
		return err
	}
	if err := s.Admin.Validate(); err != nil {
		return err
	}
	if err := s.Server.Auth.Validate(); err != nil {
		return err
	}
	if err := s.Storage.Validate(); err != nil {
		return err
	}
	if err := s.Diagnostics.Validate(); err != nil {
		return err
	}
	if err := s.Plugin.Validate(); err != nil {
		return err
	}
	if s.Log != nil {
		return s.Log.Validate()
	}
	return nil
}

// validatePublicHostValue 是 public_host 的校验：可留空；填写时只接受裸主机名或
// IP——带协议/路径/端口说明配置写错了，与其生成坏链接不如启动时拒掉。
func validatePublicHostValue(raw string) error {
	h := strings.TrimSpace(raw)
	if h == "" {
		return nil
	}
	if strings.Contains(h, "://") || strings.ContainsAny(h, "/?# ") {
		return fmt.Errorf("public_host: %q must be a bare hostname or IP (no scheme, path or port)", raw)
	}
	return nil
}
