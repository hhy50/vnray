package admin

// pages.go 是挂在 steward 里的业务页：客户端管理、用户管理、插件管理、日志查看
// 与客户端配置详情。页面渲染走 Context.Render（steward 布局 + overlay 模板），
// HTML 模板在 templates/pages/ 下。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/imfiqhan/steward"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
)

// humanBytes 把字节数格式化成可读文本。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// humanRate 把 KB/s 限速值格式化成可读文本；0 = 不限速。
func humanRate(kb int64) string {
	if kb <= 0 {
		return "不限"
	}
	return fmt.Sprintf("%d KB/s", kb)
}

// joinOutbounds 把客户端上报的路由出站渲染成可读文本；未上报或空列表显示 —。
func joinOutbounds(routing *protocol.ClientRouting) string {
	if routing == nil || len(routing.Outbounds) == 0 {
		return "—"
	}
	tags := make([]string, 0, len(routing.Outbounds))
	for _, o := range routing.Outbounds {
		tags = append(tags, o.Tag)
	}
	return strings.Join(tags, ", ")
}

// --- 分页与筛选（列表页共用） ---

// pageLink 是分页条上的一个页码；Dots 为 true 时渲染省略号占位。
type pageLink struct {
	N      int
	URL    string
	Active bool
	Dots   bool
}

// pageNav 是列表页的分页条数据：页码窗口 + 上一页/下一页链接（空串表示到头）。
type pageNav struct {
	Page    int
	PerPage int
	Total   int
	Pages   int
	Prev    string
	Next    string
	Links   []pageLink
	// PerLinks 是每页条数选项：切过去一律回到第 1 页（换了窗口大小还停在原页码没意义）。
	PerLinks []pageLink
}

// perPageChoices 是列表页可选的每页条数。
var perPageChoices = []int{10, 20, 50, 100}

// queryInt 读取非负整数查询参数；缺失或非法返回 def。
func queryInt(raw url.Values, key string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw.Get(key)))
	if err != nil || n < 1 {
		return def
	}
	return n
}

// buildPageNav 按当前查询串构造分页条：base 里的筛选参数原样保留，只替换 page。
func buildPageNav(raw url.Values, path string, page, per, total int) pageNav {
	pages := (total + per - 1) / per
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	link := func(n int) string {
		// url.Values.Clone 有语言版本门控（go1.27），手写浅拷贝。
		q := make(url.Values, len(raw))
		for k, vs := range raw {
			q[k] = vs
		}
		q.Set("page", strconv.Itoa(n))
		return path + "?" + q.Encode()
	}
	nav := pageNav{Page: page, PerPage: per, Total: total, Pages: pages}
	for _, n := range perPageChoices {
		q := make(url.Values, len(raw))
		for k, vs := range raw {
			q[k] = vs
		}
		q.Set("per", strconv.Itoa(n))
		q.Del("page")
		nav.PerLinks = append(nav.PerLinks, pageLink{N: n, URL: path + "?" + q.Encode(), Active: n == per})
	}
	if page > 1 {
		nav.Prev = link(page - 1)
	}
	if page < pages {
		nav.Next = link(page + 1)
	}
	// 页码窗口：总页数少就全列；多则折叠成 1 … p-1 p p+1 … pages。
	add := func(n int, active bool) { nav.Links = append(nav.Links, pageLink{N: n, URL: link(n), Active: active}) }
	dots := func() { nav.Links = append(nav.Links, pageLink{Dots: true}) }
	switch {
	case pages <= 7:
		for i := 1; i <= pages; i++ {
			add(i, i == page)
		}
	default:
		add(1, page == 1)
		lo, hi := max(2, page-1), min(pages-1, page+1)
		if lo > 2 {
			dots()
		}
		for i := lo; i <= hi; i++ {
			add(i, i == page)
		}
		if hi < pages-1 {
			dots()
		}
		add(pages, page == pages)
	}
	return nav
}

// slicePage 返回 [start,end) 切片边界：按 page/per 截取 total 行中的当前页。
func slicePage(page, per, total int) (start, end int) {
	start = (page - 1) * per
	if start > total {
		start = total
	}
	if end = start + per; end > total {
		end = total
	}
	return start, end
}

// --- 客户端管理 ---

