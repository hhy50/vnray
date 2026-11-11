package admin

// service.go 是管理面服务：HTTP 生命周期、路由装配（自有认证中间件），
// Vue 产物挂在 /admin/ui/，业务 API 挂在 /admin/ops/api/。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/transport"
)

// shutdownTimeout 是 Web 管理端优雅退出的最长等待。
const shutdownTimeout = 5 * time.Second

// AdminOptions 是管理面服务的构造选项。
type AdminOptions struct {
	Version string
	// Bindings 是 uuid → 客户端名 的转发绑定表；nil 表示不提供转发配置。
	Bindings UserBindings
	// Plugins 是入站插件管理器；nil 表示管理页不展示插件管理。
	Plugins PluginRegistry
	// Listen 是数据面监听地址（host:port）：插件端口的默认建议值由它推导（+1）。
	Listen string
	// PublicHost 是分享链接里对外暴露的主机；空表示退回管理台请求的 Host。
	PublicHost string
	// Username/Password 是管理台登录账号与口令（启动时 bcrypt 哈希进内存，见 newAuthService）。
	Username string
	Password string
	// Store 是持久层：管理台从这里读流量表。
	Store TrafficReader
	// Mappings 是端口映射的控制面写口。
	Mappings MappingSettings
	// PortMaps 是端口映射监听管理器。
	PortMaps MappingStatus
	// LogPath 是服务端日志文件路径。
	LogPath string
	// Settings 是运行时服务配置的读写口。
	Settings ServiceSettings
	// Rules 是用户出站规则的读写口；nil 表示规则接口不可用。
	Rules RuleSettings
}

// AdminService 是 Web 管理面服务：会话认证 + Vue SPA + 业务 API。
type AdminService struct {
	streamStop   chan struct{}
	stopOnce     sync.Once
	requests     atomic.Int64
	stopping     atomic.Bool
	requestOnce  sync.Once
	requestDone  chan struct{}
	serveDone    chan struct{}
	authDone     <-chan struct{}
	metricsDone  chan struct{}
	metricsCache atomic.Pointer[metricsSnapshot]
	cancel       context.CancelFunc
	clients      *transport.SessionManager
	opts         AdminOptions
	started      time.Time
	srv          *http.Server
	auth         *AuthService
	// addr 为实际监听地址，Start 之后可用。
	addr string
}

// opsRoute 是业务路由的唯一声明处：method+path 注册路由。
// path 相对 /admin/ops，不含前缀。
type opsRoute struct {
	method string
	path   string
	handle http.HandlerFunc
}

