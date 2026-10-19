package dispatch

import (
	"context"
	"fmt"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/pipe"
	"github.com/hhy50/vnray/internal/routine"
)

// Rule 是一条路由规则。
type Rule struct {
	Name  string
	Match func(dest Destination) bool
	Tag   string
}

// DefaultDispatcher 是默认的路由分发器。
type DefaultDispatcher struct {
	outbounds  map[string]OutboundHandler
	rules      []Rule
	defaultTag string
}

// NewDefaultDispatcher 创建 dispatcher 并注册出站。
func NewDefaultDispatcher(rules []Rule, defaultTag string, outbounds ...OutboundHandler) *DefaultDispatcher {
	d := &DefaultDispatcher{
		outbounds:  make(map[string]OutboundHandler),
		rules:      rules,
		defaultTag: defaultTag,
	}
	for _, o := range outbounds {
		d.outbounds[o.Tag()] = o
	}
	return d
}

// SetHosts 把共享的自定义域名表交给全部直连出站（拨号前查表）；nil 停用。
// 表是运行期共享实例，热更新对后续拨号即时生效。
func (d *DefaultDispatcher) SetHosts(h *Hosts) {
	for _, o := range d.outbounds {
		if do, ok := o.(*DirectOutbound); ok {
			do.SetHosts(h)
		}
	}
}

// SetIPMaps 把共享的 IP 段映射交给全部直连出站（拨号前重写目标 IP）；nil 停用。
// 表是运行期共享实例，热更新对后续拨号即时生效。
func (d *DefaultDispatcher) SetIPMaps(m *IPMaps) {
	for _, o := range d.outbounds {
		if do, ok := o.(*DirectOutbound); ok {
			do.SetIPMaps(m)
		}
	}
}

// Dispatch 异步路由并调用出站：立即返回入站端 Link，
// 出站端的连接建立与转发在后台 goroutine 进行。
func (d *DefaultDispatcher) Dispatch(ctx context.Context, dest Destination) (*Link, error) {
	if !dest.IsValid() {
		return nil, fmt.Errorf("dispatcher: invalid destination %v", dest)
	}
	inboundLink, outboundLink := d.getLink()
	routine.Go("core.dispatcher.dispatch", func() { d.routedDispatch(ctx, outboundLink, dest) })
	return inboundLink, nil
}

// getLink 用两条单向管道交叉拼出入站/出站两个端点视图。
func (d *DefaultDispatcher) getLink() (inbound *Link, outbound *Link) {
	upReader, upWriter := pipe.New()     // uplink: 入站 → 出站
	downReader, downWriter := pipe.New() // downlink: 出站 → 入站

	inbound = &Link{Reader: downReader, Writer: upWriter}
	outbound = &Link{Reader: upReader, Writer: downWriter}
	return
}

// routedDispatch 按顺序匹配规则选出站，找不到就走默认出站。
func (d *DefaultDispatcher) routedDispatch(ctx context.Context, link *Link, dest Destination) {
	tag := d.defaultTag
	var via string = "default"
	for _, r := range d.rules {
		if r.Match(dest) {
			tag = r.Tag
			via = "rule [" + r.Name + "]"
			break
		}
	}
	handler, ok := d.outbounds[tag]
	if !ok {
		logging.Error("dispatcher: outbound not found", "outbound", tag, "destination", dest.String())
		link.Reader.Interrupt()
		link.Writer.CloseWithError(&OutboundError{Reason: "outbound " + tag + " not registered"})
		return
	}
	logging.Info("dispatcher: route selected",
		"destination", dest.String(), "outbound", tag, "route", via)

	start := time.Now()
	if err := handler.Process(ctx, link, dest); err != nil {
		// 失败原因随管道传给协议层，由协议层向对端发送 Error 帧。
		logging.Warn("dispatcher: outbound failed",
			"destination", dest.String(), "outbound", tag, "route", via,
			"err", err, "duration", time.Since(start))
		link.Reader.Interrupt()
		link.Writer.CloseWithError(err)
		return
	}
	logging.Debug("dispatcher: outbound finished",
		"destination", dest.String(), "outbound", tag, "route", via,
		"duration", time.Since(start))
}
