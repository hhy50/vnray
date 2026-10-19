package routing

// 配置文件到数据面 dispatcher 的翻译：client 与 server 本机出口共用同一实现。

import (
	"strings"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/dispatch"
)

// NewDispatcher 按配置创建出站与路由规则。
func NewDispatcher(rc *config.Routing) *dispatch.DefaultDispatcher {
	outbounds := make([]dispatch.OutboundHandler, 0, len(rc.Outbounds))
	for _, oc := range rc.Outbounds {
		switch oc.Type {
		case "direct":
			outbounds = append(outbounds, dispatch.NewDirectOutbound(oc.Tag))
		case "reject":
			outbounds = append(outbounds, dispatch.NewRejectOutbound(oc.Tag, oc.Reason))
		}
	}
	rules := make([]dispatch.Rule, 0, len(rc.Rules))
	for _, rcRule := range rc.Rules {
		rules = append(rules, dispatch.Rule{
			Name:  rcRule.Name,
			Match: buildMatch(rcRule.Match),
			Tag:   rcRule.Outbound,
		})
	}
	return dispatch.NewDefaultDispatcher(rules, rc.Default, outbounds...)
}

// buildMatch 把配置条件翻译成匹配函数；各条件之间为 OR。
func buildMatch(m config.Match) func(dispatch.Destination) bool {
	return func(dest dispatch.Destination) bool {
		for _, h := range m.Host {
			if dest.Host == h {
				return true
			}
		}
		for _, prefix := range m.DomainPrefix {
			if strings.HasPrefix(dest.Host, prefix) {
				return true
			}
		}
		for _, suffix := range m.DomainSuffix {
			if strings.HasSuffix(dest.Host, suffix) {
				return true
			}
		}
		for _, keyword := range m.DomainKeyword {
			if strings.Contains(dest.Host, keyword) {
				return true
			}
		}
		for _, p := range m.Port {
			if dest.Port == p {
				return true
			}
		}
		return false
	}
}
