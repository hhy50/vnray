// Package rule 定义用户出站规则：规则由服务端按名保存，判定也在服务端做——
// 用户端只引用规则名，拿不到也改不了规则内容。
//
// 一条规则是两组目标名单：命中 reject 的目标直接拒绝，命中 direct 的目标按直连
// 交给用户绑定的客户端（隧道照常开，但客户端跳过它自己的路由规则）。
package rule

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
)

// KeyPrefix 是规则在服务端配置表（vn_service_settings）里的键前缀：rule.<name>。
const KeyPrefix = "rule."

// NameDefault 是唯一的预置规则名：用户没指定规则时的兜底。它只是名字约定，
// 与其他规则一样可以改名单、也可以删除（删掉后未指定规则的用户按放行处理）。
const NameDefault = "default"

// maxNameLen 是规则名长度上限：setting_key 列宽 64，扣掉前缀还要留余量。
const maxNameLen = 32

// Rule 是一条用户出站规则。条目支持域名（精确，忽略大小写）、`*.example.com` /
// `.example.com`（后缀通配）、IP、CIDR 与 `*`（匹配一切）。
type Rule struct {
	Reject []string `json:"reject"`
	Direct []string `json:"direct"`
}

// Named 是带名字的规则，供管理台列表与快照使用。
type Named struct {
	Name string `json:"name"`
	Rule Rule   `json:"rule"`
}

// Verdict 是规则对一个目标的判定结果。
type Verdict int

const (
	// Pass 表示未命中任何名单：按默认路径转发（走用户绑定的客户端）。
	Pass Verdict = iota
	// Direct 表示命中直连名单：隧道仍交给绑定客户端，但跳过它自己的路由规则。
	Direct
	// Reject 表示命中拒绝名单。
	Reject
)

// String 返回判定名，供日志使用。
func (v Verdict) String() string {
	switch v {
	case Direct:
		return "direct"
	case Reject:
		return "reject"
	default:
		return "pass"
	}
}

// Key 返回规则名在配置表里的键。
func Key(name string) string { return KeyPrefix + name }

// NameFromKey 从配置表键还原规则名；不是规则键时 ok 为 false。
func NameFromKey(key string) (string, bool) {
	if !strings.HasPrefix(key, KeyPrefix) {
		return "", false
	}
	name := strings.TrimPrefix(key, KeyPrefix)
	return name, name != ""
}

// ValidName 校验规则名：非空、长度受限、只允许字母数字与 _-.。
func ValidName(name string) error {
	if name == "" {
		return fmt.Errorf("rule name must not be empty")
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("rule name %q: at most %d bytes", name, maxNameLen)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return fmt.Errorf("rule name %q: only letters, digits, '_', '-' and '.' are allowed", name)
		}
	}
	return nil
}

// Defaults 返回预置规则：只有 default，且名单为空（不干预）。它只在配置表里一条
// 规则都没有时写入，删掉之后不会被重新补种。
func Defaults() map[string]Rule {
	return map[string]Rule{NameDefault: {}}
}

// Normalize 去掉空白项与首尾空白，并把空名单规整成空切片（JSON 里是 []，不是 null）。
func (r Rule) Normalize() Rule {
	return Rule{Reject: cleanList(r.Reject), Direct: cleanList(r.Direct)}
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, item := range in {
		if v := strings.TrimSpace(item); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Encode 把规则序列化成落库值。
func Encode(r Rule) (string, error) {
	b, err := json.Marshal(r.Normalize())
	if err != nil {
		return "", fmt.Errorf("encode rule: %w", err)
	}
	return string(b), nil
}

// Parse 解析落库值；空串视为空规则。
func Parse(raw string) (Rule, error) {
	if strings.TrimSpace(raw) == "" {
		return Rule{}, nil
	}
	var r Rule
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return Rule{}, fmt.Errorf("decode rule: %w", err)
	}
	return r.Normalize(), nil
}

// matcher 是一组目标模式的编译结果。域名与 IP 分开存：域名走后缀/精确比较，
// IP 走网段包含。
type matcher struct {
	all    bool
	hosts  map[string]struct{}
	suffix []string
	nets   []*net.IPNet
}

// newMatcher 编译一组模式；非法 CIDR 直接报错，避免运行期静默失效。
func newMatcher(patterns []string) (*matcher, error) {
	m := &matcher{hosts: make(map[string]struct{}, len(patterns))}
	for _, raw := range patterns {
		p := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case p == "":
			continue
		case p == "*":
			m.all = true
		case strings.Contains(p, "/"):
			_, ipnet, err := net.ParseCIDR(p)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q", raw)
			}
			m.nets = append(m.nets, ipnet)
		case strings.HasPrefix(p, "*."):
			m.suffix = append(m.suffix, strings.TrimPrefix(p, "*"))
		case strings.HasPrefix(p, "."):
			m.suffix = append(m.suffix, p)
		default:
			m.hosts[p] = struct{}{}
		}
	}
	return m, nil
}

// match 报告 host 是否命中；host 允许是域名或 IP 文本。
func (m *matcher) match(host string) bool {
	if m == nil {
		return false
	}
	if m.all {
		return true
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "" {
		return false
	}
	if _, ok := m.hosts[h]; ok {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		for _, n := range m.nets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	for _, suffix := range m.suffix {
		// 后缀通配要求前面还有标签：example.com 不该被 .example.com 命中。
		if len(h) > len(suffix) && strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

// Compiled 是编译后的规则，可并发只读使用。
type Compiled struct {
	name   string
	rule   Rule
	reject *matcher
	direct *matcher
}

// Compile 校验并编译一条规则；名字非法、条目写错都返回错误。
func Compile(name string, r Rule) (*Compiled, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	r = r.Normalize()
	reject, err := newMatcher(r.Reject)
	if err != nil {
		return nil, fmt.Errorf("rule %q reject: %w", name, err)
	}
	direct, err := newMatcher(r.Direct)
	if err != nil {
		return nil, fmt.Errorf("rule %q direct: %w", name, err)
	}
	return &Compiled{name: name, rule: r, reject: reject, direct: direct}, nil
}

// Name 返回规则名。
func (c *Compiled) Name() string { return c.name }

// Rule 返回规则内容的副本（管理台展示用）。
func (c *Compiled) Rule() Rule {
	return Rule{Reject: append([]string(nil), c.rule.Reject...), Direct: append([]string(nil), c.rule.Direct...)}
}

// Verdict 返回目标 host 的判定：reject 优先于 direct，同一目标同时命中时拒绝。
func (c *Compiled) Verdict(host string) Verdict {
	if c == nil {
		return Pass
	}
	if c.reject.match(host) {
		return Reject
	}
	if c.direct.match(host) {
		return Direct
	}
	return Pass
}

// SortNamed 按名字升序排列规则，供列表输出稳定。
func SortNamed(rules []Named) {
	sort.Slice(rules, func(i, j int) bool { return rules[i].Name < rules[j].Name })
}