// clientRow 是客户端管理页的一行；时间与出站列表已格式化为文本。
// 已注册但当前不在线的客户端也会占一行（Online=false，除名称与配置链接外都是"—"）。
type clientRow struct {
	ID     uint64
	Online bool
	// Local 表示该行是服务端本机出口（server_proxy）：常驻出口、不是会话。
	Local       bool
	Name        string
	Version     string
	Platform    string
	Remote      string
	ConnectedAt string
	Heartbeat   string
	RTT         string
	Outbounds   string
	// Iface 是网卡摘要（"eth0 192.168.1.10"），IfaceDetail 是全部网卡的完整列表（悬停显示）。
	Iface       string
	IfaceDetail string
	ConfigURL   string
	// UpBytes/DownBytes 是该客户端在流量表里的累计值（已格式化）。
	UpBytes   string
	DownBytes string
}

// clientsPage 是客户端管理页的数据：Meta 行、统计卡与筛选分页后的客户端列表。
type clientsPage struct {
	Meta          string
	ClientCount   int
	ActiveTunnels int64
	Clients       []clientRow
	Platforms     []string
	Q             string
	Platform      string
	Nav           pageNav
}

func (s *AdminService) pageClients(c *steward.Context) error {
	active := s.panelActiveTunnels()
	query := c.R.URL.Query()
	q := strings.TrimSpace(query.Get("q"))
	platform := strings.TrimSpace(query.Get("platform"))
	perPage := min(queryInt(query, "per", 10), 100)
	page := queryInt(query, "page", 1)

	// 统计卡始终反映全量运行态；列表按搜索与平台筛选后分页。
	all := s.clientsSnapshot()
	platformSet := map[string]bool{}
	rows := make([]clientRow, 0, len(all))
	// 在线名单在过滤前先记全：被平台/关键字筛掉的在线客户端不能在下面被当成离线补一行。
	onlineNames := make(map[string]bool, len(all))
	for _, info := range all {
		platformSet[info.Client.OS] = true
		if info.Client.Name != "" {
			onlineNames[info.Client.Name] = true
		}
		if q != "" && !clientMatches(info, q) {
			continue
		}
		if platform != "" && info.Client.OS != platform {
			continue
		}
		rows = append(rows, s.clientRowOf(info))
	}
	// 已注册但当前不在线的客户端补在后面：离线也要看得见，否则"断开的客户端去哪了"没有答案。
	if s.opts.Bindings != nil {
		for _, cb := range s.opts.Bindings.ClientSnapshot() {
			if onlineNames[cb.ClientKey] {
				continue
			}
			// 离线客户端没有平台与主机信息，按平台筛选时一并隐藏。
			if platform != "" {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(cb.ClientKey), strings.ToLower(q)) {
				continue
			}
			row := clientRow{
				Name:        cb.ClientKey,
				ConfigURL:   clientConfigURL(cb.ClientKey),
				Remote:      "—",
				ConnectedAt: "—",
				Heartbeat:   "—",
				RTT:         "—",
				Outbounds:   "—",
				Iface:       "—",
			}
			if cb.ClientType == store.ClientTypeLocal {
				// 本机出口是常驻出口、不是会话：状态记为本机，且没有可看的配置。
				row.Local = true
				row.Remote = "本机出口（server_proxy）"
				row.ConfigURL = ""
			}
			rows = append(rows, row)
		}
	}
	platforms := make([]string, 0, len(platformSet))
	for p := range platformSet {
		if p != "" {
			platforms = append(platforms, p)
		}
	}
	sort.Strings(platforms)

	// 流量取持久表：在线/离线客户端都能显示，重启不丢。
	_, clientTraffic := s.trafficMaps()
	for i := range rows {
		rows[i].UpBytes, rows[i].DownBytes = "—", "—"
		if tr, ok := clientTraffic[rows[i].Name]; ok {
			rows[i].UpBytes, rows[i].DownBytes = humanBytes(tr.Up), humanBytes(tr.Down)
		}
	}

	nav := buildPageNav(query, "/admin/ops/_clients", page, perPage, len(rows))
	start, end := slicePage(nav.Page, perPage, len(rows))
	pageData := clientsPage{
		Meta: fmt.Sprintf("版本 %s · 启动于 %s · 已运行 %s · 页面每 5 秒自动刷新",
			s.opts.Version, s.started.Format("2006-01-02 15:04:05"),
			time.Since(s.started).Round(time.Second).String()),
		ClientCount:   s.clients.ClientCount(),
		ActiveTunnels: active,
		Clients:       rows[start:end],
		Platforms:     platforms,
		Q:             q,
		Platform:      platform,
		Nav:           nav,
	}
	return c.Render("pages/clients.html", "客户端管理", pageData)
}

