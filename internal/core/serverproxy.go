package core

// serverproxy.go — 服务端本机出口：在进程内用 ACL 守卫 + 路由 dispatcher 直接出站，
// 不起回环客户端。它与远程客户端会话同构（都实现 transport.Exit），由 exitResolver
// 按保留名 server:self 选路。开关与参数是运行期配置（config.ServerProxySettings），
// 管理台可热改。

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/acl"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/routing"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
)

// localExit 是服务端本机出口，实现 transport.Exit。
type localExit struct {
	dispatcher dispatch.Dispatcher
}

// buildLocalExit 按运行期配置构造本机出口：ACL 守卫包装路由 dispatcher，
// public/private 均为空时不设网段限制。构建即校验（ACL 网段解析失败返回错误）。
func buildLocalExit(sp config.ServerProxySettings) (*localExit, error) {
	guard, err := acl.NewGuard(sp.Public, sp.Private)
	if err != nil {
		return nil, fmt.Errorf("server proxy: build acl guard: %w", err)
	}
	return &localExit{dispatcher: guard.Wrap(routing.NewDispatcher(&sp.Routing))}, nil
}

// OpenTunnel 校验目标并出站：ACL/目标校验同步返回错误，拨号异步（与远程客户端一致，
// 失败随 Link 传播）。返回的 net.Conn 由调用方经 ProxyBidirectional 双向搬运。
func (e *localExit) OpenTunnel(dest dispatch.Destination, _ protocol.TunnelRequest) (net.Conn, error) {
	if !dest.IsValid() {
		return nil, fmt.Errorf("server proxy: invalid destination %s", dest)
	}
	ctx, cancel := context.WithCancel(context.Background())
	link, err := e.dispatcher.Dispatch(ctx, dest)
	if err != nil {
		cancel()
		return nil, err
	}
	return &linkConn{link: link, cancel: cancel}, nil
}

// linkConn 把 dispatcher 的 Link 适配成 net.Conn 供 ProxyBidirectional 搬运。
type linkConn struct {
	link   *dispatch.Link
	cancel context.CancelFunc
}

// Read 读出站侧回传的字节。
func (c *linkConn) Read(p []byte) (int, error) { return c.link.Reader.Read(p) }

// Write 写入出站侧的字节。
func (c *linkConn) Write(p []byte) (int, error) { return c.link.Writer.Write(p) }

// Close 只做半关闭（结束上行方向、保留读方向），语义等同 yamux 流的 Close。
func (c *linkConn) Close() error { return c.link.Writer.Close() }

// Abort 是异常收尾：取消出站 ctx 并中断读方向，避免长连接目标让下行泵永久阻塞。
func (c *linkConn) Abort() {
	c.cancel()
	c.link.Reader.Interrupt()
}

// 以下方法仅为满足 net.Conn；ProxyBidirectional 不调用它们。
func (c *linkConn) LocalAddr() net.Addr              { return linkAddr("local") }
func (c *linkConn) RemoteAddr() net.Addr             { return linkAddr("remote") }
func (c *linkConn) SetDeadline(time.Time) error      { return nil }
func (c *linkConn) SetReadDeadline(time.Time) error  { return nil }
func (c *linkConn) SetWriteDeadline(time.Time) error { return nil }

// linkAddr 是 linkConn 的占位地址。
type linkAddr string

func (a linkAddr) Network() string { return "link" }
func (a linkAddr) String() string  { return string(a) }

// exitResolver 把绑定客户端名解析成出口：保留名走本机出口，其余走远程会话。
// local 可在运行期原子替换（管理台改本机出口开关时），hub 与插件共享同一实例。
type exitResolver struct {
	remote transport.ExitResolver
	local  atomic.Pointer[localExit]
}

// SetLocal 原子替换本机出口；nil 表示停用（保留名不再命中）。
func (r *exitResolver) SetLocal(local *localExit) { r.local.Store(local) }

// Resolve 实现 transport.ExitResolver；本机出口未启用时保留名一律未命中，
// 不回落到远程会话（防止冒名会话被当成本机出口）。
func (r *exitResolver) Resolve(name string) (transport.Exit, bool) {
	if name == transport.LocalClientName {
		local := r.local.Load()
		if local == nil {
			return nil, false
		}
		return local, true
	}
	return r.remote.Resolve(name)
}

// buildExitResolver 组装出口解析器：settings 里本机出口启用时附带它。返回具体类型
// *exitResolver，供运行期热改（SetLocal）。
func buildExitResolver(settings config.ServiceSettings, sessions *transport.SessionManager) (*exitResolver, error) {
	r := &exitResolver{remote: sessions}
	if settings.ServerProxy.Enabled {
		local, err := buildLocalExit(settings.ServerProxy)
		if err != nil {
			return nil, err
		}
		r.local.Store(local)
	}
	return r, nil
}

// syncLocalClient 让 client_bindings 里的「本机」行与开关一致：启用则登记，停用则只删
// 注册行——直接走 store 而非 Bindings.DeleteClient，避免连带删除 client_traffic
// （本机出口的累计流量记在 client 维度，停用不该清零）。
func syncLocalClient(ctx context.Context, st store.Store, b *admin.Bindings, enabled bool) {
	if enabled {
		if b == nil {
			return
		}
		if err := b.SetClient(admin.ClientBinding{
			ClientKey:  transport.LocalClientName,
			ClientType: store.ClientTypeLocal,
		}); err != nil {
			logging.Warn("server proxy: register local exit failed", "err", err)
		}
		return
	}
	if _, err := st.DeleteClient(ctx, transport.LocalClientName); err != nil {
		logging.Warn("server proxy: remove local exit failed", "err", err)
	}
}
