package admin

// api.go 是 Vue 管理台消费的 JSON 接口（身份、客户端、用户）。
// 所有路由挂在 /admin/ops 下，会话与 CSRF 由中间件链负责。

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
)

// protocolNames 是管理台下拉用的协议清单。
func protocolNames() []string {
	return []string{
		string(store.ProtocolVMess),
		string(store.ProtocolVLESS),
		string(store.ProtocolVNV1),
		string(store.ProtocolSS),
	}
}

// --- 身份与总览 ---

type apiSessionDTO struct {
	User struct {
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"user"`
	CSRF string `json:"csrf"`
}

// apiSession 返回当前账号与 CSRF 令牌。
func (s *AdminService) apiSession(w http.ResponseWriter, r *http.Request) {
	sess := CtxSession(r)
	if sess == nil {
		apiFail(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var out apiSessionDTO
	out.User.Username = sess.Username
	out.User.Name = sess.Name
	out.CSRF = sess.CSRFToken
	apiOK(w, out)
}

type apiStatusDTO struct {
	Version       string `json:"version"`
	StartedAt     string `json:"started_at"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	ClientCount   int    `json:"client_count"`
	ActiveTunnels int64  `json:"active_tunnels"`
	TrafficBytes  int64  `json:"traffic_bytes"`
}

// apiStatus 是顶栏用的运行态快照。
func (s *AdminService) apiStatus(w http.ResponseWriter, r *http.Request) {
	traffic, active := transport.TunnelStats()
	apiOK(w, apiStatusDTO{
		Version:       s.opts.Version,
		StartedAt:     s.started.Format(time.RFC3339),
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		ClientCount:   s.clients.ClientCount(),
		ActiveTunnels: active,
		TrafficBytes:  traffic,
	})
}

// --- 客户端 ---

type apiIfaceDTO struct {
	Name  string   `json:"name"`
	MAC   string   `json:"mac"`
	Addrs []string `json:"addrs"`
}

type apiClientDTO struct {
	ID            uint64        `json:"id"`
	Name          string        `json:"name"`
	Online        bool          `json:"online"`
	Local         bool          `json:"local"`
	Version       string        `json:"version"`
	OS            string        `json:"os"`
	Arch          string        `json:"arch"`
	Hostname      string        `json:"hostname"`
	Remote        string        `json:"remote"`
	ConnectedAt   *time.Time    `json:"connected_at"`
	LastHeartbeat *time.Time    `json:"last_heartbeat"`
	RTTMs         float64       `json:"rtt_ms"`
	Outbounds     []string      `json:"outbounds"`
	Interfaces    []apiIfaceDTO `json:"interfaces"`
	UpBytes       int64         `json:"up_bytes"`
	DownBytes     int64         `json:"down_bytes"`
}

// apiClients 返回在线会话与已注册但离线客户端的合并列表。
func (s *AdminService) apiClients(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	_, clientTraffic := s.trafficMaps()

	rows := make([]apiClientDTO, 0, 16)
	online := make(map[string]bool, 16)
	for _, info := range s.clientsSnapshot() {
		if q != "" && !clientMatches(info, q) {
			continue
		}
		if platform != "" && info.Client.OS != platform {
			continue
		}
		row := apiClientDTO{
			ID:          info.ID,
			Name:        info.Client.Name,
			Online:      true,
			Remote:      info.Remote,
			Version:     info.Client.Version,
			OS:          info.Client.OS,
			Arch:        info.Client.Arch,
			Hostname:    info.Client.Hostname,
			ConnectedAt: timePtr(info.ConnectedAt),
			RTTMs:       info.LastRTTMs,
			Outbounds:   outboundTags(info.Client.Routing),
			Interfaces:  ifaceDTOs(info.Client.Interfaces),
		}
		if !info.LastHeartbeat.IsZero() {
			row.LastHeartbeat = timePtr(info.LastHeartbeat)
		}
		if info.Client.Name != "" {
			online[info.Client.Name] = true
		}
		if tr, ok := clientTraffic[row.Name]; ok {
			row.UpBytes, row.DownBytes = tr.Up, tr.Down
		}
		rows = append(rows, row)
	}

	// 已注册但此刻没接进来的客户端也要可见
	if s.opts.Bindings != nil {
		for _, cb := range s.opts.Bindings.ClientSnapshot() {
			if online[cb.ClientKey] || platform != "" {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(cb.ClientKey), q) {
				continue
			}
			row := apiClientDTO{
				Name:       cb.ClientKey,
				Local:      cb.ClientType == store.ClientTypeLocal,
				Outbounds:  []string{},
				Interfaces: []apiIfaceDTO{},
			}
			if tr, ok := clientTraffic[cb.ClientKey]; ok {
				row.UpBytes, row.DownBytes = tr.Up, tr.Down
			}
			rows = append(rows, row)
		}
	}

	platforms := make([]string, 0, 4)
	seen := make(map[string]bool, 4)
	for _, r := range rows {
		if r.OS != "" && !seen[r.OS] {
			seen[r.OS] = true
			platforms = append(platforms, r.OS)
		}
	}
	sort.Strings(platforms)

	apiOK(w, map[string]any{
		"clients":   rows,
		"total":     len(rows),
		"platforms": platforms,
	})
}

type apiClientDetailDTO struct {
	Name         string                `json:"name"`
	Online       bool                  `json:"online"`
	Remote       string                `json:"remote"`
	Local        bool                  `json:"local"`
	LogLevel     string                `json:"log_level"`
	ReportedJSON string                `json:"reported_json"`
	ServerConfig string                `json:"server_config"`
	StoredRaw    bool                  `json:"stored_raw"`
	HasStored    bool                  `json:"has_stored"`
	UpdatedAt    string                `json:"updated_at"`
	Hosts        []protocol.CustomHost `json:"hosts"`
	IPMaps       []protocol.IPMap      `json:"ip_maps"`
	Concurrency  int                   `json:"concurrency"`
}

// apiClientDetail 返回单个客户端的完整配置。
func (s *AdminService) apiClientDetail(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		apiFail(w, http.StatusBadRequest, "客户端名称不能为空")
		return
	}
	out := apiClientDetailDTO{
		Name:         name,
		ServerConfig: "{}",
		Hosts:        []protocol.CustomHost{},
		IPMaps:       []protocol.IPMap{},
	}
	for _, info := range s.clientsSnapshot() {
		if info.Client.Name != name {
			continue
		}
		out.Online, out.Remote = true, info.Remote
		if b, err := jsonMarshalIndent(info.Client); err == nil {
			out.ReportedJSON = string(b)
		}
		break
	}
	if s.opts.Bindings != nil {
		if cb, ok := s.opts.Bindings.LookupClient(name); ok {
			out.HasStored = true
			out.Local = cb.ClientType == store.ClientTypeLocal
			out.UpdatedAt = cb.UpdatedAt.Format("2006-01-02 15:04:05")
			switch {
			case cb.ServerConfig == "":
				out.ServerConfig = "{}"
			case jsonValid(cb.ServerConfig):
				out.ServerConfig = prettyJSON(cb.ServerConfig)
			default:
				out.ServerConfig, out.StoredRaw = cb.ServerConfig, true
			}
			out.LogLevel = store.ParseClientConfig(cb.ServerConfig).LogLevel
			if out.ReportedJSON == "" && cb.Config != "" {
				if jsonValid(cb.Config) {
					out.ReportedJSON = prettyJSON(cb.Config)
				} else {
					out.ReportedJSON = cb.Config
				}
			}
		}
		push := s.opts.Bindings.PushConfig(name)
		if push.CustomHosts != nil {
			out.Hosts = push.CustomHosts
		}
		if push.IPMaps != nil {
			out.IPMaps = push.IPMaps
		}
		out.Concurrency = s.opts.Bindings.ClientConcurrency(name)
	}
	apiOK(w, out)
}