// clientMatches 按关键字模糊匹配客户端的可搜索字段：名称、地址、主机名、版本与网卡。
func clientMatches(info ClientInfo, q string) bool {
	hay := []string{info.Client.Name, info.Remote, info.Client.Hostname, info.Client.Version}
	for _, i := range info.Client.Interfaces {
		hay = append(hay, i.Name, i.MAC)
		hay = append(hay, i.Addrs...)
	}
	for _, h := range hay {
		if strings.Contains(strings.ToLower(h), strings.ToLower(q)) {
			return true
		}
	}
	return false
}

func (s *AdminService) clientRowOf(info ClientInfo) clientRow {
	row := clientRow{
		ID:          info.ID,
		Online:      true,
		Remote:      info.Remote,
		ConnectedAt: info.ConnectedAt.Format("15:04:05"),
		Outbounds:   joinOutbounds(info.Client.Routing),
	}
	row.Iface, row.IfaceDetail = ifaceSummary(info.Client.Interfaces)
	row.Name = info.Client.Name
	if row.Name == "" {
		row.Name = "—"
	}
	if info.Client.Version != "" {
		platform := info.Client.OS + "/" + info.Client.Arch
		if info.Client.Hostname != "" {
			platform += " · " + info.Client.Hostname
		}
		row.Version = info.Client.Version + " · " + platform
	}
	if !info.LastHeartbeat.IsZero() {
		row.Heartbeat = info.LastHeartbeat.Format("15:04:05")
		row.RTT = fmt.Sprintf("%.1f ms", info.LastRTTMs)
	} else {
		row.Heartbeat, row.RTT = "—", "—"
	}
	if info.Client.Name != "" {
		row.ConfigURL = clientConfigURL(info.Client.Name)
	}
	return row
}

// clientConfigURL 返回客户端配置页链接（按客户端名定位，在线离线都可用）。
func clientConfigURL(name string) string {
	return "/admin/ops/_client-config?name=" + url.QueryEscape(name)
}

// userTrafficKey 是流量表的用户索引键，与 store 里 (protocol, user_key) 的语义一致。
func userTrafficKey(p store.Protocol, key string) string { return string(p) + "\x00" + key }

