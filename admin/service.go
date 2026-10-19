package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/portmap"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
	"gorm.io/gorm"
)

// shutdownTimeout 是 Web 管理端优雅退出的最长等待。
const shutdownTimeout = 5 * time.Second

// AdminOptions 是管理面服务的构造选项。
type AdminOptions struct {
	Version string
	// Bindings 是 uuid → 客户端名 的转发绑定表；nil 表示不提供转发配置。
	Bindings *Bindings
	// Plugins 是入站插件管理器；nil 表示管理页不展示插件管理。
	Plugins *plugin.Manager
	// Listen 是数据面监听地址（host:port）：插件端口的默认建议值由它推导（+1）。
	Listen string
	// PublicHost 是分享链接里对外暴露的主机（server.public_host）；空表示退回
	// 管理台请求的 Host——管理台只监听回环地址，那样生成的链接只在本机可用。
	PublicHost string
	// Username/Password 是管理台登录账号与口令（bcrypt 落库，见 seedAdminUser）。
	Username string
	Password string
	// DB 是管理台的数据源：业务 SQL 存储的 GORM 连接（sqlite/mysql），管理台
	// 表（steward_ 前缀）与 user_bindings 同库；nil 视为装配失败，面板拒绝启动。
	DB *gorm.DB
	// Store 是持久层：管理台从这里读 user_traffic / client_traffic 两张流量表。
	Store store.Store
	// PortMaps 是端口映射监听管理器；nil 时端口映射页只读不热更新（数据面未装配）。
	PortMaps *portmap.Manager
	// LogPath 是服务端日志文件路径；空表示日志未写文件，日志页给出提示。
	LogPath string
	// Settings 是运行时服务配置的读写口（服务配置页）；nil 时该页只读（退回默认值）。
	Settings ServiceSettings
}

// AdminService 是 Web 管理面服务：steward 管理台挂在 /admin（登录、CSRF、
// 审计、RBAC 由框架接管），业务页与动作端点经 Resource.Page 挂进同一中间件链。
// 服务生命周期与进程一致、没有 Stop。
type AdminService struct {
	clients *transport.SessionManager
	opts    AdminOptions
	started time.Time
	srv     *http.Server
	panel   *panel
	// addr 为实际监听地址，Start 之后可用。
	addr string
}

// legacyRedirects 是 go-packs 时代的旧路径 → steward 路径；书签与小脚本
// 访问旧地址时 308 到新页。POST 不转（脚本需按新路径重放并携带 CSRF）。
var legacyRedirects = []struct{ from, to string }{
	{"/admin/Clients/Config", "/admin/ops/_client-config"},
	{"/admin/Clients", "/admin/ops/_clients"},
	{"/admin/Users", "/admin/ops/_users"},
	{"/admin/Plugins", "/admin/ops/_plugins"},
	{"/admin/Logs", "/admin/ops/_logs"},
	{"/admin/api/status", "/admin/ops/_status"},
	{"/admin/api/bindings", "/admin/ops/_bindings"},
	{"/admin/api/users/limit", "/admin/ops/_user-config"},
	{"/admin/api/clients/config", "/admin/ops/_client-config"},
	{"/admin/api/clients/kick", "/admin/ops/_client-kick"},
	{"/admin/api/plugins", "/admin/ops/_plugins-json"},
}

