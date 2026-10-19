package cli

// netinfo.go 采集客户端本机的网卡摘要，随认证上报给服务端（Auth 帧的 client 字段）。
// 只用于诊断，采集失败不影响接入。

import (
	"net"

	"github.com/hhy50/vnray/internal/protocol"
)

// primaryIPv4 返回首个 up、非回环网卡上的 IPv4 地址（不带掩码）；找不到返回空串。
// 采集失败不影响调用方，只退化成没有 IP 的默认名。
func primaryIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ip, _, err := net.ParseCIDR(a.String())
			if err != nil {
				continue
			}
			if v4 := ip.To4(); v4 != nil && !v4.IsLinkLocalUnicast() {
				return v4.String()
			}
		}
	}
	return ""
}

// collectInterfaces 返回 up、非回环、且至少有一个非 link-local 地址的网卡。
// 回环与已关闭的网卡没有诊断价值；只有 fe80:: 的网卡（macOS 的 awdl/llw/utun 之类）
// 只在链路内有意义，全报上去只会把管理台的列表塞满。系统调用失败时返回 nil。
func collectInterfaces() []protocol.NetInterface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]protocol.NetInterface, 0, len(ifaces))
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		keep := make([]string, 0, len(addrs))
		for _, a := range addrs {
			if ip, _, err := net.ParseCIDR(a.String()); err == nil && ip.IsLinkLocalUnicast() {
				continue
			}
			keep = append(keep, a.String())
		}
		if len(keep) == 0 {
			continue
		}
		out = append(out, protocol.NetInterface{
			Name:  ifi.Name,
			MTU:   ifi.MTU,
			MAC:   ifi.HardwareAddr.String(),
			Flags: ifi.Flags.String(),
			Addrs: keep,
		})
	}
	return out
}
