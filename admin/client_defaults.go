package admin

import (
	"slices"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/transport"
)

// SetGlobalClientConfig 更新全局继承值，调用方在落库成功后推送在线客户端。
func (b *Bindings) SetGlobalClientConfig(settings config.GlobalClientSettings) {
	global := &protocol.ClientConfig{}
	for _, h := range settings.CustomHosts {
		global.CustomHosts = append(global.CustomHosts, protocol.CustomHost{Domain: h.Domain, IP: h.IP})
	}
	for _, m := range settings.IPMaps {
		global.IPMaps = append(global.IPMaps, protocol.IPMap{From: m.From, To: m.To})
	}
	b.globalClient.Store(global)
}

// PushConfig 返回分层快照，保持独立配置与全局配置各自的规则和顺序。
func (b *Bindings) PushConfig(clientKey string) protocol.PushConfig {
	push := protocol.PushConfig{ClientConfig: b.ClientPushConfig(clientKey)}
	global := b.globalClient.Load()
	if global != nil {
		push.GlobalConfig = protocol.ClientConfig{
			CustomHosts: slices.Clone(global.CustomHosts),
			IPMaps:      slices.Clone(global.IPMaps),
		}
	}
	return push
}

// PushClientConfig 串行发送最新分层快照，避免并发保存时旧快照晚到。
func (b *Bindings) PushClientConfig(client *transport.Session) error {
	b.pushMu.Lock()
	defer b.pushMu.Unlock()
	return client.SendPushConfig(b.PushConfig(client.Info().Name))
}

func normalizeGlobalClient(settings config.GlobalClientSettings) (config.GlobalClientSettings, error) {
	domains, ips := []string{}, []string{}
	for _, h := range settings.CustomHosts {
		domains = append(domains, h.Domain)
		ips = append(ips, h.IP)
	}
	hosts, err := parseCustomHosts(domains, ips)
	if err != nil {
		return config.GlobalClientSettings{}, err
	}
	froms, tos := []string{}, []string{}
	for _, m := range settings.IPMaps {
		froms = append(froms, m.From)
		tos = append(tos, m.To)
	}
	maps, err := parseIPMaps(froms, tos)
	if err != nil {
		return config.GlobalClientSettings{}, err
	}
	out := config.GlobalClientSettings{}
	for _, h := range hosts {
		out.CustomHosts = append(out.CustomHosts, config.CustomHost{Domain: h.Domain, IP: h.IP})
	}
	for _, m := range maps {
		out.IPMaps = append(out.IPMaps, config.IPMap{From: m.From, To: m.To})
	}
	return out, out.Validate()
}
