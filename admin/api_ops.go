package admin

// api_ops.go 是 Vue 管理台消费的运维类接口：端口映射、插件、认证密钥、日志与
// 运行期服务配置。权限由中间件按路由判定。

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/store"
)

// --- 端口映射 ---

type apiPortMapDTO struct {
	ID           uint64 `json:"id"`
	Client       string `json:"client"`
	RemotePort   int    `json:"remote_port"`
	LocalHost    string `json:"local_host"`
	LocalPort    int    `json:"local_port"`
	LimitUpKB    int64  `json:"limit_up_kb"`
	LimitDownKB  int64  `json:"limit_down_kb"`
	Enabled      bool   `json:"enabled"`
	Remark       string `json:"remark"`
	Listening    bool   `json:"listening"`
	ClientOnline bool   `json:"client_online"`
	UpdatedAt    string `json:"updated_at"`
}

// apiPortMaps 返回映射规则（带运行状态）、可绑定客户端与对外主机提示。
func (s *AdminService) apiPortMaps(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"rows":        []apiPortMapDTO{},
		"clients":     s.clientOptionsForPortMap(),
		"public_host": s.shareHost(r),
	}
	if s.opts.Store == nil {
		apiOK(w, out)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	rows, err := s.opts.Store.ListPortMappings(ctx)
	if err != nil {
		logging.Warnf("admin: list port mappings: %v", err)
	}
	online := make(map[string]bool)
	for _, info := range s.clientsSnapshot() {
		if info.Client.Name != "" {
			online[info.Client.Name] = true
		}
	}
	var listening map[uint64]bool
	if s.opts.PortMaps != nil {
		listening = s.opts.PortMaps.ListeningIDs()
	}
	items := make([]apiPortMapDTO, 0, len(rows))
	for _, m := range rows {
		items = append(items, apiPortMapDTO{
			ID:           m.ID,
			Client:       m.Client,
			RemotePort:   m.RemotePort,
			LocalHost:    m.LocalHost,
			LocalPort:    m.LocalPort,
			LimitUpKB:    m.Config.UpLimit / 1024,
			LimitDownKB:  m.Config.DownLimit / 1024,
			Enabled:      m.Enabled,
			Remark:       m.Remark,
			Listening:    listening[m.ID],
			ClientOnline: online[m.Client],
			UpdatedAt:    m.UpdatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	out["rows"] = items
	apiOK(w, out)
}

type apiPortMapIn struct {
	Client     string `json:"client"`
	RemotePort int    `json:"remote_port"`
	LocalHost  string `json:"local_host"`
	LocalPort  int    `json:"local_port"`
	UpKB       int64  `json:"limit_up_kb"`
	DownKB     int64  `json:"limit_down_kb"`
	Remark     string `json:"remark"`
	Enabled    bool   `json:"enabled"`
}

// apiPortMapSave 新增（POST /portmaps）或更新（PUT /portmaps/{id}）一条映射。
func (s *AdminService) apiPortMapSave(w http.ResponseWriter, r *http.Request) {
	if s.opts.Store == nil {
		apiFail(w, http.StatusNotFound, "端口映射未配置")
		return
	}
	var in apiPortMapIn
	if !apiDecode(w, r, &in) {
		return
	}
	id, _ := strconv.ParseUint(strings.TrimSpace(r.PathValue("id")), 10, 64)
	in.Client = strings.TrimSpace(in.Client)
	in.LocalHost = strings.TrimSpace(in.LocalHost)
	in.Remark = strings.TrimSpace(in.Remark)
	if in.Client == "" || in.LocalHost == "" {
		apiFail(w, http.StatusBadRequest, "客户端与本地主机都必须填写")
		return
	}
	if in.RemotePort < 1 || in.RemotePort > 65535 || in.LocalPort < 1 || in.LocalPort > 65535 {
		apiFail(w, http.StatusBadRequest, "端口必须在 1-65535 之间")
		return
	}
	if in.UpKB < 0 || in.DownKB < 0 {
		apiFail(w, http.StatusBadRequest, "限速必须是非负整数")
		return
	}
	m := store.PortMapping{
		ID:         id,
		Client:     in.Client,
		RemotePort: in.RemotePort,
		LocalHost:  in.LocalHost,
		LocalPort:  in.LocalPort,
		Config:     store.PortConfig{UpLimit: in.UpKB * 1024, DownLimit: in.DownKB * 1024},
		Enabled:    in.Enabled,
		Remark:     in.Remark,
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if err := s.opts.Store.SetPortMapping(ctx, m); err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	logging.Info("admin: port mapping saved", "id", id, "client", m.Client,
		"remote_port", m.RemotePort, "local", fmt.Sprintf("%s:%d", m.LocalHost, m.LocalPort),
		"actor", actorOf(r))
	s.syncPortMaps()
	apiOK(w, map[string]any{"id": m.ID})
}

// apiPortMapDelete 删除一条映射。
func (s *AdminService) apiPortMapDelete(w http.ResponseWriter, r *http.Request) {
	if s.opts.Store == nil {
		apiFail(w, http.StatusNotFound, "端口映射未配置")
		return
	}
	id, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id == 0 {
		apiFail(w, http.StatusBadRequest, "映射编号不合法")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	deleted, err := s.opts.Store.DeletePortMapping(ctx, id)
	if err != nil {
		logging.Error("admin: port mapping delete failed", "id", id, "err", err)
		apiFail(w, http.StatusInternalServerError, "删除失败：存储错误")
		return
	}
	logging.Info("admin: port mapping deleted", "id", id, "existed", deleted, "actor", actorOf(r))
	s.syncPortMaps()
	apiOK(w, map[string]any{"deleted": deleted})
}

// apiPortMapToggle 翻转启用状态。
func (s *AdminService) apiPortMapToggle(w http.ResponseWriter, r *http.Request) {
	if s.opts.Store == nil {
		apiFail(w, http.StatusNotFound, "端口映射未配置")
		return
	}
	id, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id == 0 {
		apiFail(w, http.StatusBadRequest, "映射编号不合法")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	m, ok, err := s.opts.Store.GetPortMapping(ctx, id)
	switch {
	case err != nil:
		apiFail(w, http.StatusInternalServerError, "读取失败：存储错误")
		return
	case !ok:
		apiFail(w, http.StatusNotFound, "没有这条映射")
		return
	}
	m.Enabled = !m.Enabled
	if err := s.opts.Store.SetPortMapping(ctx, m); err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	logging.Info("admin: port mapping toggled", "id", id, "enabled", m.Enabled, "actor", actorOf(r))
	s.syncPortMaps()
	apiOK(w, map[string]any{"enabled": m.Enabled})
}

// --- 插件 ---

type apiPortSlot struct {
	Protocol string `json:"protocol"`
	Label    string `json:"label"`
	Field    string `json:"field"`
	Value    int    `json:"value"`
}

type apiPluginDTO struct {
	Name         string            `json:"name"`
	Enabled      bool              `json:"enabled"`
	Ports        []plugin.PortKV   `json:"ports"`
	Params       map[string]string `json:"params"`
	Slots        []apiPortSlot     `json:"slots"`
	AcceptsUsers bool              `json:"accepts_users"`
	TLS          *plugin.TLSInfo   `json:"tls,omitempty"`
}

// apiPlugins 返回插件状态、端口槽位与是否托管账号。
func (s *AdminService) apiPlugins(w http.ResponseWriter, r *http.Request) {
	serverPort := s.serverPort()
	items := make([]apiPluginDTO, 0, 4)
	for _, st := range s.pluginsList() {
		dto := apiPluginDTO{
			Name:    st.Name,
			Enabled: st.Enabled,
			Ports:   st.Ports,
			Params:  st.Params,
			Slots:   portSlots(st.Ports, serverPort),
			TLS:     st.TLS,
		}
		if dto.Ports == nil {
			dto.Ports = []plugin.PortKV{}
		}
		if dto.Params == nil {
			dto.Params = map[string]string{}
		}
		if s.opts.Plugins != nil {
			if p, err := s.opts.Plugins.Get(st.Name); err == nil {
				if _, ok := p.(plugin.UserSyncer); ok {
					dto.AcceptsUsers = true
				}
			}
		}
		items = append(items, dto)
	}
	apiOK(w, map[string]any{"plugins": items, "server_port": serverPort})
}

type apiPluginIn struct {
	Params map[string]string `json:"params"`
}

// apiPluginStart 启用插件。
func (s *AdminService) apiPluginStart(w http.ResponseWriter, r *http.Request) {
	s.apiPluginAction(w, r, "start")
}

// apiPluginStop 停用插件。
func (s *AdminService) apiPluginStop(w http.ResponseWriter, r *http.Request) {
	s.apiPluginAction(w, r, "stop")
}

// apiPluginPort 运行中热切换端口。
func (s *AdminService) apiPluginPort(w http.ResponseWriter, r *http.Request) {
	s.apiPluginAction(w, r, "port")
}

// apiPluginTLS 运行中切换 TLS（目前只有 vless 入站支持）。
func (s *AdminService) apiPluginTLS(w http.ResponseWriter, r *http.Request) {
	s.apiPluginAction(w, r, "tls")
}

func (s *AdminService) apiPluginAction(w http.ResponseWriter, r *http.Request, action string) {
	if s.opts.Plugins == nil {
		apiFail(w, http.StatusNotFound, "插件管理器未配置")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		apiFail(w, http.StatusBadRequest, "插件名不能为空")
		return
	}
	p, err := s.opts.Plugins.Get(name)
	if err != nil {
		apiFail(w, http.StatusNotFound, err.Error())
		return
	}
	var in apiPluginIn
	if action != "stop" {
		if !apiDecode(w, r, &in) {
			return
		}
	}
	ctx := r.Context()
	switch action {
	case "start":
		if err := p.Enable(ctx, in.Params); err != nil {
			logging.Error("admin: plugin enable failed", "plugin", name, "params", in.Params, "err", err)
			apiFail(w, http.StatusBadRequest, "启用失败: "+err.Error())
			return
		}
		logging.Info("admin: plugin enabled", "plugin", name, "params", in.Params, "actor", actorOf(r))
	case "stop":
		if err := p.Disable(ctx); err != nil {
			logging.Error("admin: plugin disable failed", "plugin", name, "err", err)
			apiFail(w, http.StatusInternalServerError, "停用失败：存储错误")
			return
		}
		logging.Info("admin: plugin disabled", "plugin", name, "actor", actorOf(r))
	case "port":
		ports, err := portsFromParams(in.Params)
		if err != nil {
			apiFail(w, http.StatusBadRequest, err.Error())
			return
		}
		if ms, ok := p.(plugin.MultiPortSetter); ok {
			if err := ms.SetPorts(ctx, ports); err != nil {
				logging.Error("admin: plugin set ports failed", "plugin", name, "ports", ports, "err", err)
				apiFail(w, http.StatusBadRequest, "改端口失败: "+err.Error())
				return
			}
			logging.Info("admin: plugin ports updated", "plugin", name, "ports", ports, "actor", actorOf(r))
			break
		}
		setter, ok := p.(plugin.PortSetter)
		if !ok {
			apiFail(w, http.StatusBadRequest, "该插件不支持运行中改端口")
			return
		}
		port := 0
		for _, v := range ports {
			port = v
		}
		if port == 0 {
			raw, _ := strconv.Atoi(strings.TrimSpace(in.Params["port"]))
			port = raw
		}
		if port <= 0 || port >= 65536 {
			apiFail(w, http.StatusBadRequest, "端口必须是 1..65535 的整数")
			return
		}
		if err := setter.SetPort(ctx, port); err != nil {
			logging.Error("admin: plugin set port failed", "plugin", name, "port", port, "err", err)
			apiFail(w, http.StatusBadRequest, "改端口失败: "+err.Error())
			return
		}
		logging.Info("admin: plugin port updated", "plugin", name, "port", port, "actor", actorOf(r))
	case "tls":
		setter, ok := p.(plugin.TLSSetter)
		if !ok {
			apiFail(w, http.StatusBadRequest, "该插件不支持 TLS")
			return
		}
		enabled, err := parseOnOff(in.Params["enabled"])
		if err != nil {
			apiFail(w, http.StatusBadRequest, err.Error())
			return
		}
		sni := strings.TrimSpace(in.Params["sni"])
		if err := setter.SetTLS(ctx, enabled, sni); err != nil {
			logging.Error("admin: plugin set tls failed", "plugin", name, "enabled", enabled, "sni", sni, "err", err)
			apiFail(w, http.StatusBadRequest, "配置 TLS 失败: "+err.Error())
			return
		}
		logging.Info("admin: plugin tls updated", "plugin", name, "enabled", enabled, "sni", sni, "actor", actorOf(r))
	default:
		apiFail(w, http.StatusBadRequest, "不支持的动作")
		return
	}
	apiOK(w, map[string]any{"plugin": name, "action": action})
}

// portsFromParams 从参数里解析 <协议>_port 字段。
func portsFromParams(params map[string]string) (map[store.Protocol]int, error) {
	out := make(map[store.Protocol]int, 2)
	for k, raw := range params {
		name, ok := strings.CutSuffix(k, "_port")
		if !ok {
			continue
		}
		p := store.Protocol(name)
		if !p.Valid() {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || port <= 0 || port >= 65536 {
			return nil, fmt.Errorf("%s 必须是 1..65535 的整数", k)
		}
		out[p] = port
	}
	return out, nil
}

// parseOnOff 解析管理台开关字段：接受 on/off、true/false、1/0，空串视为关闭；其余
// 值报错而不是静默取默认——配置写错要让人看见。
func parseOnOff(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "on", "true", "1":
		return true, nil
	case "off", "false", "0", "":
		return false, nil
	}
	return false, fmt.Errorf("开关值 %q 无法识别（用 on/off）", raw)
}

// --- 认证密钥 ---

type apiKeyDTO struct {
	Key       string `json:"key"`
	Scope     string `json:"scope"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// apiKeys 返回认证密钥列表。
func (s *AdminService) apiKeys(w http.ResponseWriter, r *http.Request) {
	rows := []apiKeyDTO{}
	if s.opts.Bindings != nil {
		for _, k := range s.opts.Bindings.AuthKeySnapshot() {
			rows = append(rows, apiKeyDTO{
				Key:       k.Key,
				Scope:     k.Scope,
				CreatedAt: k.CreatedAt.Format("2006-01-02 15:04:05"),
				UpdatedAt: k.UpdatedAt.Format("2006-01-02 15:04:05"),
			})
		}
	}
	apiOK(w, map[string]any{"keys": rows})
}

type apiKeyIn struct {
	Key   string `json:"key"`
	Scope string `json:"scope"`
}

// apiKeySave 按密钥 upsert。
func (s *AdminService) apiKeySave(w http.ResponseWriter, r *http.Request) {
	if s.opts.Bindings == nil {
		apiFail(w, http.StatusNotFound, "认证密钥未配置")
		return
	}
	var in apiKeyIn
	if !apiDecode(w, r, &in) {
		return
	}
	key := strings.TrimSpace(in.Key)
	scope := strings.TrimSpace(in.Scope)
	if err := s.opts.Bindings.SetAuthKey(key, scope); err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	logging.Info("admin: auth key saved", "scope", scope, "actor", actorOf(r))
	apiOK(w, map[string]any{"key": key, "scope": scope})
}

// apiKeyDelete 按密钥删除。
func (s *AdminService) apiKeyDelete(w http.ResponseWriter, r *http.Request) {
	if s.opts.Bindings == nil {
		apiFail(w, http.StatusNotFound, "认证密钥未配置")
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		apiFail(w, http.StatusBadRequest, "key 不能为空")
		return
	}
	deleted, err := s.opts.Bindings.DeleteAuthKey(key)
	if err != nil {
		logging.Error("admin: auth key delete failed", "err", err)
		apiFail(w, http.StatusInternalServerError, "删除失败：存储错误")
		return
	}
	logging.Info("admin: auth key deleted", "existed", deleted, "actor", actorOf(r))
	apiOK(w, map[string]any{"deleted": deleted})
}

// --- 日志 ---

// apiLogs 返回日志尾部（按关键字过滤）。
func (s *AdminService) apiLogs(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	lines := queryInt(r.URL.Query(), "lines", 300)
	if lines > 2000 {
		lines = 2000
	}
	out := map[string]any{"path": s.opts.LogPath, "query": query, "lines": []string{}, "truncated": false}
	if s.opts.LogPath == "" {
		out["lines"] = []string{"日志输出到 stderr，未写文件；在配置 log.output 设置文件路径后可在此查看。"}
		apiOK(w, out)
		return
	}
	tail, truncated := tailFile(s.opts.LogPath, 256*1024, lines, query)
	out["lines"], out["truncated"] = tail, truncated
	apiOK(w, out)
}

// intPtr 把整数包成指针，用于"缺省表示保持现值"的可选字段。
func intPtr(n int) *int { return &n }

// queryInt 读取非负整数查询参数；缺失或非法返回 def。
func queryInt(raw url.Values, key string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw.Get(key)))
	if err != nil || n < 1 {
		return def
	}
	return n
}

// --- 运行期服务配置 ---

type apiDurationFields struct {
	HeartbeatInterval    string `json:"heartbeat_interval"`
	HeartbeatTimeout     string `json:"heartbeat_timeout"`
	TrafficFlushInterval string `json:"traffic_flush_interval"`
	KeepAliveInterval    string `json:"keepalive_interval"`
	KeepAliveTimeout     string `json:"keepalive_timeout"`
	StreamOpenTimeout    string `json:"stream_open_timeout"`
	MaxStreamWindowKB    *int   `json:"max_stream_window_kb"`
	AcceptBacklog        *int   `json:"accept_backlog"`
	LogRotateMaxSizeMB   *int   `json:"log_rotate_max_size_mb"`
	LogRotateMaxBackups  *int   `json:"log_rotate_max_backups"`
	LogRotateMaxAgeDays  *int   `json:"log_rotate_max_age_days"`
	PublicHost           string `json:"public_host"`
	HeartbeatEnabled     bool   `json:"heartbeat_enabled"`
	LogLevel             string `json:"log_level"`
	ServerProxyEnabled   bool   `json:"server_proxy_enabled"`
	ServerProxyPublic    string `json:"server_proxy_public"`
	ServerProxyPrivate   string `json:"server_proxy_private"`
	ServerProxyRouting   string `json:"server_proxy_routing"`
	LogToFile            bool   `json:"log_to_file"`
}

// apiServiceConfig 返回运行期可在线修改的服务端配置。
func (s *AdminService) apiServiceConfig(w http.ResponseWriter, r *http.Request) {
	cur := s.currentServiceSettings()
	routingText, err := config.EncodeRoutingText(cur.ServerProxy.Routing)
	if err != nil {
		routingText = ""
	}
	apiOK(w, apiDurationFields{
		PublicHost:           cur.PublicHost,
		HeartbeatEnabled:     cur.Heartbeat.Enabled,
		HeartbeatInterval:    cur.Heartbeat.Interval.String(),
		HeartbeatTimeout:     cur.Heartbeat.Timeout.String(),
		LogLevel:             cur.LogLevel,
		TrafficFlushInterval: cur.TrafficFlushInterval.String(),
		KeepAliveInterval:    cur.Session.KeepAliveInterval.String(),
		KeepAliveTimeout:     cur.Session.KeepAliveTimeout.String(),
		MaxStreamWindowKB:    intPtr(cur.Session.MaxStreamWindow / 1024),
		AcceptBacklog:        intPtr(cur.Session.AcceptBacklog),
		StreamOpenTimeout:    cur.Session.StreamOpenTimeout.String(),
		LogRotateMaxSizeMB:   intPtr(cur.LogRotation.MaxSizeMB),
		LogRotateMaxBackups:  intPtr(cur.LogRotation.MaxBackups),
		LogRotateMaxAgeDays:  intPtr(cur.LogRotation.MaxAgeDays),
		ServerProxyEnabled:   cur.ServerProxy.Enabled,
		ServerProxyPublic:    cur.ServerProxy.Public,
		ServerProxyPrivate:   cur.ServerProxy.Private,
		ServerProxyRouting:   routingText,
		LogToFile:            s.opts.LogPath != "",
	})
}

// apiServiceConfigSave 校验并落库，成功后热分发到运行态。
func (s *AdminService) apiServiceConfigSave(w http.ResponseWriter, r *http.Request) {
	if s.opts.Settings == nil {
		apiFail(w, http.StatusServiceUnavailable, "运行时配置未接入")
		return
	}
	var in apiDurationFields
	if !apiDecode(w, r, &in) {
		return
	}
	cur := s.currentServiceSettings()
	next := cur
	next.PublicHost = strings.TrimSpace(in.PublicHost)
	next.Heartbeat.Enabled = in.HeartbeatEnabled
	next.ServerProxy.Enabled = in.ServerProxyEnabled
	next.ServerProxy.Public = strings.TrimSpace(in.ServerProxyPublic)
	next.ServerProxy.Private = strings.TrimSpace(in.ServerProxyPrivate)
	if strings.TrimSpace(in.ServerProxyRouting) != "" {
		rc, err := config.DecodeRoutingText(in.ServerProxyRouting)
		if err != nil {
			apiFail(w, http.StatusBadRequest, err.Error())
			return
		}
		next.ServerProxy.Routing = rc
	}
	if lv := strings.ToLower(strings.TrimSpace(in.LogLevel)); lv != "" {
		next.LogLevel = lv
	}

	var err error
	if next.Heartbeat.Interval, err = parseFormDuration("heartbeat_interval", in.HeartbeatInterval, cur.Heartbeat.Interval); err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	if next.Heartbeat.Timeout, err = parseFormDuration("heartbeat_timeout", in.HeartbeatTimeout, cur.Heartbeat.Timeout); err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	if next.TrafficFlushInterval, err = parseFormDuration("traffic_flush_interval", in.TrafficFlushInterval, cur.TrafficFlushInterval); err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	if next.Session.KeepAliveInterval, err = parseFormDuration("keepalive_interval", in.KeepAliveInterval, cur.Session.KeepAliveInterval); err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	if next.Session.KeepAliveTimeout, err = parseFormDuration("keepalive_timeout", in.KeepAliveTimeout, cur.Session.KeepAliveTimeout); err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	if next.Session.StreamOpenTimeout, err = parseFormDuration("stream_open_timeout", in.StreamOpenTimeout, cur.Session.StreamOpenTimeout); err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.MaxStreamWindowKB != nil {
		if *in.MaxStreamWindowKB <= 0 {
			next.Session.MaxStreamWindow = 0
		} else {
			next.Session.MaxStreamWindow = *in.MaxStreamWindowKB * 1024
		}
	}
	if in.AcceptBacklog != nil {
		next.Session.AcceptBacklog = *in.AcceptBacklog
	}
	if in.LogRotateMaxSizeMB != nil {
		next.LogRotation.MaxSizeMB = *in.LogRotateMaxSizeMB
	}
	if in.LogRotateMaxBackups != nil {
		next.LogRotation.MaxBackups = *in.LogRotateMaxBackups
	}
	if in.LogRotateMaxAgeDays != nil {
		next.LogRotation.MaxAgeDays = *in.LogRotateMaxAgeDays
	}

	if err := next.Validate(); err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.opts.Settings.Apply(next); err != nil {
		logging.Error("admin: service config apply failed", "err", err)
		apiFail(w, http.StatusBadRequest, "保存失败: "+err.Error())
		return
	}
	logging.Info("admin: service config saved", "actor", actorOf(r))
	apiOK(w, map[string]any{"saved": true})
}
