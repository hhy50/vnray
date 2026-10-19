package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ParseNetworkPatterns 解析逗号分隔的网段模式列表，每个模式支持三种写法：
// CIDR（"192.168.31.0/24"）、IPv4 尾部通配（"192.168.31.*"）、单 IP（"127.0.0.1"）。
// 空字符串或仅含逗号/空白时返回空列表。
func ParseNetworkPatterns(text string) ([]net.IPNet, error) {
	var nets []net.IPNet
	for _, token := range strings.Split(text, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		n, err := parseOnePattern(token)
		if err != nil {
			return nil, fmt.Errorf("network pattern %q: %w", token, err)
		}
		nets = append(nets, *n)
	}
	return nets, nil
}

func parseOnePattern(token string) (*net.IPNet, error) {
	switch {
	case strings.Contains(token, "/"):
		_, network, err := net.ParseCIDR(token)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR: %w", err)
		}
		return network, nil

	case strings.Contains(token, "*"):
		return parseIPv4Wildcard(token)

	default:
		ip := net.ParseIP(token)
		if ip == nil {
			return nil, fmt.Errorf("not a valid IP, CIDR or wildcard")
		}
		// 单 IP 视为对应主机前缀；统一成 4 字节表示 IPv4。
		if v4 := ip.To4(); v4 != nil {
			return &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}, nil
		}
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}, nil
	}
}

// parseIPv4Wildcard 把 "192.168.31.*" 规整为 IPv4 前缀；
// 星号只能从某一个八位组开始连续出现在尾部。
func parseIPv4Wildcard(token string) (*net.IPNet, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 4 {
		return nil, fmt.Errorf("wildcard must be a dotted IPv4 pattern with 4 octets")
	}
	octets := make([]byte, 4)
	fixed := 0
	seenStar := false
	for i, part := range parts {
		if part == "*" {
			seenStar = true
			octets[i] = 0
			continue
		}
		if seenStar {
			return nil, fmt.Errorf("wildcard octets must be a trailing run")
		}
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || value > 255 {
			return nil, fmt.Errorf("octet %q must be 0..255", part)
		}
		octets[i] = byte(value)
		fixed++
	}
	if !seenStar {
		return nil, fmt.Errorf("not a wildcard pattern")
	}
	ip := net.IPv4(octets[0], octets[1], octets[2], octets[3])
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(fixed*8, 32)}, nil
}
