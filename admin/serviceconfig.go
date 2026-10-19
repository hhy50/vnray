package admin

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/imfiqhan/steward"

	"github.com/hhy50/vnray/internal/config"
)

// ServiceSettings 是运行时服务配置的读写接口，由服务端装配层实现（core.serviceConfig），
// 经 AdminOptions.Settings 注入。admin 只依赖接口，不反向依赖 core。
type ServiceSettings interface {
	// Current 返回当前生效的配置快照。
	Current() config.ServiceSettings
	// Apply 校验并落库新配置，成功后热分发到运行态。
	Apply(config.ServiceSettings) error
}

// currentServiceSettings 返回当前生效的运行时配置；未注入 Settings（测试或未接装配）时
// 退回"默认值 + opts.PublicHost"，保证页面与分享链接仍可用。
func (s *AdminService) currentServiceSettings() config.ServiceSettings {
	if s.opts.Settings != nil {
		return s.opts.Settings.Current()
	}
	cur := config.DefaultServiceSettings()
	cur.PublicHost = s.opts.PublicHost
	return cur
}

// serviceConfigPage 是服务配置页的表单初值（一律渲染成文本/勾选，便于编辑）。
type serviceConfigPage struct {
	PublicHost           string
	HeartbeatEnabled     bool
	HeartbeatInterval    string
	HeartbeatTimeout     string
	LogLevel             string
	TrafficFlushInterval string
	KeepAliveInterval    string
	KeepAliveTimeout     string
	MaxStreamWindowKB    string
	AcceptBacklog        string
	StreamOpenTimeout    string
	LogRotateMaxSizeMB   string
	LogRotateMaxBackups  string
	LogRotateMaxAgeDays  string
	// 本机出口（server_proxy）。
	ServerProxyEnabled bool
	ServerProxyPublic  string
	ServerProxyPrivate string
	ServerProxyRouting string
	// LogToFile 指示日志是否输出到文件；false 时轮转参数不生效，页面给出提示。
	LogToFile bool
}

// logLevelChoices 是全局日志级别的下拉选项。
var logLevelChoices = []string{"debug", "info", "warn", "error"}

func (s *AdminService) pageServiceConfig(c *steward.Context) error {
	cur := s.currentServiceSettings()
	routingText, err := config.EncodeRoutingText(cur.ServerProxy.Routing)
	if err != nil {
		routingText = ""
	}
	page := serviceConfigPage{
		PublicHost:           cur.PublicHost,
		HeartbeatEnabled:     cur.Heartbeat.Enabled,
		HeartbeatInterval:    cur.Heartbeat.Interval.String(),
		HeartbeatTimeout:     cur.Heartbeat.Timeout.String(),
		LogLevel:             cur.LogLevel,
		TrafficFlushInterval: cur.TrafficFlushInterval.String(),
		KeepAliveInterval:    cur.Session.KeepAliveInterval.String(),
		KeepAliveTimeout:     cur.Session.KeepAliveTimeout.String(),
		MaxStreamWindowKB:    strconv.Itoa(cur.Session.MaxStreamWindow / 1024),
		AcceptBacklog:        strconv.Itoa(cur.Session.AcceptBacklog),
		StreamOpenTimeout:    cur.Session.StreamOpenTimeout.String(),
		LogRotateMaxSizeMB:   strconv.Itoa(cur.LogRotation.MaxSizeMB),
		LogRotateMaxBackups:  strconv.Itoa(cur.LogRotation.MaxBackups),
		LogRotateMaxAgeDays:  strconv.Itoa(cur.LogRotation.MaxAgeDays),
		ServerProxyEnabled:   cur.ServerProxy.Enabled,
		ServerProxyPublic:    cur.ServerProxy.Public,
		ServerProxyPrivate:   cur.ServerProxy.Private,
		ServerProxyRouting:   routingText,
		LogToFile:            s.opts.LogPath != "",
	}
	return c.Render("pages/serviceconfig.html", "服务配置", page)
}