// trafficMaps 一次读出两张流量表；读失败只告警并返回空表，页面照常渲染。
func (s *AdminService) trafficMaps() (map[string]store.UserTraffic, map[string]store.ClientTraffic) {
	users := make(map[string]store.UserTraffic)
	clients := make(map[string]store.ClientTraffic)
	if s.opts.Store == nil {
		return users, clients
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if rows, err := s.opts.Store.ListUserTraffic(ctx); err != nil {
		logging.Warnf("admin: list user traffic: %v", err)
	} else {
		for _, r := range rows {
			users[userTrafficKey(r.Protocol, r.Key)] = r
		}
	}
	if rows, err := s.opts.Store.ListClientTraffic(ctx); err != nil {
		logging.Warnf("admin: list client traffic: %v", err)
	} else {
		for _, r := range rows {
			clients[r.Key] = r
		}
	}
	return users, clients
}

// ifaceSummary 把上报的网卡压成表格里的一格：text 优先取第一块有 IPv4 的网卡，
// detail 是全部网卡的完整列表（悬停提示）。未上报时返回 "—"。
func ifaceSummary(ifs []protocol.NetInterface) (text, detail string) {
	if len(ifs) == 0 {
		return "—", ""
	}
	lines := make([]string, 0, len(ifs))
	for _, i := range ifs {
		line := i.Name
		if len(i.Addrs) > 0 {
			line += " " + strings.Join(i.Addrs, " ")
		}
		lines = append(lines, line)
	}
	detail = strings.Join(lines, " · ")
	for _, i := range ifs {
		for _, a := range i.Addrs {
			ip, _, err := net.ParseCIDR(a)
			if err == nil && ip.To4() != nil {
				return i.Name + " " + ip.String(), detail
			}
		}
	}
	if len(ifs[0].Addrs) > 0 {
		return ifs[0].Name + " " + ifs[0].Addrs[0], detail
	}
	return ifs[0].Name, detail
}

// --- 客户端配置 ---

// clientConfigPage 是单个客户端的配置页：上报配置只读，服务端配置可编辑。
type clientConfigPage struct {
	Name         string
	Online       bool
	Remote       string
	ReportedJSON string
	StoredConfig string
	StoredRaw    bool
	HasStored    bool
	UpdatedAt    string
	// LogLevel 是该客户端会话的日志级别；空表示默认 info。
	LogLevel string
}

func (s *AdminService) pageClientConfig(c *steward.Context) error {
	name := strings.TrimSpace(c.R.URL.Query().Get("name"))
	if name == "" {
		http.Error(c.W, "client name is required", http.StatusBadRequest)
		return nil
	}
	data := clientConfigPage{Name: name, StoredConfig: "{}"}
	for _, info := range s.clientsSnapshot() {
		if info.Client.Name != name {
			continue
		}
		data.Online, data.Remote = true, info.Remote
		if b, err := json.MarshalIndent(info.Client, "", "  "); err == nil {
			data.ReportedJSON = string(b)
		}
		break
	}
	if s.opts.Bindings != nil {
		if cb, ok := s.opts.Bindings.LookupClient(name); ok {
			data.HasStored = true
			data.UpdatedAt = cb.UpdatedAt.Format("2006-01-02 15:04:05")
			// 服务端配置在 server_config 列；config 列是客户端上报的快照。
			switch {
			case cb.ServerConfig == "":
				data.StoredConfig = "{}"
			case json.Valid([]byte(cb.ServerConfig)):
				data.StoredConfig = prettyJSON(cb.ServerConfig)
			default:
				data.StoredConfig, data.StoredRaw = cb.ServerConfig, true
			}
			data.LogLevel = store.ParseClientConfig(cb.ServerConfig).LogLevel
			if data.ReportedJSON == "" && cb.Config != "" {
				if json.Valid([]byte(cb.Config)) {
					data.ReportedJSON = prettyJSON(cb.Config)
				} else {
					data.ReportedJSON = cb.Config
				}
			}
		}
	}
	return c.Render("pages/clientconfig.html", "客户端配置", data)
}

// prettyJSON 把 JSON 文本缩进排版；非法 JSON 原样返回。
func prettyJSON(raw string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return raw
	}
	return buf.String()
}

// --- 用户管理 ---

// userRow 是用户管理页的一行：绑定 + 上下行限速 + 累计转发字节（已格式化）。
type userRow struct {
	UserKey   string
	Protocol  string
	Name      string
	Client    string
	Enabled   bool
	LimitUp   string
	LimitDown string
	// Concurrency 是最大并发连接数（0 = 不限）。
	Concurrency int
	UpBytes     string
	DownBytes   string
	RawUpKB     int64
	RawDownKB   int64
	CreatedAt   string
	UpdatedAt   string
	// LogLevel 是该用户会话的日志级别；空表示默认 info。
	LogLevel string
	// Shareable 为 true 时该行渲染"复制链接"按钮（只有 vmess/vless 用户有可分享的链接）。
	Shareable bool
	// ShareLink 是 vmess:// 或 vless:// 分享链接；对应入站没端口（插件未启用/未设置）时为空。
	ShareLink string
}

type usersPage struct {
	Users         []userRow
	Total         int
	ClientOptions []clientOption
	Q             string
	Protocol      string
	Nav           pageNav
}