// NewAdminService 创建管理面服务：内存凭证播种与路由装配。
func NewAdminService(addr string, clients *transport.SessionManager, opts AdminOptions) (*AdminService, error) {
	s := &AdminService{
		streamStop: make(chan struct{}),
		clients:    clients,
		opts:       opts,
		started:    time.Now(),
	}

	auth, err := newAuthService(opts)
	if err != nil {
		return nil, err
	}
	s.auth = auth

	mux := http.NewServeMux()

	// Vue 管理台（/admin/ui/）
	mountUI(mux)

	// 登录页（公开）
	mux.HandleFunc("GET /admin/auth/login", s.handleLoginPage)
	mux.HandleFunc("POST /admin/auth/login", s.handleLoginSubmit)
	mux.HandleFunc("POST /admin/auth/logout", s.handleLogout)

	// 业务 API（需登录 + CSRF）
	opsMux := http.NewServeMux()
	s.registerOpsRoutes(opsMux)
	mux.Handle("/admin/ops/", s.wrapOps(opsMux))

	// 根路径重定向到管理台
	mux.HandleFunc("GET /admin/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, uiPath, http.StatusFound)
	})
	mux.Handle("/", http.RedirectHandler("/admin/", http.StatusSeeOther))

	s.srv = &http.Server{
		Addr:              addr,
		Handler:           s.trackRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

func (s *AdminService) trackRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		defer func() {
			if s.requests.Add(-1) == 0 && s.stopping.Load() {
				s.requestOnce.Do(func() { close(s.requestDone) })
			}
		}()
		if s.stopping.Load() {
			http.Error(w, "admin service is shutting down", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// wrapOps 给 /admin/ops 下的路由套上全局中间件：auth 注入 → 会话加载 → 登录校验 → CSRF。
// 从内到外依次包裹，请求从外到内走：withAuth → session → requireAuth → csrf → handler。
func (s *AdminService) wrapOps(next http.Handler) http.Handler {
	h := csrfMiddleware(next)
	h = requireAuth(h)
	h = sessionMiddleware(h)
	h = withAuth(s.auth, h)
	return h
}

// registerOpsRoutes 注册所有业务 API 路由。
func (s *AdminService) registerOpsRoutes(mux *http.ServeMux) {
	for _, r := range s.opsRoutes() {
		mux.HandleFunc(r.method+" /admin/ops/"+r.path, r.handle)
	}
}

// opsRoutes 声明全部 JSON 接口。
func (s *AdminService) opsRoutes() []opsRoute {
	return []opsRoute{
		{"GET", "api/session", s.apiSession},
		{"GET", "api/status", s.apiStatus},
		{"GET", "api/metrics", s.apiMetrics},

		{"GET", "api/clients", s.apiClients},
		{"GET", "api/clients/{name}", s.apiClientDetail},
		{"PUT", "api/clients/{name}/config", s.apiClientConfigSave},
		{"PUT", "api/clients/{name}/hosts", s.apiClientHostsSave},
		{"POST", "api/clients/{id}/kick", s.apiClientKick},

		{"GET", "api/users", s.apiUsers},
		{"POST", "api/users", s.apiUserSave},
		{"DELETE", "api/users", s.apiUserDelete},
		{"POST", "api/users/status", s.apiUserStatus},
		{"PUT", "api/users/limits", s.apiUserLimits},

		{"GET", "api/portmaps", s.apiPortMaps},
		{"POST", "api/portmaps", s.apiPortMapSave},
		{"PUT", "api/portmaps/{id}", s.apiPortMapSave},
		{"POST", "api/portmaps/{id}/toggle", s.apiPortMapToggle},
		{"DELETE", "api/portmaps/{id}", s.apiPortMapDelete},

		{"GET", "api/plugins", s.apiPlugins},
		{"POST", "api/plugins/{name}/start", s.apiPluginStart},
		{"POST", "api/plugins/{name}/stop", s.apiPluginStop},
		{"PUT", "api/plugins/{name}/port", s.apiPluginPort},
		{"PUT", "api/plugins/{name}/tls", s.apiPluginTLS},

		{"GET", "api/keys", s.apiKeys},
		{"POST", "api/keys", s.apiKeySave},
		{"DELETE", "api/keys", s.apiKeyDelete},

		{"GET", "api/logs", s.apiLogs},
		{"GET", "api/logs/stream", s.apiLogStream},
		{"GET", "api/service-config", s.apiServiceConfig},
		{"PUT", "api/service-config", s.apiServiceConfigSave},
		{"GET", "api/client-config", s.apiGlobalClientConfig},
		{"PUT", "api/client-config", s.apiGlobalClientConfigSave},

		{"GET", "api/rules", s.apiRules},
		{"PUT", "api/rules/{name}", s.apiRuleSave},
		{"DELETE", "api/rules/{name}", s.apiRuleDelete},
	}
}

// Name 返回服务名。
func (s *AdminService) Name() string { return "AdminService" }

// Start 启动 HTTP 服务并立即返回；后台任务由 Drain 取消并等待。
func (s *AdminService) Start(ctx context.Context) error {
	if s.stopping.Load() || s.serveDone != nil {
		return errors.New("admin: service already started or stopped")
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("webadmin listen %s: %w", s.srv.Addr, err)
	}
	s.addr = ln.Addr().String()
	// HTTP 请求在优雅退出期间仍可完成，强制关闭时才统一取消。
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	s.srv.BaseContext = func(net.Listener) context.Context { return runCtx }
	done := make(chan struct{})
	s.serveDone = done
	s.metricsDone = make(chan struct{})
	routine.Go("admin.metrics.sample", func() {
		defer close(s.metricsDone)
		s.runMetrics(runCtx)
	})
	logging.Info("admin: web admin started", "addr", s.addr, "ui", uiPath)
	if s.auth != nil {
		s.authDone = s.auth.start(runCtx)
	}
	routine.Go("admin.service.serve", func() {
		defer close(done)
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Error("admin: web admin stopped", "err", err)
		}
	})
	return nil
}

// Drain 实现 extension.Drainer：优雅关闭管理端。
func (s *AdminService) Drain(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.requestDone = make(chan struct{})
		s.stopping.Store(true)
		if s.requests.Load() == 0 {
			s.requestOnce.Do(func() { close(s.requestDone) })
		}
		if s.streamStop != nil {
			close(s.streamStop)
		}
	})
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
	}
	var err error
	if s.srv != nil {
		err = s.srv.Shutdown(ctx)
		if err != nil {
			err = errors.Join(err, s.srv.Close())
		}
	}
	cancel := s.cancel
	if cancel != nil {
		cancel()
	}
	// Close 只关闭连接；独立收尾期限用于等待取消后的 handler 真正返回。
	waitCtx, waitCancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer waitCancel()
	return errors.Join(err, s.WaitDrained(waitCtx))
}

