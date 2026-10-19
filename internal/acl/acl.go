package acl

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/logging"
)

// resolveTimeout 是域名解析的超时；超时按"无法确认"处理并拒绝。
const resolveTimeout = 3 * time.Second

// Guard 是访问控制守卫，包装 Dispatcher 在分发前校验目标网段。
type Guard struct {
	public   []net.IPNet
	private  []net.IPNet
	resolver *net.Resolver
}

// NewGuard 按配置文本构造守卫；两个配置都为空时返回 nil（不限制）。
func NewGuard(publicText, privateText string) (*Guard, error) {
	public, err := config.ParseNetworkPatterns(publicText)
	if err != nil {
		return nil, fmt.Errorf("public: %w", err)
	}
	private, err := config.ParseNetworkPatterns(privateText)
	if err != nil {
		return nil, fmt.Errorf("private: %w", err)
	}
	if len(public) == 0 && len(private) == 0 {
		return nil, nil
	}
	return &Guard{
		public:  public,
		private: private,
		// PreferGo 避免部分平台 cgo 解析器的秒级耗时。
		resolver: &net.Resolver{PreferGo: true},
	}, nil
}

// Wrap 用守卫包装 next；g 为 nil 时原样返回 next。
func (g *Guard) Wrap(next dispatch.Dispatcher) dispatch.Dispatcher {
	if g == nil {
		return next
	}
	return &guardDispatcher{guard: g, next: next}
}

// Check 校验目标是否允许分发；错误信息作为 Error 帧原因发给对端。
func (g *Guard) Check(ctx context.Context, host string) error {
	if ip := net.ParseIP(host); ip != nil {
		return g.checkIP(host, ip)
	}
	return g.checkDomain(ctx, host)
}

func (g *Guard) checkIP(host string, ip net.IP) error {
	if reason := g.match(ip); reason != "" {
		logging.Warn("acl: destination rejected", "host", host, "reason", reason)
		return &dispatch.OutboundError{Reason: fmt.Sprintf("acl: %s denied: %s", host, reason)}
	}
	return nil
}

func (g *Guard) checkDomain(ctx context.Context, host string) error {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	ips, err := g.resolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		logging.Warn("acl: destination resolve failed", "host", host, "err", err)
		return &dispatch.OutboundError{Reason: fmt.Sprintf("acl: cannot resolve %s to verify network", host)}
	}
	for _, addr := range ips {
		if reason := g.match(addr.IP); reason != "" {
			logging.Warn("acl: destination rejected",
				"host", host, "ip", addr.IP.String(), "reason", reason)
			return &dispatch.OutboundError{Reason: fmt.Sprintf("acl: %s (%s) denied: %s", host, addr.IP, reason)}
		}
	}
	return nil
}

// match 返回拒绝原因；空串表示允许。private 优先于 public。
func (g *Guard) match(ip net.IP) string {
	for i := range g.private {
		if g.private[i].Contains(ip) {
			return "in private network " + g.private[i].String()
		}
	}
	if len(g.public) == 0 {
		return ""
	}
	for i := range g.public {
		if g.public[i].Contains(ip) {
			return ""
		}
	}
	return "not in public networks"
}

// guardDispatcher 是带访问控制的 Dispatcher 装饰器。
type guardDispatcher struct {
	guard *Guard
	next  dispatch.Dispatcher
}

func (d *guardDispatcher) Dispatch(ctx context.Context, dest dispatch.Destination) (*dispatch.Link, error) {
	if err := d.guard.Check(ctx, dest.Host); err != nil {
		return nil, err
	}
	return d.next.Dispatch(ctx, dest)
}