// handleServiceConfigPost 是服务配置的写入口：解析表单 → 组装值对象 → 校验 → 经 Settings 落库并热分发。
func (s *AdminService) handleServiceConfigPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.Settings == nil {
		http.Error(w, "runtime service settings are not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	next, err := serviceSettingsFromForm(r, s.currentServiceSettings())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := next.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.opts.Settings.Apply(next); err != nil {
		http.Error(w, "save failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/admin/ops/_service-config", http.StatusSeeOther)
}

// serviceSettingsFromForm 把表单拼成值对象；空字段保留 cur 的现值。
func serviceSettingsFromForm(r *http.Request, cur config.ServiceSettings) (config.ServiceSettings, error) {
	next := cur
	next.PublicHost = strings.TrimSpace(r.PostFormValue("public_host"))
	next.Heartbeat.Enabled = r.PostFormValue("heartbeat_enabled") == "on"

	// 本机出口：开关 + 网段 + routing（YAML 文本；留空保留现值）。
	next.ServerProxy.Enabled = r.PostFormValue("server_proxy_enabled") == "on"
	next.ServerProxy.Public = strings.TrimSpace(r.PostFormValue("server_proxy_public"))
	next.ServerProxy.Private = strings.TrimSpace(r.PostFormValue("server_proxy_private"))
	if raw := r.PostFormValue("server_proxy_routing"); strings.TrimSpace(raw) != "" {
		rc, err := config.DecodeRoutingText(raw)
		if err != nil {
			return config.ServiceSettings{}, err
		}
		next.ServerProxy.Routing = rc
	}

	var err error
	if next.Heartbeat.Interval, err = parseFormDuration("heartbeat_interval", r.PostFormValue("heartbeat_interval"), cur.Heartbeat.Interval); err != nil {
		return config.ServiceSettings{}, err
	}
	if next.Heartbeat.Timeout, err = parseFormDuration("heartbeat_timeout", r.PostFormValue("heartbeat_timeout"), cur.Heartbeat.Timeout); err != nil {
		return config.ServiceSettings{}, err
	}
	if lv := strings.ToLower(strings.TrimSpace(r.PostFormValue("log_level"))); lv != "" {
		next.LogLevel = lv
	}
	if next.TrafficFlushInterval, err = parseFormDuration("traffic_flush_interval", r.PostFormValue("traffic_flush_interval"), cur.TrafficFlushInterval); err != nil {
		return config.ServiceSettings{}, err
	}
	if next.Session.KeepAliveInterval, err = parseFormDuration("keepalive_interval", r.PostFormValue("keepalive_interval"), cur.Session.KeepAliveInterval); err != nil {
		return config.ServiceSettings{}, err
	}
	if next.Session.KeepAliveTimeout, err = parseFormDuration("keepalive_timeout", r.PostFormValue("keepalive_timeout"), cur.Session.KeepAliveTimeout); err != nil {
		return config.ServiceSettings{}, err
	}
	if next.Session.StreamOpenTimeout, err = parseFormDuration("stream_open_timeout", r.PostFormValue("stream_open_timeout"), cur.Session.StreamOpenTimeout); err != nil {
		return config.ServiceSettings{}, err
	}
	// 单流窗口按 KiB 输入：0 表示用后端默认，非 0 时最小 256KiB。
	windowKB, err := parseFormInt("max_stream_window_kb", r.PostFormValue("max_stream_window_kb"), cur.Session.MaxStreamWindow/1024)
	if err != nil {
		return config.ServiceSettings{}, err
	}
	if windowKB <= 0 {
		next.Session.MaxStreamWindow = 0
	} else {
		next.Session.MaxStreamWindow = windowKB * 1024
	}
	if next.Session.AcceptBacklog, err = parseFormInt("accept_backlog", r.PostFormValue("accept_backlog"), cur.Session.AcceptBacklog); err != nil {
		return config.ServiceSettings{}, err
	}
	if next.LogRotation.MaxSizeMB, err = parseFormInt("log_rotate_max_size_mb", r.PostFormValue("log_rotate_max_size_mb"), cur.LogRotation.MaxSizeMB); err != nil {
		return config.ServiceSettings{}, err
	}
	if next.LogRotation.MaxBackups, err = parseFormInt("log_rotate_max_backups", r.PostFormValue("log_rotate_max_backups"), cur.LogRotation.MaxBackups); err != nil {
		return config.ServiceSettings{}, err
	}
	if next.LogRotation.MaxAgeDays, err = parseFormInt("log_rotate_max_age_days", r.PostFormValue("log_rotate_max_age_days"), cur.LogRotation.MaxAgeDays); err != nil {
		return config.ServiceSettings{}, err
	}
	return next, nil
}

// parseFormDuration 解析表单里的时长文本；空串保留 fallback，格式与取值范围由 Validate 兜底。
func parseFormDuration(field, raw string, fallback config.Duration) (config.Duration, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q (want e.g. 5s)", field, raw)
	}
	return config.Duration(d), nil
}

// parseFormInt 解析表单里的整数；空串保留 fallback。
func parseFormInt(field, raw string, fallback int) (int, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: want an integer, got %q", field, raw)
	}
	return n, nil
}
