package main

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/hhy50/vnray/internal/acl"
	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/client"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/metrics"
	"github.com/hhy50/vnray/internal/pipe"
	"github.com/hhy50/vnray/internal/protocol"
)

// clientApp 是客户端进程的运行时组装：ACL 守卫的 dispatcher + 数据面客户端。
type clientApp struct {
	serverAddr string
	dialOpts   client.DialOptions
	dispatcher dispatch.Dispatcher
	reconnect  config.Reconnect
}

// Run 连接服务端（含认证握手）并阻塞处理请求。
//
// 默认不返回：拨号失败或会话中断后按固定间隔重连，只有认证被拒或 ctx 结束才退出。
func (a *clientApp) Run(ctx context.Context) error {
	if !a.reconnect.Enabled {
		return a.serveOnce(ctx)
	}
	return retryLoop(ctx, a.reconnect.Interval.Std(), a.serveOnce)
}

// serveOnce 建立一次连接并服务到会话结束；ctx 结束会主动关掉会话让 Serve 返回。
func (a *clientApp) serveOnce(ctx context.Context) error {
	c, err := client.DialContext(ctx, a.serverAddr, a.dispatcher, a.dialOpts)
	if err != nil {
		return fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()

	logging.Infof("waiting for server requests: server=%s", a.serverAddr)
	if err := c.ServeContext(ctx); err != nil {
		return err
	}
	return nil
}

// newClientApp 装配客户端：ACL 守卫包装配置翻译出的路由，并挂上自定义域名表。
func newClientApp(cfg config.Client) (*clientApp, error) {
	// pipe 保持零项目内依赖，缓冲指标经观察者注入；须在数据面启动前设置。
	pipe.SetBufferObserver(metrics.GaugeFor("pipe_buffered_bytes").Add)
	if _, err := authkey.Parse(cfg.Server.AuthKey); err != nil {
		return nil, err
	}
	guard, err := acl.NewGuard(cfg.Public, cfg.Private)
	if err != nil {
		return nil, err
	}
	reconnect := effectiveReconnect(cfg.Server.Reconnect)
	// 自定义域名表与 IP 段映射由数据面（配置上报应答/下发帧）与拨号（直连出站）共享同一
	// 组实例：服务端热更后新隧道立即按新配置拨号。
	hosts := dispatch.NewHosts()
	ipMaps := dispatch.NewIPMaps()
	dispatcher := newRoutingDispatcher(cfg.Routing)
	dispatcher.SetHosts(hosts)
	dispatcher.SetIPMaps(ipMaps)
	// 直连分发器与服务端出站规则的 direct 名单配套：只跳过路由规则，ACL 仍然生效。
	direct := newDirectDispatcher()
	direct.SetHosts(hosts)
	direct.SetIPMaps(ipMaps)
	info := newClientInfo(cfg, reconnect)
	return &clientApp{
		serverAddr: cfg.Server.Addr,
		dialOpts: client.DialOptions{
			Auth: client.AuthOptions{Key: cfg.Server.AuthKey},
			Info: info,
			ReportInfo: func() protocol.ClientInfo {
				latest := newClientInfo(cfg, reconnect)
				latest.Group = info.Group
				latest.Name = info.Name
				return latest
			},
			OnConfig: func(cfg protocol.PushConfig) {
				hosts.UpdateLayers(cfg.ClientConfig.CustomHosts, cfg.GlobalConfig.CustomHosts)
				ipMaps.UpdateLayers(cfg.ClientConfig.IPMaps, cfg.GlobalConfig.IPMaps)
				logging.Debug("client: push config applied",
					"client_hosts", len(cfg.ClientConfig.CustomHosts), "global_hosts", len(cfg.GlobalConfig.CustomHosts),
					"client_ip_maps", len(cfg.ClientConfig.IPMaps), "global_ip_maps", len(cfg.GlobalConfig.IPMaps))
			},
			Direct: guard.Wrap(direct),
		},
		dispatcher: guard.Wrap(dispatcher),
		reconnect:  reconnect,
	}, nil
}

// effectiveReconnect 取生效的重连配置：未配置时用默认值。
func effectiveReconnect(cfg *config.Reconnect) config.Reconnect {
	if cfg != nil {
		return *cfg
	}
	return config.DefaultReconnect()
}

// newClientInfo 汇总客户端要定期上报的生效配置与环境：本端标识 + 配置摘要 + 主机环境。
func newClientInfo(cfg config.Client, reconnect config.Reconnect) protocol.ClientInfo {
	hostname, _ := os.Hostname()
	group := cfg.Group
	if group == "" {
		group = config.DefaultClientGroup
	}
	name := cfg.Name
	if name == "" {
		// 拿不到 IPv4 时退化成不带 IP 的默认名。
		name = hostname
		if ip := primaryIPv4(); ip != "" {
			name += "-" + ip
		}
	}
	log := cfg.Log
	if log == nil {
		log = config.DefaultLog()
	}
	info := protocol.ClientInfo{
		Group:      group,
		Name:       name,
		Version:    version,
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Hostname:   hostname,
		PID:        os.Getpid(),
		Server:     cfg.Server.Addr,
		Public:     cfg.Public,
		Private:    cfg.Private,
		Interfaces: collectInterfaces(),
		Log: &protocol.ClientLog{
			Level:   log.Level,
			Output:  log.Output,
			Console: log.Console,
		},
		Reconnect: &protocol.ClientReconnect{
			Enabled:  reconnect.Enabled,
			Interval: reconnect.Interval.String(),
		},
		Routing: newClientRouting(cfg.Routing),
	}
	return info
}

// newClientRouting 翻译生效的路由配置；未配置时为 nil，对应字段不上报。
func newClientRouting(rc *config.Routing) *protocol.ClientRouting {
	if rc == nil {
		return nil
	}
	routing := &protocol.ClientRouting{Default: rc.Default}
	for _, o := range rc.Outbounds {
		routing.Outbounds = append(routing.Outbounds, protocol.ClientOutbound{
			Tag: o.Tag, Type: o.Type, Reason: o.Reason,
		})
	}
	for _, r := range rc.Rules {
		routing.Rules = append(routing.Rules, protocol.ClientRule{
			Name:     r.Name,
			Outbound: r.Outbound,
			Match: protocol.ClientMatch{
				Host:          r.Match.Host,
				DomainPrefix:  r.Match.DomainPrefix,
				DomainSuffix:  r.Match.DomainSuffix,
				DomainKeyword: r.Match.DomainKeyword,
				Port:          r.Match.Port,
			},
		})
	}
	return routing
}