type apiClientConfigIn struct {
	Config   string `json:"config"`
	LogLevel string `json:"log_level"`
}

// apiClientConfigSave 保存服务端下发的运维配置。
func (s *AdminService) apiClientConfigSave(w http.ResponseWriter, r *http.Request) {
	if s.opts.Bindings == nil {
		apiFail(w, http.StatusNotFound, "转发绑定表未配置")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		apiFail(w, http.StatusBadRequest, "客户端名称不能为空")
		return
	}
	var in apiClientConfigIn
	if !apiDecode(w, r, &in) {
		return
	}
	level, err := parseLogLevel(in.LogLevel)
	if err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	raw := strings.TrimSpace(in.Config)
	var obj map[string]any
	if raw != "" {
		if !jsonUnmarshalObj(raw, &obj) || obj == nil {
			apiFail(w, http.StatusBadRequest, "config 必须是 JSON 对象")
			return
		}
	}
	if obj == nil {
		obj = map[string]any{}
	}
	current, _ := obj["log_level"].(string)
	if norm, _ := parseLogLevel(current); norm != level {
		if level == "" {
			delete(obj, "log_level")
		} else {
			obj["log_level"] = level
		}
		merged, err := jsonMarshalIndent(obj)
		if err != nil {
			apiFail(w, http.StatusBadRequest, "config 必须是 JSON 对象")
			return
		}
		raw = string(merged)
	}
	cur, ok := s.opts.Bindings.LookupClient(name)
	if !ok {
		cur = ClientBinding{ClientKey: name, ClientType: store.ClientTypeProxy}
	}
	cur.ServerConfig = raw
	if err := s.opts.Bindings.SetClient(cur); err != nil {
		logging.Error("admin: client config save failed", "client", name, "err", err)
		apiFail(w, http.StatusInternalServerError, "保存失败：存储错误")
		return
	}
	logging.Info("admin: client config saved", "client", name, "bytes", len(raw), "actor", actorOf(r))
	apiOK(w, map[string]any{"name": name})
}

