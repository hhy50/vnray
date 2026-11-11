package config

import (
	"fmt"
	"strings"
)

// ClientServer 是客户端的 server 段：服务端地址、握手密钥与重连策略。
type ClientServer struct {
	Addr      string     `yaml:"addr"`
	AuthKey   string     `yaml:"auth_key"`
	Reconnect *Reconnect `yaml:"reconnect"`
}

type Client struct {
	Server ClientServer `yaml:"server"`
	// Group 是客户端分组，留空使用 default。
	Group string `yaml:"group"`
	// Name 是组内实例名，留空使用 ${hostname}-${ip}。
	Name    string   `yaml:"name"`
	Public  string   `yaml:"public"`
	Private string   `yaml:"private"`
	Log     *Log     `yaml:"log"`
	Routing *Routing `yaml:"routing"`
}

// DefaultClientGroup 是未指定分组时使用的默认值。
const DefaultClientGroup = "default"

// ValidateClientIdentity 校验上报的独立分组与实例名。
func ValidateClientIdentity(group, name string) error {
	for _, part := range []struct{ field, value string }{{"group", group}, {"name", name}} {
		if part.value == "" || strings.ContainsAny(part.value, ":*") {
			return fmt.Errorf("%s: must be a nonempty segment without ':' or '*'", part.field)
		}
	}
	return nil
}

type Routing struct {
	Default   string     `yaml:"default" json:"default"`
	Outbounds []Outbound `yaml:"outbounds" json:"outbounds"`
	Rules     []Rule     `yaml:"rules" json:"rules"`
}

type Outbound struct {
	Tag    string `yaml:"tag" json:"tag"`
	Type   string `yaml:"type" json:"type"`
	Reason string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

type Rule struct {
	Name     string `yaml:"name" json:"name"`
	Outbound string `yaml:"outbound" json:"outbound"`
	Match    Match  `yaml:"match" json:"match"`
}

// Match 是规则的命中条件集合，各条件与各值之间均为 OR。
type Match struct {
	Host          []string `yaml:"host,omitempty" json:"host,omitempty"`
	DomainPrefix  []string `yaml:"domain_prefix,omitempty" json:"domain_prefix,omitempty"`
	DomainSuffix  []string `yaml:"domain_suffix,omitempty" json:"domain_suffix,omitempty"`
	DomainKeyword []string `yaml:"domain_keyword,omitempty" json:"domain_keyword,omitempty"`
	Port          []int    `yaml:"port,omitempty" json:"port,omitempty"`
}

func (m Match) isEmpty() bool {
	return len(m.Host) == 0 && len(m.DomainPrefix) == 0 && len(m.DomainSuffix) == 0 &&
		len(m.DomainKeyword) == 0 && len(m.Port) == 0
}

// DefaultClient 返回内置默认配置：default direct；blocked. 前缀域名走 reject。
func DefaultClient() Client {
	reconnect := DefaultReconnect()
	return Client{
		Group: DefaultClientGroup,
		Server: ClientServer{
			Addr:      "127.0.0.1:9000",
			Reconnect: &reconnect,
		},
		Log: DefaultLog(),
		Routing: &Routing{
			Default: "direct",
			Outbounds: []Outbound{
				{Tag: "direct", Type: "direct"},
				{Tag: "reject", Type: "reject", Reason: "domain blocked by client rule"},
			},
			Rules: []Rule{
				{Name: "block-list", Outbound: "reject", Match: Match{DomainPrefix: []string{"blocked."}}},
				{Name: "private-ip", Outbound: "direct", Match: Match{Host: []string{"127.0.0.1"}}},
			},
		},
	}
}

// Validate 校验客户端配置。
func (c Client) Validate() error {
	if err := validateAddress("server.addr", c.Server.Addr); err != nil {
		return err
	}
	if strings.ContainsAny(c.Group, ":*") {
		return fmt.Errorf("group: must be a single segment without ':' or '*'")
	}
	if strings.ContainsAny(c.Name, ":*") {
		return fmt.Errorf("name: must be a single segment without ':' or '*'; configure group separately")
	}
	if c.Server.AuthKey != "" {
		if err := validateAuthKey("server.auth_key", c.Server.AuthKey); err != nil {
			return err
		}
	}
	if _, err := ParseNetworkPatterns(c.Public); err != nil {
		return fmt.Errorf("public: %w", err)
	}
	if _, err := ParseNetworkPatterns(c.Private); err != nil {
		return fmt.Errorf("private: %w", err)
	}
	if c.Log != nil {
		if err := c.Log.Validate(); err != nil {
			return err
		}
	}
	if c.Server.Reconnect != nil {
		if err := c.Server.Reconnect.Validate(); err != nil {
			return err
		}
	}
	return c.Routing.Validate()
}

// Validate 校验路由配置。
func (r Routing) Validate() error {
	if len(r.Outbounds) == 0 {
		return fmt.Errorf("routing.outbounds: at least one outbound is required")
	}
	tags := make(map[string]bool, len(r.Outbounds))
	for i, o := range r.Outbounds {
		if o.Tag == "" {
			return fmt.Errorf("routing.outbounds[%d]: tag must not be empty", i)
		}
		if tags[o.Tag] {
			return fmt.Errorf("routing.outbounds[%d]: duplicated outbound tag %q", i, o.Tag)
		}
		tags[o.Tag] = true
		if !validOutTypes[o.Type] {
			return fmt.Errorf("routing.outbounds[%d] (%s): invalid type %q, expect direct/reject", i, o.Tag, o.Type)
		}
	}
	if r.Default == "" {
		return fmt.Errorf("routing.default: default outbound tag must not be empty")
	}
	if !tags[r.Default] {
		return fmt.Errorf("routing.default: tag %q is not registered in outbounds", r.Default)
	}
	for i, rule := range r.Rules {
		if rule.Name == "" {
			return fmt.Errorf("routing.rules[%d]: rule name must not be empty", i)
		}
		if !tags[rule.Outbound] {
			return fmt.Errorf("routing.rules[%d] (%s): outbound %q is not registered", i, rule.Name, rule.Outbound)
		}
		if rule.Match.isEmpty() {
			return fmt.Errorf("routing.rules[%d] (%s): match must contain at least one condition", i, rule.Name)
		}
		for _, p := range rule.Match.Port {
			if p <= 0 || p >= 65536 {
				return fmt.Errorf("routing.rules[%d] (%s): port %d must be in 1..65535", i, rule.Name, p)
			}
		}
	}
	return nil
}
