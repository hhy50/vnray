package config

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
)

// CustomHost 是域名到固定 IP 的配置。
type CustomHost struct {
	Domain string `json:"domain"`
	IP     string `json:"ip"`
}

// IPMap 是按 IPv4 四段匹配和替换的配置。
type IPMap struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// GlobalClientSettings 是所有远程客户端继承的基础配置。
type GlobalClientSettings struct {
	CustomHosts []CustomHost `json:"custom_hosts"`
	IPMaps      []IPMap      `json:"ip_maps"`
}

// Clone 返回独立副本，防止管理台编辑改变运行中的配置。
func (c GlobalClientSettings) Clone() GlobalClientSettings {
	c.CustomHosts = slices.Clone(c.CustomHosts)
	c.IPMaps = slices.Clone(c.IPMaps)
	return c
}

// Validate 校验全局客户端配置，允许单段域名和 IPv4 通配段。
func (c GlobalClientSettings) Validate() error {
	for i, h := range c.CustomHosts {
		domain := strings.TrimSuffix(strings.TrimSpace(h.Domain), ".")
		if domain == "" || len(domain) > 253 || strings.ContainsAny(domain, " \t\r\n/:") || net.ParseIP(domain) != nil {
			return fmt.Errorf("global_client.custom_hosts 第 %d 行：域名无效", i+1)
		}
		if net.ParseIP(h.IP) == nil {
			return fmt.Errorf("global_client.custom_hosts 第 %d 行：IP 无效", i+1)
		}
	}
	for i, m := range c.IPMaps {
		for _, pattern := range []string{m.From, m.To} {
			if !validIPMapPattern(pattern) {
				return fmt.Errorf("global_client.ip_maps 第 %d 行：%q 必须为四段 IPv4，每段为 0-255 或 *", i+1, pattern)
			}
		}
	}
	return nil
}

func validIPMapPattern(pattern string) bool {
	parts := strings.Split(pattern, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if part == "*" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	return true
}