func (s *AdminService) pageUsers(c *steward.Context) error {
	query := c.R.URL.Query()
	q := strings.TrimSpace(query.Get("q"))
	protocol := strings.TrimSpace(query.Get("protocol"))
	perPage := min(queryInt(query, "per", 10), 100)
	page := queryInt(query, "page", 1)

	// 默认按创建时间倒序；同一时刻写入的行（同批新增）退化为按 (协议, key) 升序，保证顺序稳定。
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
	rows := make([]userRow, 0, len(bindings))
	// 流量取持久表：进程重启后仍然累计。
	userTraffic, _ := s.trafficMaps()
	// 分享链接的两端在整页内固定：主机取配置/请求，端口取 v2ray 插件里对应协议的端口。
	shareHost := s.shareHost(c.R)
	vmessPort, vlessPort := s.v2rayPorts()
	for _, b := range bindings {
		if protocol != "" && string(b.Protocol) != protocol {
			continue
		}
		if q != "" {
			hay := strings.ToLower(b.UserKey + "\x00" + b.Client)
			if !strings.Contains(hay, strings.ToLower(q)) {
				continue
			}
		}
		row := userRow{
			UserKey:     b.UserKey,
			Protocol:    string(b.Protocol),
			Name:        b.Name,
			Client:      b.Client,
			Enabled:     b.Status == store.UserEnabled,
			LimitUp:     humanRate(b.Config.UpLimit / 1024),
			LimitDown:   humanRate(b.Config.DownLimit / 1024),
			Concurrency: b.Config.Concurrency,
			RawUpKB:     b.Config.UpLimit / 1024,
			RawDownKB:   b.Config.DownLimit / 1024,
			CreatedAt:   b.CreatedAt.Format("2006-01-02 15:04:05"),
			UpdatedAt:   b.UpdatedAt.Format("2006-01-02 15:04:05"),
			LogLevel:    b.Config.LogLevel,
			Shareable:   b.Protocol.UsesUUID(),
		}
		if row.Shareable && shareHost != "" {
			if b.Protocol == store.ProtocolVMess {
				row.ShareLink = linkForPort(vmessPort, shareHost, b.UserKey, true)
			} else {
				row.ShareLink = linkForPort(vlessPort, shareHost, b.UserKey, false)
			}
		}
		row.UpBytes, row.DownBytes = "—", "—"
		if tr, ok := userTraffic[userTrafficKey(b.Protocol, b.UserKey)]; ok {
			row.UpBytes, row.DownBytes = humanBytes(tr.Up), humanBytes(tr.Down)
		}
		rows = append(rows, row)
	}

	nav := buildPageNav(query, "/admin/ops/_users", page, perPage, len(rows))
	start, end := slicePage(nav.Page, perPage, len(rows))
	return c.Render("pages/users.html", "用户管理", usersPage{
		Users:         rows[start:end],
		Total:         len(rows),
		ClientOptions: s.clientOptions(),
		Q:             q,
		Protocol:      protocol,
		Nav:           nav,
	})
}

// clientOption 是客户端下拉框的一项：Name 为绑定值，Label 为下拉里的说明。
type clientOption struct {
	Name  string
	Label string
}