// NewAdminService 创建管理面服务：打开管理库、迁移表、种子账号并注册页面。
// addr 为监听地址；clients 为会话管理器（在线会话唯一来源）。
func NewAdminService(addr string, clients *transport.SessionManager, opts AdminOptions) (*AdminService, error) {
	s := &AdminService{
		clients: clients,
		opts:    opts,
		started: time.Now(),
	}
	p, err := newPanel(opts, s)
	if err != nil {
		return nil, err
	}
	s.panel = p

	mux := http.NewServeMux()
	for _, lr := range legacyRedirects {
		to := lr.to
		mux.HandleFunc("GET "+lr.from, func(w http.ResponseWriter, r *http.Request) {
			// 保留查询串：客户端配置页的 ?name= 在新路径下仍然有效。
			http.Redirect(w, r, to+"?"+r.URL.RawQuery, http.StatusPermanentRedirect)
		})
	}
	// steward 的路由模式自带 /admin 前缀，原样挂载即可。
	mux.Handle("/admin", p.app)
	mux.Handle("/admin/", p.app)
	mux.Handle("/", http.RedirectHandler("/admin/", http.StatusSeeOther))
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

// Name 返回服务名，用于启动日志与诊断。
func (s *AdminService) Name() string { return "AdminService" }

// Start 启动 HTTP 服务并立即返回；ctx 只用于打开监听。
func (s *AdminService) Start(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("webadmin listen %s: %w", s.srv.Addr, err)
	}
	s.addr = ln.Addr().String()
	logging.Info("admin: web admin started", "addr", s.addr, "panel", "/admin/")
	routine.Go("admin.service.serve", func() {
		// Serve 只在 Shutdown 后返回 ErrServerClosed，错误语义由 Drain 负责。
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Error("admin: web admin stopped", "err", err)
		}
	})
	return nil
}

// Drain 实现 extension.Drainer：优雅关闭管理端，等在途请求处理完（ctx 无 deadline 时用
// shutdownTimeout），随后停采样协程并释放管理库。
func (s *AdminService) Drain(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
	}
	err := s.srv.Shutdown(ctx)
	s.panel.close()
	return err
}

// Addr 返回实际监听地址；未启动时返回配置地址。
func (s *AdminService) Addr() string {
	if s.addr != "" {
		return s.addr
	}
	return s.srv.Addr
}

// panelActiveTunnels 暴露当前活跃隧道数给客户端看板。
func (s *AdminService) panelActiveTunnels() int64 {
	_, active := transport.TunnelStats()
	return active
}

// statusJSON 是 _status 的响应结构。
type statusJSON struct {
	Version       string       `json:"version"`
	StartedAt     time.Time    `json:"started_at"`
	UptimeSeconds int64        `json:"uptime_seconds"`
	ClientCount   int          `json:"client_count"`
	Clients       []ClientInfo `json:"clients"`
}

func (s *AdminService) status() statusJSON {
	return statusJSON{
		Version:       s.opts.Version,
		StartedAt:     s.started,
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		ClientCount:   s.clients.ClientCount(),
		Clients:       s.clientsSnapshot(),
	}
}

// clientsSnapshot 从会话管理器组装管理台的客户端视图：静态字段取登记项，
// 请求数与心跳结果由会话实时持有，这里读取而不是事件推送。
func (s *AdminService) clientsSnapshot() []ClientInfo {
	entries := s.clients.Clients()
	out := make([]ClientInfo, 0, len(entries))
	for _, e := range entries {
		c := e.Client
		if c == nil {
			continue
		}
		info := ClientInfo{
			ID:          e.ID,
			Remote:      e.Info.Remote,
			ConnectedAt: e.Info.Since,
			Client:      c.Info(),
		}
		info.LastHeartbeat = c.LastHeartbeat()
		info.LastRTTMs = float64(c.LastRTT()) / float64(time.Millisecond)
		out = append(out, info)
	}
	return out
}

// pluginsList 返回插件状态列表；未配置插件管理器时返回 nil。
func (s *AdminService) pluginsList() []plugin.Status {
	if s.opts.Plugins == nil {
		return nil
	}
	return s.opts.Plugins.List()
}

// bindings 返回转发绑定快照；未配置绑定表时返回空列表。
func (s *AdminService) bindings() []Binding {
	if s.opts.Bindings == nil {
		return nil
	}
	return s.opts.Bindings.Snapshot()
}

func (s *AdminService) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.status())
}

