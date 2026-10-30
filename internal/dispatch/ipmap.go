package dispatch

// IP 段映射：目标地址是 IPv4 且命中 From 模式时，按 To 模式重写成新 IP。
// 模式为点分四段，每段是 0-255 字面量或 * 通配（* 位取源 IP 的对应段），
// 如 123.123.*.* → 192.168.*.* 把 123.123.x.y 改写为 192.168.x.y。仅支持 IPv4。

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/hhy50/vnray/internal/protocol"
)

// ipMapRule 是一条编译好的段映射规则：from/to 各四段，any[i] 表示该段是通配。
type ipMapRule struct {
	from    [4]byte
	fromAny [4]bool
	to      [4]byte
	toAny   [4]bool
}

// IPMaps 是 IPv4 段映射表，并发安全；nil 指针表示未启用。
type IPMaps struct {
	mu          sync.RWMutex
	rules       []ipMapRule
	globalRules []ipMapRule
}

// NewIPMaps 创建一张空表。
func NewIPMaps() *IPMaps { return &IPMaps{} }

// Update 整体替换表内容；nil 等价于清空。解析失败的模式静默跳过（表可能被
// 手工编辑过，坏规则不该拖垮拨号）。
func (m *IPMaps) Update(entries []protocol.IPMap) {
	m.UpdateLayers(entries, nil)
}

// UpdateLayers 同时替换两层规则；独立规则未命中才查询全局规则。
func (m *IPMaps) UpdateLayers(local, global []protocol.IPMap) {
	localRules, globalRules := compileIPMaps(local), compileIPMaps(global)
	m.mu.Lock()
	m.rules, m.globalRules = localRules, globalRules
	m.mu.Unlock()
}

func compileIPMaps(entries []protocol.IPMap) []ipMapRule {
	rules := make([]ipMapRule, 0, len(entries))
	for _, e := range entries {
		from, fromAny, ferr := parseIPMapPattern(e.From)
		to, toAny, terr := parseIPMapPattern(e.To)
		if ferr != nil || terr != nil {
			continue
		}
		rules = append(rules, ipMapRule{from: from, fromAny: fromAny, to: to, toAny: toAny})
	}
	return rules
}

// Rewrite 把 IPv4 目标重写成映射后的地址；未命中或目标不是 IPv4 时返回 false。
func (m *IPMaps) Rewrite(ip string) (string, bool) {
	if m == nil {
		return "", false
	}
	src := net.ParseIP(ip).To4()
	if src == nil {
		return "", false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if rewritten, ok := rewriteIP(m.rules, src); ok {
		return rewritten, true
	}
	return rewriteIP(m.globalRules, src)
}

func rewriteIP(rules []ipMapRule, src net.IP) (string, bool) {
	for _, r := range rules {
		matched := true
		for i := 0; i < 4; i++ {
			if !r.fromAny[i] && r.from[i] != src[i] {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		var out [4]byte
		for i := 0; i < 4; i++ {
			if r.toAny[i] {
				out[i] = src[i]
			} else {
				out[i] = r.to[i]
			}
		}
		return net.IPv4(out[0], out[1], out[2], out[3]).String(), true
	}
	return "", false
}

// ValidIPMap 校验一对段映射模式，供管理台录入校验；错误信息面向运维。
func ValidIPMap(from, to string) error {
	if _, _, err := parseIPMapPattern(from); err != nil {
		return fmt.Errorf("from: %w", err)
	}
	if _, _, err := parseIPMapPattern(to); err != nil {
		return fmt.Errorf("to: %w", err)
	}
	return nil
}

// parseIPMapPattern 把点分四段模式解析成字节值与通配标记；段既非 * 也非 0-255 报错。
func parseIPMapPattern(s string) ([4]byte, [4]bool, error) {
	var oct [4]byte
	var any [4]bool
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return oct, any, fmt.Errorf("IP 段模式 %q 必须恰有 4 段", s)
	}
	for i, p := range parts {
		if p == "*" {
			any[i] = true
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return oct, any, fmt.Errorf("IP 段模式 %q 第 %d 段 %q 必须是 0-255 或 *", s, i+1, p)
		}
		oct[i] = byte(n)
	}
	return oct, any, nil
}