// clientOptions 汇总客户端下拉框的候选项：在线客户端（标注"在线"）与已注册的
// client_bindings 记录（绑定到暂不在线的客户端也合法），按名字去重排序。
func (s *AdminService) clientOptions() []clientOption {
	seen := make(map[string]bool)
	var out []clientOption
	for _, info := range s.clientsSnapshot() {
		name := info.Client.Name
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, clientOption{Name: name, Label: name + "（在线）"})
	}
	if s.opts.Bindings != nil {
		for _, cb := range s.opts.Bindings.ClientSnapshot() {
			if seen[cb.ClientKey] {
				continue
			}
			seen[cb.ClientKey] = true
			label := cb.ClientKey + "（已注册）"
			if cb.ClientType == store.ClientTypeLocal {
				label = cb.ClientKey + "（本机）"
			}
			out = append(out, clientOption{Name: cb.ClientKey, Label: label})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// clientOptionsForPortMap 去掉本机出口：端口映射不支持本机出口。
func (s *AdminService) clientOptionsForPortMap() []clientOption {
	all := s.clientOptions()
	out := make([]clientOption, 0, len(all))
	for _, o := range all {
		if o.Name == transport.LocalClientName {
			continue
		}
		out = append(out, o)
	}
	return out
}

// --- 插件管理 ---

// portSlot 是插件对话框里的一个端口输入框：协议名 + 表单字段名 + 当前值。
type portSlot struct {
	Protocol store.Protocol
	Label    string
	Field    string
	Value    int
}

// pluginView 是插件管理页的一个插件区块：状态、账号托管标志与端口输入框。
type pluginView struct {
	plugin.Status
	AcceptsUsers bool
	// Slots 是启用/改端口对话框里的端口输入框，顺序即插件声明的槽位顺序。
	Slots []portSlot
}

type pluginPage struct {
	Plugins    []pluginView
	ServerPort int
}

func (s *AdminService) pagePluginsView() pluginPage {
	list := s.pluginsList()
	serverPort := s.serverPort()
	page := pluginPage{ServerPort: serverPort}
	for _, st := range list {
		v := pluginView{Status: st}
		if s.opts.Plugins != nil {
			if p, err := s.opts.Plugins.Get(st.Name); err == nil {
				if _, ok := p.(plugin.UserSyncer); ok {
					v.AcceptsUsers = true
				}
			}
		}
		v.Slots = portSlots(st.Ports, serverPort)
		page.Plugins = append(page.Plugins, v)
	}
	return page
}

// portSlots 把插件的端口清单翻成对话框输入框：单槽位沿用既有表单字段名 port，多槽位用
// <协议>_port。端口未配置（0）时按服务端监听端口 + 1 兜底——直接用服务端端口会和数据面撞车。
func portSlots(ports []plugin.PortKV, serverPort int) []portSlot {
	out := make([]portSlot, 0, len(ports))
	single := len(ports) == 1
	for _, kv := range ports {
		field := string(kv.Protocol) + "_port"
		value := kv.Port
		if single {
			field = "port"
			value, _ = suggestPorts(kv.Port, serverPort)
		}
		out = append(out, portSlot{
			Protocol: kv.Protocol,
			Label:    strings.ToUpper(string(kv.Protocol)),
			Field:    field,
			Value:    value,
		})
	}
	return out
}

func (s *AdminService) pagePlugins(c *steward.Context) error {
	return c.Render("pages/plugins.html", "插件管理", s.pagePluginsView())
}

// suggestPorts 返回插件行的两个端口：StartPort 是启用按钮提交的端口（当前端口，
// 未设置时取服务端监听端口 + 1——直接用服务端端口会和数据面撞车）；NextPort 是
// 修改端口对话框的默认值（基准 + 1）。serverPort 为 0（监听地址解析失败）时基准
// 退化为 current，由服务端校验报错。
func suggestPorts(current, serverPort int) (start, next int) {
	base := current
	if base <= 0 {
		base = serverPort
	}
	next = base + 1
	if next > 65535 {
		next = 65535
	}
	if current > 0 {
		start = current
	} else {
		start = next
	}
	return start, next
}

// --- 密钥管理 ---

// authKeyRow 是密钥管理页的一行：密钥 + 范围 key + 时间（已格式化）。
type authKeyRow struct {
	Key       string
	Scope     string
	CreatedAt string
	UpdatedAt string
}

type keysPage struct {
	Keys []authKeyRow
}

func (s *AdminService) pageKeys(c *steward.Context) error {
	page := keysPage{}
	if s.opts.Bindings != nil {
		for _, k := range s.opts.Bindings.AuthKeySnapshot() {
			page.Keys = append(page.Keys, authKeyRow{
				Key:       k.Key,
				Scope:     k.Scope,
				CreatedAt: k.CreatedAt.Format("2006-01-02 15:04:05"),
				UpdatedAt: k.UpdatedAt.Format("2006-01-02 15:04:05"),
			})
		}
	}
	return c.Render("pages/keys.html", "密钥管理", page)
}

// handleAuthKeys 是密钥的写入口（POST，表单字段 action=set/delete）：set 按
// 密钥 upsert（同密钥换范围即覆盖），delete 按密钥删除。
func (s *AdminService) handleAuthKeys(w http.ResponseWriter, r *http.Request) {
	if s.opts.Bindings == nil {
		http.Error(w, "auth keys are not configured", http.StatusNotFound)
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
	key := strings.TrimSpace(r.PostFormValue("key"))
	switch r.PostFormValue("action") {
	case "delete":
		if key == "" {
			http.Error(w, "key must not be empty", http.StatusBadRequest)
			return
		}
		deleted, err := s.opts.Bindings.DeleteAuthKey(key)
		if err != nil {
			logging.Error("admin: auth key delete failed", "err", err)
			http.Error(w, "delete failed: storage error", http.StatusInternalServerError)
			return
		}
		logging.Info("admin: auth key deleted", "existed", deleted, "scope_key", key)
	default:
		scope := strings.TrimSpace(r.PostFormValue("scope"))
		// SetAuthKey 负责最小长度与范围形态校验，错误原样回给页面。
		if err := s.opts.Bindings.SetAuthKey(key, scope); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		logging.Info("admin: auth key saved", "scope", scope)
	}
	http.Redirect(w, r, "/admin/ops/_keys", http.StatusSeeOther)
}

// --- 日志查看 ---

type logsPage struct {
	Path      string
	Query     string
	Lines     []string
	Truncated bool
}

func (s *AdminService) pageLogs(c *steward.Context) error {
	query := strings.TrimSpace(c.R.URL.Query().Get("q"))
	data := logsPage{Path: s.opts.LogPath, Query: query}
	if s.opts.LogPath != "" {
		data.Lines, data.Truncated = tailFile(s.opts.LogPath, 256*1024, 300, query)
	} else {
		data.Lines = []string{"日志输出到 stderr，未写文件；在配置 log.output 设置文件路径后可在此查看。"}
	}
	return c.Render("pages/logs.html", "日志查看", data)
}

// --- 端口映射 ---

// portMapRow 是端口映射页的一行：规则 + 运行状态（客户端在线、监听器在听）。
type portMapRow struct {
	ID         uint64
	Client     string
	RemotePort int
	LocalAddr  string
	LimitUp    string
	LimitDown  string
	Enabled    bool
	Remark     string
	// ClientOnline 是该映射的客户端当前是否在线；离线时监听器仍在，只是新连接会被拒。
	ClientOnline bool
	// Listening 是服务端是否已在监听该映射的远程端口（false = 未启用/端口被占/启动失败）。
	Listening bool
	UpdatedAt string
}

// portMapEdit 是编辑表单的预填值；nil 表示新增。
type portMapEdit struct {
	ID         uint64
	Client     string
	RemotePort string
	LocalHost  string
	LocalPort  string
	UpKB       string
	DownKB     string
	Remark     string
	Enabled    bool
}

type portMapsPage struct {
	Rows    []portMapRow
	Clients []clientOption
	Edit    *portMapEdit
	// PublicHost 是提示外部访问用的主机（server.public_host 或请求 Host）。
	PublicHost string
}

func (s *AdminService) pagePortMaps(c *steward.Context) error {
	data := portMapsPage{Clients: s.clientOptionsForPortMap(), PublicHost: s.shareHost(c.R)}
	if s.opts.Store == nil {
		return c.Render("pages/portmaps.html", "端口映射", data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if raw := strings.TrimSpace(c.R.URL.Query().Get("edit")); raw != "" {
		if id, err := strconv.ParseUint(raw, 10, 64); err == nil {
			if m, ok, err := s.opts.Store.GetPortMapping(ctx, id); err == nil && ok {
				data.Edit = &portMapEdit{ID: m.ID, Client: m.Client,
					RemotePort: strconv.Itoa(m.RemotePort), LocalHost: m.LocalHost,
					LocalPort: strconv.Itoa(m.LocalPort),
					UpKB:      strconv.FormatInt(m.Config.UpLimit/1024, 10),
					DownKB:    strconv.FormatInt(m.Config.DownLimit/1024, 10),
					Remark:    m.Remark, Enabled: m.Enabled}
			}
		}
	}
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
	for _, m := range rows {
		data.Rows = append(data.Rows, portMapRow{
			ID:           m.ID,
			Client:       m.Client,
			RemotePort:   m.RemotePort,
			LocalAddr:    net.JoinHostPort(m.LocalHost, strconv.Itoa(m.LocalPort)),
			LimitUp:      humanRate(m.Config.UpLimit / 1024),
			LimitDown:    humanRate(m.Config.DownLimit / 1024),
			Enabled:      m.Enabled,
			Remark:       m.Remark,
			ClientOnline: online[m.Client],
			Listening:    listening[m.ID],
			UpdatedAt:    m.UpdatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	return c.Render("pages/portmaps.html", "端口映射", data)
}

// tailFile 读文件末尾 maxBytes 字节，返回按过滤词筛出的最后 limit 行。
// 日志经 lumberjack 轮转可能很大，只 seek 文件尾部避免整读。
func tailFile(path string, maxBytes, limit int, filter string) (lines []string, truncated bool) {
	f, err := os.Open(path)
	if err != nil {
		return []string{"打开日志文件失败: " + err.Error()}, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return []string{"读取日志文件失败: " + err.Error()}, false
	}
	offset := int64(0)
	if info.Size() > int64(maxBytes) {
		offset = info.Size() - int64(maxBytes)
		truncated = true
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return []string{"读取日志文件失败: " + err.Error()}, false
	}
	all := strings.Split(strings.TrimLeft(string(buf), "\n"), "\n")
	if truncated && len(all) > 0 {
		all = all[1:] // 丢弃被截断的半行
	}
	if filter != "" {
		var kept []string
		for _, l := range all {
			if strings.Contains(l, filter) {
				kept = append(kept, l)
			}
		}
		all = kept
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, truncated
}