// handleBindingsGet 提供转发绑定的 JSON 快照（脚本采集用）。
func (s *AdminService) handleBindingsGet(w http.ResponseWriter, r *http.Request) {
	if s.opts.Bindings == nil {
		http.Error(w, "forward bindings are not configured", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, s.bindings())
}

// handleBindingsPost 提供转发绑定的写入口：POST 按 action=set/delete/enable/disable 修改。
// enable/disable 是列表行内的开关，只翻转 status，其余字段原样保留。
func (s *AdminService) handleBindingsPost(w http.ResponseWriter, r *http.Request) {
	if s.opts.Bindings == nil {
		http.Error(w, "forward bindings are not configured", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.PostFormValue("user_key"))
	client := strings.TrimSpace(r.PostFormValue("client"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	p := store.Protocol(strings.TrimSpace(r.PostFormValue("protocol")))
	if p == "" {
		p = store.ProtocolVNV1
	}
	if key == "" || !p.Valid() {
		http.Error(w, "a known protocol and a non-empty user_key are required", http.StatusBadRequest)
		return
	}
	switch r.PostFormValue("action") {
	case "delete":
		deleted, err := s.opts.Bindings.DeleteUser(p, key)
		if err != nil {
			logging.Error("admin: user binding delete failed", "protocol", p, "user_key", key, "err", err)
			http.Error(w, "delete failed: storage error", http.StatusInternalServerError)
			return
		}
		logging.Info("admin: user binding deleted",
			"protocol", p, "user_key", key, "existed", deleted)
	case "enable", "disable":
		cur, ok := s.opts.Bindings.Lookup(p, key)
		if !ok {
			http.Error(w, "user is not registered", http.StatusNotFound)
			return
		}
		cur.Status = store.UserEnabled
		if r.PostFormValue("action") == "disable" {
			cur.Status = store.UserDisabled
		}
		if err := s.opts.Bindings.SetUser(cur); err != nil {
			logging.Error("admin: user status update failed",
				"protocol", p, "user_key", key, "status", cur.Status, "err", err)
			http.Error(w, "save failed: storage error", http.StatusInternalServerError)
			return
		}
		logging.Info("admin: user status updated",
			"protocol", p, "user_key", key, "status", cur.Status)
	default:
		if client == "" {
			http.Error(w, "client must not be empty", http.StatusBadRequest)
			return
		}
		if name == "" {
			http.Error(w, "name must not be empty", http.StatusBadRequest)
			return
		}
		// vmess/vless 用户的密钥就是 uuid：格式不对时入站验证器装不进，
		// 会出现"表里有、连不上"，在这里就拒掉。
		if err := plugin.ValidateUserKey(p, key); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// 身份是 (protocol, user_key)：这里按提交的协议查，查到的行其协议必然一致，
		// 「换协议」因此表现为新增另一行（旧行仍在，需另行删除），不存在就地改协议。
		// 覆盖时不丢既有 config（限速）：SetUser 只更新 name/status/client/config。
		cur, ok := s.opts.Bindings.Lookup(p, key)
		if !ok {
			cur = Binding{UserKey: key, Protocol: p}
		}
		cur.Name = name
		cur.Status = store.UserEnabled
		if r.PostFormValue("status") == "0" {
			cur.Status = store.UserDisabled
		}
		cur.Client = client
		if err := s.opts.Bindings.SetUser(cur); err != nil {
			logging.Error("admin: user binding save failed",
				"protocol", p, "user_key", key, "client", client, "err", err)
			http.Error(w, "save failed: storage error", http.StatusInternalServerError)
			return
		}
		logging.Info("admin: user binding saved",
			"protocol", p, "user_key", key, "name", name, "client", client)
	}
	http.Redirect(w, r, "/admin/ops/_users", http.StatusSeeOther)
}

// handlePlugins 返回插件状态列表（JSON，脚本采集用）。
func (s *AdminService) handlePlugins(w http.ResponseWriter, _ *http.Request) {
	if s.opts.Plugins == nil {
		http.Error(w, "plugins are not configured", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, s.pluginsList())
}

// pluginFormParams 把表单里除控制字段外的项原样收成插件参数：表单字段名即参数名
// （v2ray 的 vmess_port/vless_port，ss 的 port/method）。
func pluginFormParams(r *http.Request) map[string]string {
	out := make(map[string]string, len(r.PostForm))
	for k, vs := range r.PostForm {
		switch k {
		case "plugin", "action", "_token":
			continue
		}
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

// pluginFormPorts 把表单里形如 <协议>_port 的字段解析成协议 → 端口，供多端口插件的
// 端口热切换使用。缺少可用字段或端口非法时报错。
func pluginFormPorts(r *http.Request) (map[store.Protocol]int, error) {
	out := make(map[store.Protocol]int)
	for k, vs := range r.PostForm {
		name, ok := strings.CutSuffix(k, "_port")
		if !ok || len(vs) == 0 {
			continue
		}
		protocol := store.Protocol(name)
		if !protocol.Valid() {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(vs[0]))
		if err != nil || port <= 0 || port >= 65536 {
			return nil, fmt.Errorf("%s must be an integer in 1..65535", k)
		}
		out[protocol] = port
	}
	if len(out) == 0 {
		return nil, errors.New("no <protocol>_port field found")
	}
	return out, nil
}

// handlePluginAction 是插件控制端点（POST，表单字段 plugin + action）：start（带各端口）/
// stop 对所有插件通用；set_port 支持运行中热切换。
func (s *AdminService) handlePluginAction(w http.ResponseWriter, r *http.Request) {
	if s.opts.Plugins == nil {
		http.Error(w, "plugins are not configured", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("plugin"))
	if name == "" {
		http.Error(w, "missing plugin name", http.StatusNotFound)
		return
	}
	p, err := s.opts.Plugins.Get(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	ctx := r.Context()
	switch r.PostFormValue("action") {
	case "start":
		params := pluginFormParams(r)
		if err := p.Enable(ctx, params); err != nil {
			logging.Error("admin: plugin enable failed", "plugin", name, "params", params, "err", err)
			http.Error(w, "enable failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		logging.Info("admin: plugin enabled", "plugin", name, "params", params)
	case "stop":
		if err := p.Disable(ctx); err != nil {
			logging.Error("admin: plugin disable failed", "plugin", name, "err", err)
			http.Error(w, "disable failed: storage error", http.StatusInternalServerError)
			return
		}
		logging.Info("admin: plugin disabled", "plugin", name)
	case "set_port":
		if ms, ok := p.(plugin.MultiPortSetter); ok {
			ports, err := pluginFormPorts(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := ms.SetPorts(ctx, ports); err != nil {
				logging.Error("admin: plugin set ports failed", "plugin", name, "ports", ports, "err", err)
				http.Error(w, "set ports failed: "+err.Error(), http.StatusBadRequest)
				return
			}
			logging.Info("admin: plugin ports updated", "plugin", name, "ports", ports)
			break
		}
		port, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("port")))
		if err != nil || port <= 0 || port >= 65536 {
			http.Error(w, "port must be an integer in 1..65535", http.StatusBadRequest)
			return
		}
		setter, ok := p.(plugin.PortSetter)
		if !ok {
			http.Error(w, "plugin does not support changing port at runtime", http.StatusBadRequest)
			return
		}
		if err := setter.SetPort(ctx, port); err != nil {
			logging.Error("admin: plugin set port failed", "plugin", name, "port", port, "err", err)
			http.Error(w, "set port failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		logging.Info("admin: plugin port updated", "plugin", name, "port", port)
	case "set_uuid":
		// uuid 路由与限速已并入用户表：插件侧不再有独立写口，统一走 _bindings。
		http.Error(w, "uuid routing moved to user bindings (POST /admin/ops/_bindings with user_type=v2ray-uuid)",
			http.StatusGone)
		return
	case "del_uuid":
		http.Error(w, "uuid routing moved to user bindings (POST /admin/ops/_bindings with user_type=v2ray-uuid)",
			http.StatusGone)
		return
	case "set_limit":
		http.Error(w, "account limits moved to user bindings (POST /admin/ops/_user-config)",
			http.StatusGone)
		return
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/admin/ops/_plugins", http.StatusSeeOther)
}

func parseLimitKB(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 || n > (1<<63-1)/1024 {
		return 0, fmt.Errorf("limit must be between 0 and %d KB/s", int64((1<<63-1)/1024))
	}
	return n, nil
}

// parseLogLevel 归一化日志级别：空与 info 都视为默认（存空串，读时按 info），
// 其余只接受 debug/warn/error——写进配置的必须是能解析的级别，否则会话会静默退回 info。
func parseLogLevel(raw string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(raw)); v {
	case "", "info":
		return "", nil
	case "debug", "warn", "error":
		return v, nil
	default:
		return "", fmt.Errorf("log_level must be debug, info, warn or error, got %q", raw)
	}
}

// handleUserConfig 把用户限速与日志级别写入唯一配置源，再由观察者更新运行态。
func (s *AdminService) handleUserConfig(w http.ResponseWriter, r *http.Request) {
	if s.opts.Bindings == nil {
		http.Error(w, "forward bindings are not configured", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.PostFormValue("user_key"))
	p := store.Protocol(strings.TrimSpace(r.PostFormValue("protocol")))
	if p == "" {
		p = store.ProtocolVNV1
	}
	if key == "" || !p.Valid() {
		http.Error(w, "a known protocol and a non-empty user_key are required", http.StatusBadRequest)
		return
	}
	upKB, err1 := parseLimitKB(r.PostFormValue("up_kbps"))
	downKB, err2 := parseLimitKB(r.PostFormValue("down_kbps"))
	if err1 != nil || err2 != nil {
		http.Error(w, "up_kbps/down_kbps must be non-negative integers", http.StatusBadRequest)
		return
	}
	concurrency, err := parseConcurrency(r.PostFormValue("concurrency"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	level, err := parseLogLevel(r.PostFormValue("log_level"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// 限额与日志级别都挂在既有绑定行上，因此该用户必须已登记（客户端的归属不能凭空捏造）。
	cur, ok := s.opts.Bindings.Lookup(p, key)
	if !ok {
		http.Error(w, fmt.Sprintf("%s user %q is not registered", p, key), http.StatusNotFound)
		return
	}
	cur.Config = store.UserConfig{UpLimit: upKB * 1024, DownLimit: downKB * 1024, LogLevel: level, Concurrency: concurrency}
	if err := s.opts.Bindings.SetUser(cur); err != nil {
		logging.Error("admin: user config save failed", "protocol", p, "user_key", key, "err", err)
		http.Error(w, "save failed: storage error", http.StatusInternalServerError)
		return
	}
	logging.Info("admin: user config saved", "protocol", p, "user_key", key,
		"up_kbps", upKB, "down_kbps", downKB, "log_level", level, "concurrency", concurrency)
	http.Redirect(w, r, "/admin/ops/_users", http.StatusSeeOther)
}

// parseConcurrency 解析非负整数并发上限；空串视为 0（不限）。
func parseConcurrency(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("concurrency must be a non-negative integer")
	}
	return n, nil
}

// handleClientKick 断开一个在线客户端：按管理台编号找到会话并关闭其数据面连接。
// 客户端若开启了自动重连，稍后会重新接入并获得新编号。
func (s *AdminService) handleClientKick(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseUint(strings.TrimSpace(r.PostFormValue("id")), 10, 64)
	if err != nil {
		http.Error(w, "bad client id", http.StatusBadRequest)
		return
	}
	for _, e := range s.clients.Clients() {
		if e.ID != id {
			continue
		}
		name := ""
		if e.Info != nil {
			name = e.Info.Name
		}
		if err := e.Client.Close(); err != nil {
			logging.Error("admin: kick client failed", "id", id, "err", err)
			http.Error(w, "kick failed", http.StatusInternalServerError)
			return
		}
		logging.Info("admin: client kicked", "id", id, "name", name)
		http.Redirect(w, r, "/admin/ops/_clients", http.StatusSeeOther)
		return
	}
	http.Error(w, "no such client", http.StatusNotFound)
}

// serverPort 返回数据面监听端口（AdminOptions.Listen 的端口部分）；解析失败返回 0，
// 插件端口的默认建议随之退化为"当前端口"，由服务端校验兜底。
func (s *AdminService) serverPort() int {
	_, portStr, err := net.SplitHostPort(s.opts.Listen)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return port
}

// handleClientConfig 保存客户端的服务端配置（client_bindings 记录）：表单 name +
// config（JSON 文本，空串表示清除）。与客户端上报配置相互独立——上报配置只读，
// 客户端的生效配置由其 YAML 决定。
func (s *AdminService) handleClientConfig(w http.ResponseWriter, r *http.Request) {
	if s.opts.Bindings == nil {
		http.Error(w, "forward bindings are not configured", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	config := strings.TrimSpace(r.PostFormValue("config"))
	if name == "" {
		http.Error(w, "client name must not be empty", http.StatusBadRequest)
		return
	}
	level, err := parseLogLevel(r.PostFormValue("log_level"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var obj map[string]any
	if config != "" {
		if err := json.Unmarshal([]byte(config), &obj); err != nil || obj == nil {
			http.Error(w, "config must be a JSON object", http.StatusBadRequest)
			return
		}
	}
	if obj == nil {
		obj = map[string]any{}
	}
	// 日志级别的下拉框是唯一写口：只有它与文本域里的值不同时才重写 JSON，
	// 否则原样保存，免得把运维写的排版整体重排。
	current, _ := obj["log_level"].(string)
	if norm, _ := parseLogLevel(current); norm != level {
		if level == "" {
			delete(obj, "log_level")
		} else {
			obj["log_level"] = level
		}
		merged, err := json.MarshalIndent(obj, "", "  ")
		if err != nil {
			http.Error(w, "config must be a JSON object", http.StatusBadRequest)
			return
		}
		config = string(merged)
	}
	cur, ok := s.opts.Bindings.LookupClient(name)
	if !ok {
		cur = ClientBinding{ClientKey: name, ClientType: store.ClientTypeProxy}
	}
	// 管理页编辑的是服务端下发的运维值；config 列留给客户端上报的快照。
	cur.ServerConfig = config
	if err := s.opts.Bindings.SetClient(cur); err != nil {
		logging.Error("admin: client config save failed", "client", name, "err", err)
		http.Error(w, "save failed: storage error", http.StatusInternalServerError)
		return
	}
	logging.Info("admin: client config saved", "client", name, "bytes", len(config))
	http.Redirect(w, r, "/admin/ops/_client-config?name="+url.QueryEscape(name), http.StatusSeeOther)
}

// handleClientHosts 是客户端自定义配置（host 表 + IP 段映射）的读写口：
//   - GET ?name= 返回该客户端当前的自定义 host 表与 IP 段映射（JSON，弹窗预填用）；
//   - POST 接收表单 domain/ip 与 ip_from/ip_to 两个并列组，校验后写穿 server_config
//     的 custom_hosts 与 ip_maps 键，再推送给在线客户端。
func (s *AdminService) handleClientHosts(w http.ResponseWriter, r *http.Request) {
	if s.opts.Bindings == nil {
		http.Error(w, "forward bindings are not configured", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodGet {
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			http.Error(w, "client name is required", http.StatusBadRequest)
			return
		}
		cfg := s.opts.Bindings.PushConfig(name)
		writeJSON(w, http.StatusOK, map[string]any{
			"hosts": cfg.CustomHosts, "ip_maps": cfg.IPMaps,
			"concurrency": s.opts.Bindings.ClientConcurrency(name),
		})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		http.Error(w, "client name must not be empty", http.StatusBadRequest)
		return
	}
	hosts, err := parseCustomHosts(r.PostForm["domain"], r.PostForm["ip"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ipMaps, err := parseIPMaps(r.PostForm["ip_from"], r.PostForm["ip_to"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	concurrency, err := parseConcurrency(r.PostFormValue("concurrency"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 读改写 server_config：只动 custom_hosts / ip_maps / concurrency 三键，其余运维字段原样保留。
	cur, ok := s.opts.Bindings.LookupClient(name)
	if !ok {
		cur = ClientBinding{ClientKey: name, ClientType: store.ClientTypeProxy}
	}
	obj := map[string]any{}
	if cur.ServerConfig != "" {
		if err := json.Unmarshal([]byte(cur.ServerConfig), &obj); err != nil || obj == nil {
			http.Error(w, "existing server config is not valid JSON; fix it on the client config page first", http.StatusBadRequest)
			return
		}
	}
	if len(hosts) == 0 {
		delete(obj, "custom_hosts")
	} else {
		obj["custom_hosts"] = hosts
	}
	if len(ipMaps) == 0 {
		delete(obj, "ip_maps")
	} else {
		obj["ip_maps"] = ipMaps
	}
	if concurrency == 0 {
		delete(obj, "concurrency")
	} else {
		obj["concurrency"] = concurrency
	}
	merged, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		http.Error(w, "server config is not a valid JSON object", http.StatusBadRequest)
		return
	}
	cur.ServerConfig = string(merged)
	if err := s.opts.Bindings.SetClient(cur); err != nil {
		logging.Error("admin: client custom config save failed", "client", name, "err", err)
		http.Error(w, "save failed: storage error", http.StatusInternalServerError)
		return
	}
	logging.Info("admin: client custom config saved", "client", name,
		"custom_hosts", len(hosts), "ip_maps", len(ipMaps), "concurrency", concurrency)
	// 在线客户端立即生效：自定义 host / IP 段映射走数据面下发帧，并发上限走服务端会话热改。
	if pushed := s.pushConfig(name, protocol.PushConfig{CustomHosts: hosts, IPMaps: ipMaps}); pushed {
		s.applyClientConcurrency(name, concurrency)
		logging.Info("admin: client push config done", "client", name)
	}
	// 离线客户端并发上限在 auth 建立会话时按 server_config 读取，无需在此处理。
	http.Redirect(w, r, "/admin/ops/_clients", http.StatusSeeOther)
}

// applyClientConcurrency 把 client 级并发上限热更到在线会话；找不到在线会话时静默跳过
// （离线客户端在下次认证建立会话时按 server_config 读取）。
func (s *AdminService) applyClientConcurrency(name string, max int) {
	for _, e := range s.clients.ClientSessions() {
		if e.Info().Name == name {
			e.SetClientConcurrency(max)
			return
		}
	}
}

// parseIPMaps 校验并归一化表单提交的 IP 段映射行：from/to 按行对齐，
// 全空行视为占位跳过，只有一栏为空报错。
func parseIPMaps(froms, tos []string) ([]protocol.IPMap, error) {
	var maps []protocol.IPMap
	for i := range froms {
		from := strings.TrimSpace(froms[i])
		to := ""
		if i < len(tos) {
			to = strings.TrimSpace(tos[i])
		}
		if from == "" && to == "" {
			continue
		}
		if from == "" || to == "" {
			return nil, fmt.Errorf("第 %d 行：源段与目标段必须成对填写", i+1)
		}
		if err := dispatch.ValidIPMap(from, to); err != nil {
			return nil, fmt.Errorf("第 %d 行：%v", i+1, err)
		}
		maps = append(maps, protocol.IPMap{From: from, To: to})
	}
	return maps, nil
}

// parseCustomHosts 校验并归一化表单提交的自定义 host 行：domain 与 ip 按行对齐，
// 全空行视为占位跳过，只有一栏为空报错。域名去尾点、转小写存储（查表时同样归一化）。
func parseCustomHosts(domains, ips []string) ([]protocol.CustomHost, error) {
	var hosts []protocol.CustomHost
	for i := range domains {
		domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domains[i])), ".")
		ip := ""
		if i < len(ips) {
			ip = strings.TrimSpace(ips[i])
		}
		if domain == "" && ip == "" {
			continue
		}
		if domain == "" || ip == "" {
			return nil, fmt.Errorf("第 %d 行：域名与 IP 必须成对填写", i+1)
		}
		if strings.ContainsAny(domain, " \t/:") || net.ParseIP(domain) != nil {
			return nil, fmt.Errorf("第 %d 行：%q 不是合法域名", i+1, domain)
		}
		if len(domain) > 253 {
			return nil, fmt.Errorf("第 %d 行：域名过长", i+1)
		}
		if net.ParseIP(ip) == nil {
			return nil, fmt.Errorf("第 %d 行：%q 不是合法 IP", i+1, ip)
		}
		hosts = append(hosts, protocol.CustomHost{Domain: domain, IP: ip})
	}
	return hosts, nil
}

// pushConfig 把服务端下发的全量配置推送给在线客户端；该客户端不在线时返回 false。
func (s *AdminService) pushConfig(name string, cfg protocol.PushConfig) bool {
	for _, e := range s.clients.ClientSessions() {
		if e.Info().Name != name {
			continue
		}
		if err := e.SendPushConfig(cfg); err != nil {
			logging.Warn("admin: push config failed", "client", name, "err", err)
			return false
		}
		return true
	}
	return false
}

// syncPortMaps 热更新端口映射监听器；管理器未装配（数据面未启用）时静默跳过。
func (s *AdminService) syncPortMaps() {
	if s.opts.PortMaps == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	s.opts.PortMaps.Sync(ctx)
}

// handlePortMapsPost 是端口映射的写入口（POST，表单 action=save/toggle/delete）：
// 写穿 port_mappings 表后热更新服务端监听器集合。
func (s *AdminService) handlePortMapsPost(w http.ResponseWriter, r *http.Request) {
	if s.opts.Store == nil {
		http.Error(w, "port mappings are not configured", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	switch r.PostFormValue("action") {
	case "delete":
		id, err := strconv.ParseUint(strings.TrimSpace(r.PostFormValue("id")), 10, 64)
		if err != nil || id == 0 {
			http.Error(w, "bad mapping id", http.StatusBadRequest)
			return
		}
		deleted, err := s.opts.Store.DeletePortMapping(ctx, id)
		if err != nil {
			logging.Error("admin: port mapping delete failed", "id", id, "err", err)
			http.Error(w, "delete failed: storage error", http.StatusInternalServerError)
			return
		}
		logging.Info("admin: port mapping deleted", "id", id, "existed", deleted)
	case "toggle":
		id, err := strconv.ParseUint(strings.TrimSpace(r.PostFormValue("id")), 10, 64)
		if err != nil || id == 0 {
			http.Error(w, "bad mapping id", http.StatusBadRequest)
			return
		}
		m, ok, err := s.opts.Store.GetPortMapping(ctx, id)
		switch {
		case err != nil:
			http.Error(w, "load failed: storage error", http.StatusInternalServerError)
			return
		case !ok:
			http.Error(w, "no such mapping", http.StatusNotFound)
			return
		}
		m.Enabled = !m.Enabled
		if err := s.opts.Store.SetPortMapping(ctx, m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		logging.Info("admin: port mapping toggled", "id", id, "enabled", m.Enabled)
	case "save":
		client := strings.TrimSpace(r.PostFormValue("client"))
		remotePort, errRemote := strconv.Atoi(strings.TrimSpace(r.PostFormValue("remote_port")))
		localHost := strings.TrimSpace(r.PostFormValue("local_host"))
		localPort, errLocal := strconv.Atoi(strings.TrimSpace(r.PostFormValue("local_port")))
		remark := strings.TrimSpace(r.PostFormValue("remark"))
		enabled := r.PostFormValue("enabled") == "on"
		id, _ := strconv.ParseUint(strings.TrimSpace(r.PostFormValue("id")), 10, 64)
		upKB, errUp := parseLimitKB(r.PostFormValue("up_kbps"))
		downKB, errDown := parseLimitKB(r.PostFormValue("down_kbps"))
		if client == "" || errRemote != nil || localHost == "" || errLocal != nil || errUp != nil || errDown != nil {
			http.Error(w, "client、远程端口、本地地址都必须填写，端口与限速必须是非负整数", http.StatusBadRequest)
			return
		}
		m := store.PortMapping{ID: id, Client: client, RemotePort: remotePort,
			LocalHost: localHost, LocalPort: localPort,
			Config:  store.PortConfig{UpLimit: upKB * 1024, DownLimit: downKB * 1024},
			Enabled: enabled, Remark: remark}
		if err := s.opts.Store.SetPortMapping(ctx, m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		logging.Info("admin: port mapping saved", "id", id, "client", client,
			"remote_port", remotePort,
			"target", net.JoinHostPort(localHost, strconv.Itoa(localPort)),
			"up_kbps", upKB, "down_kbps", downKB, "enabled", enabled)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	s.syncPortMaps()
	http.Redirect(w, r, "/admin/ops/_portmaps", http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ClientInfo 是单个客户端会话的只读视图（管理台 _status 的数据行）。
type ClientInfo struct {
	ID          uint64    `json:"id"`
	Remote      string    `json:"remote"`
	ConnectedAt time.Time `json:"connected_at"`
	// Client 是客户端在认证时上报的配置与环境摘要。
	Client protocol.ClientInfo `json:"client"`
	// LastHeartbeat 是最近一次心跳应答的本地时刻；尚未测到时为 time.Time 零值。
	LastHeartbeat time.Time `json:"last_heartbeat"`
	// LastRTTMs 是最近一次心跳测得的往返时延（毫秒）；尚未测到时为 0。
	LastRTTMs float64 `json:"last_rtt_ms"`
}
