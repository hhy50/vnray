package admin

// helpers.go 是从旧页面层留下来的通用小工具：前端只拿原始数据，
// 这里只保留"取数时需要"的格式化与解析，不做任何展示排版。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
)

// prettyJSON 把 JSON 文本缩进排版；非法 JSON 原样返回。
func prettyJSON(raw string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return raw
	}
	return buf.String()
}

// clientMatches 按关键字模糊匹配客户端的可搜索字段。
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

// clientOptions 汇总客户端下拉候选：在线客户端（标注"在线"）与已注册记录，按名字去重排序。
// 标注文案随请求语言变化，因此需要 r。
func (s *AdminService) clientOptions(r *http.Request) []apiClientOption {
	seen := make(map[string]bool)
	out := make([]apiClientOption, 0, 8)
	for _, info := range s.clientsSnapshot() {
		name := info.Client.Name
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, apiClientOption{Name: name, Label: msg(r, msgClientLabelOnline, name)})
	}
	if s.opts.Bindings != nil {
		for _, cb := range s.opts.Bindings.ClientSnapshot() {
			if seen[cb.ClientKey] {
				continue
			}
			seen[cb.ClientKey] = true
			label := msg(r, msgClientLabelRegistered, cb.ClientKey)
			if cb.ClientType == store.ClientTypeLocal {
				label = msg(r, msgClientLabelLocal, cb.ClientKey)
			}
			out = append(out, apiClientOption{Name: cb.ClientKey, Label: label})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// clientOptionsForPortMap 去掉本机出口：端口映射不支持本机出口。
func (s *AdminService) clientOptionsForPortMap(r *http.Request) []apiClientOption {
	all := s.clientOptions(r)
	out := make([]apiClientOption, 0, len(all))
	for _, o := range all {
		if o.Name == transport.LocalClientName {
			continue
		}
		out = append(out, o)
	}
	return out
}

// portSlots 把插件的端口清单翻成对话框输入框：单槽位沿用字段名 port，多槽位用
// <协议>_port。端口未配置时按服务端监听端口 + 1 兜底（直接用服务端端口会和数据面撞车）。
func portSlots(ports []plugin.PortKV, serverPort int) []apiPortSlot {
	out := make([]apiPortSlot, 0, len(ports))
	single := len(ports) == 1
	for _, kv := range ports {
		field := string(kv.Protocol) + "_port"
		value := kv.Port
		if single {
			field = "port"
			value, _ = suggestPorts(kv.Port, serverPort)
		}
		out = append(out, apiPortSlot{
			Protocol: string(kv.Protocol),
			Label:    strings.ToUpper(string(kv.Protocol)),
			Field:    field,
			Value:    value,
		})
	}
	return out
}

// suggestPorts 返回插件启用的端口建议：当前端口，未设置时取服务端监听端口 + 1。
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

// tailFile 读文件末尾 maxBytes 字节，返回按过滤词筛出的最后 limit 行。// 日志经 lumberjack 轮转可能很大，只 seek 文件尾部避免整读。
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
		all = all[1:]
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
