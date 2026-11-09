package dispatch

// 客户端本地维护的自定义域名表：出站拨号前先查表，命中则改用表里的 IP 直连，
// 未命中才交给 DNS。服务端下发整表替换（见 internal/protocol.CustomHosts）。

import (
	"strings"
	"sync/atomic"

	"github.com/hhy50/vnray/internal/protocol"
)

// Hosts 是自定义域名表，并发安全；nil 指针表示未启用（一律走 DNS）。
type Hosts struct {
	tables atomic.Pointer[hostTables]
}

type hostTables struct {
	local  map[string]string
	global map[string]string
}

// NewHosts 创建一张空表。
func NewHosts() *Hosts { return &Hosts{} }

// Update 整体替换表内容；nil 等价于清空。
func (h *Hosts) Update(entries []protocol.CustomHost) {
	h.UpdateLayers(entries, nil)
}

// UpdateLayers 同时替换独立与全局表，查找时独立表优先。
func (h *Hosts) UpdateLayers(local, global []protocol.CustomHost) {
	h.tables.Store(&hostTables{local: hostTable(local), global: hostTable(global)})
}

func hostTable(entries []protocol.CustomHost) map[string]string {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		if d := normalizeDomain(e.Domain); d != "" && e.IP != "" {
			m[d] = e.IP
		}
	}
	return m
}

// LookupHost 返回域名对应的自定义 IP；未命中返回 false。
func (h *Hosts) LookupHost(domain string) (string, bool) {
	if h == nil {
		return "", false
	}
	tables := h.tables.Load()
	if tables == nil {
		return "", false
	}
	domain = normalizeDomain(domain)
	ip, ok := tables.local[domain]
	if !ok {
		ip, ok = tables.global[domain]
	}
	return ip, ok
}

// normalizeDomain 归一化域名用于查表：转小写、去尾部点。
func normalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	return strings.TrimSuffix(d, ".")
}
