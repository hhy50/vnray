package access

import (
	"context"
	"slices"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
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
func (b *Bindings) PushConfig(parent context.Context, clientKey string) protocol.PushConfig {
	ctx, cancel := queryCtx(parent)
	defer cancel()
	return b.pushConfigFor(b.clientPushConfig(ctx, clientKey))
}

// pushConfigFor 在已取得的本客户端配置之上叠加全局继承值。
func (b *Bindings) pushConfigFor(local protocol.ClientConfig) protocol.PushConfig {
	push := protocol.PushConfig{ClientConfig: local}
	global := b.globalClient.Load()
	if global != nil {
		push.GlobalConfig = protocol.ClientConfig{
			CustomHosts: slices.Clone(global.CustomHosts),
			IPMaps:      slices.Clone(global.IPMaps),
		}
	}
	return push
}

// ClientRuntime 一次点查取回握手与配置下发要用的全部客户端参数。这三项同源同表，
// 分开取就是每项各查一次库，而握手处在连接建立路径上。
func (b *Bindings) ClientRuntime(parent context.Context, clientKey string) transport.ClientRuntime {
	ctx, cancel := queryCtx(parent)
	defer cancel()
	var local protocol.ClientConfig
	var rt transport.ClientRuntime
	if c, ok := b.lookupClient(ctx, clientKey); ok {
		cc := store.ParseClientConfig(c.ServerConfig)
		local = protocol.ClientConfig{CustomHosts: cc.CustomHosts, IPMaps: cc.IPMaps}
		rt.LogLevel, rt.Concurrency = cc.LogLevel, cc.Concurrency
	}
	rt.Push = b.pushConfigFor(local)
	return rt
}

// PushClientConfig 串行发送最新分层快照，避免并发保存时旧快照晚到。
func (b *Bindings) PushClientConfig(parent context.Context, client *transport.Session) error {
	ctx, cancel := queryCtx(parent)
	defer cancel()
	return b.PushClientConfigContext(ctx, client)
}

// PushClientConfigContext 在调用方期限内推送客户端的最新配置。
func (b *Bindings) PushClientConfigContext(ctx context.Context, client *transport.Session) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var readErr error
	err := client.SendPushConfigContext(ctx, func() protocol.PushConfig {
		row, _, err := b.store.GetClient(ctx, client.Info().ClientKey())
		if err != nil {
			readErr = err
			// 读取失败不能发布默认配置或清除已有并发上限。
			cancel()
			return protocol.PushConfig{}
		}
		settings := store.ParseClientConfig(row.ServerConfig)
		client.SetClientConcurrency(settings.Concurrency)
		return b.pushConfigFor(protocol.ClientConfig{CustomHosts: settings.CustomHosts, IPMaps: settings.IPMaps})
	})
	if readErr != nil {
		return readErr
	}
	return err
}