type apiHostRow struct {
	Domain string `json:"domain"`
	IP     string `json:"ip"`
}

type apiIPMapRow struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type apiClientHostsIn struct {
	Hosts       []apiHostRow  `json:"hosts"`
	IPMaps      []apiIPMapRow `json:"ip_maps"`
	Concurrency int           `json:"concurrency"`
}

// apiClientHostsSave 保存自定义 Host、IP 段映射与并发上限。
func (s *AdminService) apiClientHostsSave(w http.ResponseWriter, r *http.Request) {
	if s.opts.Bindings == nil {
		apiFail(w, http.StatusNotFound, "转发绑定表未配置")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		apiFail(w, http.StatusBadRequest, "客户端名称不能为空")
		return
	}
	var in apiClientHostsIn
	if !apiDecode(w, r, &in) {
		return
	}
	domains := make([]string, 0, len(in.Hosts))
	ips := make([]string, 0, len(in.Hosts))
	for _, h := range in.Hosts {
		domains = append(domains, h.Domain)
		ips = append(ips, h.IP)
	}
	hosts, err := parseCustomHosts(domains, ips)
	if err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	froms := make([]string, 0, len(in.IPMaps))
	tos := make([]string, 0, len(in.IPMaps))
	for _, m := range in.IPMaps {
		froms = append(froms, m.From)
		tos = append(tos, m.To)
	}
	ipMaps, err := parseIPMaps(froms, tos)
	if err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Concurrency < 0 {
		apiFail(w, http.StatusBadRequest, "并发上限必须是非负整数")
		return
	}

	cur, ok := s.opts.Bindings.LookupClient(name)
	if !ok {
		cur = ClientBinding{ClientKey: name, ClientType: store.ClientTypeProxy}
	}
	obj := map[string]any{}
	if cur.ServerConfig != "" {
		if !jsonUnmarshalObj(cur.ServerConfig, &obj) || obj == nil {
			apiFail(w, http.StatusBadRequest, "现有 server_config 不是合法 JSON，请先在配置页修正")
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
	if in.Concurrency == 0 {
		delete(obj, "concurrency")
	} else {
		obj["concurrency"] = in.Concurrency
	}
	merged, err := jsonMarshalIndent(obj)
	if err != nil {
		apiFail(w, http.StatusBadRequest, "server_config 不是合法 JSON 对象")
		return
	}
	cur.ServerConfig = string(merged)
	if err := s.opts.Bindings.SetClient(cur); err != nil {
		logging.Error("admin: client custom config save failed", "client", name, "err", err)
		apiFail(w, http.StatusInternalServerError, "保存失败：存储错误")
		return
	}
	logging.Info("admin: client custom config saved", "client", name,
		"custom_hosts", len(hosts), "ip_maps", len(ipMaps), "concurrency", in.Concurrency, "actor", actorOf(r))
	pushed := s.pushConfig(name, protocol.PushConfig{CustomHosts: hosts, IPMaps: ipMaps})
	if pushed {
		s.applyClientConcurrency(name, in.Concurrency)
	}
	apiOK(w, map[string]any{"name": name, "pushed": pushed})
}

// apiClientKick 断开一个在线客户端。
func (s *AdminService) apiClientKick(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id == 0 {
		apiFail(w, http.StatusBadRequest, "客户端编号不合法")
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
			apiFail(w, http.StatusInternalServerError, "断开失败: "+err.Error())
			return
		}
		logging.Info("admin: client kicked", "id", id, "name", name, "actor", actorOf(r))
		apiOK(w, map[string]any{"id": id})
		return
	}
	apiFail(w, http.StatusNotFound, "没有这个在线客户端")
}

// --- 用户 ---

type apiClientOption struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

type apiUserDTO struct {
	Protocol    string `json:"protocol"`
	UserKey     string `json:"user_key"`
	Name        string `json:"name"`
	Client      string `json:"client"`
	Enabled     bool   `json:"enabled"`
	LimitUpKB   int64  `json:"limit_up_kb"`
	LimitDownKB int64  `json:"limit_down_kb"`
	Concurrency int    `json:"concurrency"`
	LogLevel    string `json:"log_level"`
	UpBytes     int64  `json:"up_bytes"`
	DownBytes   int64  `json:"down_bytes"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	Shareable   bool   `json:"shareable"`
	ShareLink   string `json:"share_link"`
	// ShareLinkLegacy 是给旧内核（Xray < 26.2.6）的链接，仅在自签证书时给出。
	// 两代链接绝不能合并成一条：新内核见到 allowInsecure 会拒绝启动整个进程。
	ShareLinkLegacy string `json:"share_link_legacy,omitempty"`
}

// apiUsers 返回用户绑定列表以及新增用户需要的下拉候选。
func (s *AdminService) apiUsers(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	protocolFilter := strings.TrimSpace(r.URL.Query().Get("protocol"))

	bindings := s.bindings()
	sort.SliceStable(bindings, func(i, j int) bool {
		ti, tj := bindings[i].CreatedAt, bindings[j].CreatedAt
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		if bindings[i].Protocol != bindings[j].Protocol {
			return bindings[i].Protocol < bindings[j].Protocol
		}
		return bindings[i].UserKey < bindings[j].UserKey
	})
	userTraffic, _ := s.trafficMaps()
	shareHost := s.shareHost(r)
	vmessPort, vlessPort := s.v2rayPorts()
	vlessTLS := s.vlessShareTLS()

	rows := make([]apiUserDTO, 0, len(bindings))
	for _, b := range bindings {
		if protocolFilter != "" && string(b.Protocol) != protocolFilter {
			continue
		}
		if q != "" {
			hay := strings.ToLower(b.UserKey + "\x00" + b.Client + "\x00" + b.Name)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		row := apiUserDTO{
			Protocol:    string(b.Protocol),
			UserKey:     b.UserKey,
			Name:        b.Name,
			Client:      b.Client,
			Enabled:     b.Status == store.UserEnabled,
			LimitUpKB:   b.Config.UpLimit / 1024,
			LimitDownKB: b.Config.DownLimit / 1024,
			Concurrency: b.Config.Concurrency,
			LogLevel:    b.Config.LogLevel,
			CreatedAt:   b.CreatedAt.Format("2006-01-02 15:04:05"),
			UpdatedAt:   b.UpdatedAt.Format("2006-01-02 15:04:05"),
			Shareable:   b.Protocol.UsesUUID(),
		}
		if row.Shareable && shareHost != "" {
			target := shareTarget{Host: shareHost, Port: vmessPort}
			if b.Protocol == store.ProtocolVMess {
				row.ShareLink = linkForPort(target, b.UserKey, true)
			} else {
				target.Port, target.TLS = vlessPort, vlessTLS
				row.ShareLink = linkForPort(target, b.UserKey, false)
				// 只有自签证书才需要旧内核的兼容链接：受信证书两代客户端都直连通过。
				if vlessPort > 0 && vlessTLS != nil && vlessTLS.SelfSigned {
					row.ShareLinkLegacy = vlessLinkLegacy(target, b.UserKey, shareName(b.UserKey))
				}
			}
		}
		if tr, ok := userTraffic[userTrafficKey(b.Protocol, b.UserKey)]; ok {
			row.UpBytes, row.DownBytes = tr.Up, tr.Down
		}
		rows = append(rows, row)
	}

	apiOK(w, map[string]any{
		"users":     rows,
		"total":     len(rows),
		"clients":   s.clientOptions(),
		"protocols": protocolNames(),
	})
}

type apiUserSaveIn struct {
	Protocol string `json:"protocol"`
	UserKey  string `json:"user_key"`
	Name     string `json:"name"`
	Client   string `json:"client"`
	Enabled  *bool  `json:"enabled"`
}

// apiUserSave 登记或覆盖一条用户绑定。
func (s *AdminService) apiUserSave(w http.ResponseWriter, r *http.Request) {
	if s.opts.Bindings == nil {
		apiFail(w, http.StatusNotFound, "转发绑定表未配置")
		return
	}
	var in apiUserSaveIn
	if !apiDecode(w, r, &in) {
		return
	}
	p := store.Protocol(strings.TrimSpace(in.Protocol))
	if p == "" {
		p = store.ProtocolVNV1
	}
	key := strings.TrimSpace(in.UserKey)
	client := strings.TrimSpace(in.Client)
	name := strings.TrimSpace(in.Name)
	if key == "" || !p.Valid() {
		apiFail(w, http.StatusBadRequest, "协议与 user_key 都必须填写")
		return
	}
	if client == "" || name == "" {
		apiFail(w, http.StatusBadRequest, "名称与出口客户端都必须填写")
		return
	}
	if err := plugin.ValidateUserKey(p, key); err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	cur, ok := s.opts.Bindings.Lookup(p, key)
	if !ok {
		cur = Binding{UserKey: key, Protocol: p}
	}
	cur.Name = name
	cur.Client = client
	cur.Status = store.UserEnabled
	if in.Enabled != nil && !*in.Enabled {
		cur.Status = store.UserDisabled
	}
	if err := s.opts.Bindings.SetUser(cur); err != nil {
		logging.Error("admin: user binding save failed", "protocol", p, "user_key", key, "err", err)
		apiFail(w, http.StatusInternalServerError, "保存失败：存储错误")
		return
	}
	logging.Info("admin: user binding saved",
		"protocol", p, "user_key", key, "name", name, "client", client, "actor", actorOf(r))
	apiOK(w, map[string]any{"protocol": p, "user_key": key})
}

// apiUserDelete 删除一条用户绑定。
func (s *AdminService) apiUserDelete(w http.ResponseWriter, r *http.Request) {
	p, key, ok := bindingQuery(r)
	if !ok {
		return
	}
	deleted, err := s.opts.Bindings.DeleteUser(p, key)
	if err != nil {
		logging.Error("admin: user binding delete failed", "protocol", p, "user_key", key, "err", err)
		apiFail(w, http.StatusInternalServerError, "删除失败：存储错误")
		return
	}
	logging.Info("admin: user binding deleted", "protocol", p, "user_key", key, "existed", deleted, "actor", actorOf(r))
	apiOK(w, map[string]any{"deleted": deleted})
}

// apiUserStatus 启用/停用一条绑定。
func (s *AdminService) apiUserStatus(w http.ResponseWriter, r *http.Request) {
	p, key, ok := bindingQuery(r)
	if !ok {
		return
	}
	raw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("enabled")))
	if raw != "true" && raw != "false" && raw != "1" && raw != "0" {
		apiFail(w, http.StatusBadRequest, "enabled 必须是 true 或 false")
		return
	}
	status := store.UserDisabled
	if raw == "true" || raw == "1" {
		status = store.UserEnabled
	}
	cur, ok := s.opts.Bindings.Lookup(p, key)
	if !ok {
		apiFail(w, http.StatusNotFound, "用户未登记")
		return
	}
	cur.Status = status
	if err := s.opts.Bindings.SetUser(cur); err != nil {
		logging.Error("admin: user status update failed", "protocol", p, "user_key", key, "err", err)
		apiFail(w, http.StatusInternalServerError, "保存失败：存储错误")
		return
	}
	logging.Info("admin: user status updated", "protocol", p, "user_key", key, "status", status, "actor", actorOf(r))
	apiOK(w, map[string]any{"enabled": status == store.UserEnabled})
}

type apiUserLimitsIn struct {
	UpKB        int64  `json:"up_kbps"`
	DownKB      int64  `json:"down_kbps"`
	Concurrency int    `json:"concurrency"`
	LogLevel    string `json:"log_level"`
}

// apiUserLimits 保存用户级限速、并发与日志级别。
func (s *AdminService) apiUserLimits(w http.ResponseWriter, r *http.Request) {
	p, key, ok := bindingQuery(r)
	if !ok {
		return
	}
	var in apiUserLimitsIn
	if !apiDecode(w, r, &in) {
		return
	}
	if in.UpKB < 0 || in.DownKB < 0 {
		apiFail(w, http.StatusBadRequest, "限速必须是非负整数")
		return
	}
	if in.Concurrency < 0 {
		apiFail(w, http.StatusBadRequest, "并发上限必须是非负整数")
		return
	}
	level, err := parseLogLevel(in.LogLevel)
	if err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	cur, ok := s.opts.Bindings.Lookup(p, key)
	if !ok {
		apiFail(w, http.StatusNotFound, "用户未登记")
		return
	}
	cur.Config = store.UserConfig{
		UpLimit:     in.UpKB * 1024,
		DownLimit:   in.DownKB * 1024,
		LogLevel:    level,
		Concurrency: in.Concurrency,
	}
	if err := s.opts.Bindings.SetUser(cur); err != nil {
		logging.Error("admin: user config save failed", "protocol", p, "user_key", key, "err", err)
		apiFail(w, http.StatusInternalServerError, "保存失败：存储错误")
		return
	}
	logging.Info("admin: user config saved", "protocol", p, "user_key", key,
		"up_kbps", in.UpKB, "down_kbps", in.DownKB, "concurrency", in.Concurrency,
		"log_level", level, "actor", actorOf(r))
	apiOK(w, map[string]any{"protocol": p, "user_key": key})
}

// bindingQuery 从查询串取用户身份。
func bindingQuery(r *http.Request) (store.Protocol, string, bool) {
	q := r.URL.Query()
	p := store.Protocol(strings.TrimSpace(q.Get("protocol")))
	if p == "" {
		p = store.ProtocolVNV1
	}
	key := strings.TrimSpace(q.Get("key"))
	if key == "" || !p.Valid() {
		return "", "", false
	}
	return p, key, true
}

// --- 小工具 ---

func timePtr(t time.Time) *time.Time { return &t }

// outboundTags 取出路由出站标签。
func outboundTags(routing *protocol.ClientRouting) []string {
	if routing == nil {
		return []string{}
	}
	tags := make([]string, 0, len(routing.Outbounds))
	for _, o := range routing.Outbounds {
		tags = append(tags, o.Tag)
	}
	return tags
}

// ifaceDTOs 把网卡快照转成前端结构。
func ifaceDTOs(ifs []protocol.NetInterface) []apiIfaceDTO {
	out := make([]apiIfaceDTO, 0, len(ifs))
	for _, i := range ifs {
		addrs := i.Addrs
		if addrs == nil {
			addrs = []string{}
		}
		out = append(out, apiIfaceDTO{Name: i.Name, MAC: i.MAC, Addrs: addrs})
	}
	return out
}

// jsonMarshalIndent 是 json.MarshalIndent 的简单包装。
func jsonMarshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// jsonValid 检查字符串是否是合法 JSON 对象。
func jsonValid(s string) bool {
	var m map[string]any
	return jsonUnmarshalObj(s, &m)
}

// jsonUnmarshalObj 解析 JSON 对象到 map。
func jsonUnmarshalObj(s string, out *map[string]any) bool {
	if s == "" {
		return false
	}
	if err := json.Unmarshal([]byte(s), out); err != nil || *out == nil {
		return false
	}
	return true
}