// WaitDrained 等待管理端请求与后台任务退出，供释放共享依赖前复核。
func (s *AdminService) WaitDrained(ctx context.Context) error {
	stopping := s.stopping.Load()
	requests, serving := s.requests.Load(), s.serveDone != nil
	var requestDone <-chan struct{}
	if stopping {
		requestDone = s.requestDone
	}
	waits := []struct {
		name string
		done <-chan struct{}
	}{
		{"requests", requestDone},
		{"serve", s.serveDone},
		{"auth cleanup", s.authDone},
		{"metrics sampler", s.metricsDone},
	}
	if !stopping && (requests != 0 || serving) {
		return errors.New("admin: drain has not started")
	}
	for _, wait := range waits {
		if wait.done == nil {
			continue
		}
		select {
		case <-wait.done:
			continue
		default:
		}
		select {
		case <-wait.done:
		case <-ctx.Done():
			return fmt.Errorf("admin: wait for %s: %w", wait.name, ctx.Err())
		}
	}
	return nil
}

// Addr 返回实际监听地址；未启动时返回配置地址。
func (s *AdminService) Addr() string {
	if s.addr != "" {
		return s.addr
	}
	return s.srv.Addr
}

// --- 登录/登出 ---

// handleLoginPage 处理 GET /admin/auth/login：重定向到 SPA 的登录页。
// 登录页是前端路由 /admin/ui/login，登录提交走 POST /admin/auth/login。
func (s *AdminService) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// 已登录直接进管理台
	if sess := sessionFromRequest(r); sess != "" {
		if _, ok := s.auth.SessionByToken(sess); ok {
			http.Redirect(w, r, uiPath, http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/admin/ui/login", http.StatusSeeOther)
}

type loginIn struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLoginSubmit 处理登录请求：JSON 格式，成功设置 cookie 并返回 CSRF。
func (s *AdminService) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	var in loginIn
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		if !apiDecode(w, r, &in) {
			return
		}
	} else {
		in.Username = strings.TrimSpace(r.FormValue("username"))
		in.Password = r.FormValue("password")
	}

	// 按 IP+用户名 限速：窗口内连续失败达到阈值后锁定一段时间
	key := limiterKey(r, in.Username)
	if ok, wait := s.auth.LoginAllowed(key); !ok {
		text := msg(r, msgTooManyAttempts, int(wait.Minutes())+1)
		if wantsJSON(r) {
			apiFail(w, http.StatusTooManyRequests, text)
		} else {
			http.Error(w, text, http.StatusTooManyRequests)
		}
		return
	}

	sessionToken, csrfToken, err := s.auth.Login(in.Username, in.Password)
	if err != nil {
		s.auth.LoginFailed(key)
		reason := loginErrKey(err)
		if wantsJSON(r) {
			apiFailKey(w, r, http.StatusUnauthorized, reason)
		} else {
			http.Error(w, msg(r, reason), http.StatusUnauthorized)
		}
		return
	}
	s.auth.LoginSucceeded(key)

	// 设置会话 cookie
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})

	if wantsJSON(r) {
		apiOK(w, map[string]any{"csrf": csrfToken})
	} else {
		http.Redirect(w, r, uiPath, http.StatusSeeOther)
	}
}

// handleLogout 处理登出。
func (s *AdminService) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := sessionFromRequest(r)
	if token != "" {
		s.auth.Logout(token)
	}
	http.SetCookie(w, &http.Cookie{
		Name:   sessionCookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	if wantsJSON(r) {
		apiOK(w, map[string]any{"ok": true})
	} else {
		http.Redirect(w, r, "/admin/auth/login", http.StatusSeeOther)
	}
}

// limiterKey 生成登录限速的键：远端 IP + 用户名。
func limiterKey(r *http.Request, username string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host + "\x00" + strings.ToLower(strings.TrimSpace(username))
}

// --- 共用业务方法 ---

// clientsSnapshot 从会话管理器组装客户端视图。
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

// bindings 返回转发绑定快照。
func (s *AdminService) bindings(ctx context.Context) []Binding {
	if s.opts.Bindings == nil {
		return nil
	}
	return s.opts.Bindings.Snapshot(ctx)
}

// serverPort 返回数据面监听端口。
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

// parseIPMaps 校验并归一化 IP 段映射行。
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

// parseCustomHosts 校验并归一化自定义 host 行。
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

// parseLogLevel 归一化日志级别。
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

// pushConfig 把服务端下发的全量配置推送给在线客户端。
func (s *AdminService) pushConfig(ctx context.Context, name string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	for _, e := range s.clients.ClientSessions() {
		if e.Info().ClientKey() != name {
			continue
		}
		if err := s.opts.Bindings.PushClientConfig(ctx, e); err != nil {
			logging.Warn("admin: push config failed", "client", name, "err", err)
			return false
		}
		return true
	}
	return false
}

// ClientInfo 是管理台展示的客户端视图。
type ClientInfo struct {
	ID            uint64              `json:"id"`
	Remote        string              `json:"remote"`
	ConnectedAt   time.Time           `json:"connected_at"`
	Client        protocol.ClientInfo `json:"client"`
	LastHeartbeat time.Time           `json:"last_heartbeat"`
	LastRTTMs     float64             `json:"last_rtt_ms"`
}
