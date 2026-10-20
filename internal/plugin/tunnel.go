package plugin

// tunnel.go — v2ray 插件两条入站（vmess / vless）共用的隧道装配：绑定客户端查找、
// 限速/统计选项汇总、单连接搬运与 mux 主连接服务。两个协议只在请求头编码上不同，
// 从这里往下的数据面完全一致：一条入站连接对应一条隧道流（单连接）或一组子连接
// （mux 主连接），限速与统计都按 (协议, uuid) 口径。

import (
	"net"
	"time"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/identity"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/mux"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/vmess"
)

// relayDeps 汇总两个入站共用的装配件：用户表、会话表、限速与流量记账。
// proto 是本次连接的协议（vmess / vless），限速与流量统计都按它命名身份。
type relayDeps struct {
	users   UserStore
	exits   transport.ExitResolver
	limits  *ratelimit.Registry
	traffic *traffic.Meter
	proto   store.Protocol
}

// pumpSingle / pumpMux 是隧道泵的路径名（流量统计区分路径用）。
const (
	pumpSingle = "plugin.v2ray.tunnel"
	pumpMux    = "plugin.v2ray.mux"
)

// boundClient 查找绑定的出口（远程客户端会话或服务端本机出口）；未命中时记日志并
// 返回 nil，由调用方关闭入站连接。日志器已带 user/name 字段，这里不再重复传 user。
func (d relayDeps) boundClient(log *logging.Scoped, proto, client, target string) transport.Exit {
	exit, ok := d.exits.Resolve(client)
	if !ok {
		log.Warn(proto+": bound client offline, closing inbound conn",
			"client", client, "target", target)
		return nil
	}
	return exit
}

// options 汇总该 uuid 的限速与流量统计选项；跨 mux 子连接合计，与单连接共用
// 同一对桶。
func (d relayDeps) options(user, client string) transport.ProxyOptions {
	var o transport.ProxyOptions
	if d.limits != nil {
		pair := d.limits.Pair(ratelimit.Identity(d.proto, user))
		o.UpLimit, o.DownLimit, o.Stats = pair.Up, pair.Down, pair.Stats
	}
	if d.traffic != nil {
		o.Stats = d.traffic.UserStats(d.proto, user)
		o.ClientStats = d.traffic.ClientStats(client)
	}
	return o
}

// udpIdle 是 UDP 隧道无数据流动的关停兜底（DNS 等短会话几乎不触发；长静默才关停）。
const udpIdle = 2 * time.Minute

// relaySingle 单连接路径：开一条隧道双向搬运，返回时关闭入站连接与隧道。
// proto、network 与 origin 都取自入站解析结果，pump 用固定路径常量，只传这次连接
// 真正不同的参数。UDP 隧道按数据报搬运（无半关闭），TCP 沿用字节流半关闭泵。
func relaySingle(d relayDeps, log *logging.Scoped, id identity.Identity, target dispatch.Destination, network string, conn net.Conn) {
	proto := string(d.proto)
	tgt := target.String()
	sess := d.boundClient(log, proto, id.Client, tgt)
	if sess == nil {
		_ = conn.Close()
		return
	}
	// 用户级并发：占一个并发位再建隧道，超限排队。
	if d.limits != nil {
		gate := d.limits.Gate(ratelimit.Identity(d.proto, id.Key))
		if err := gate.Acquire(nil); err != nil {
			log.Warn(proto+": user concurrency limit reached", "client", id.Client, "target", tgt, "err", err)
			_ = conn.Close()
			return
		}
		defer gate.Release()
	}
	tunnel, err := sess.OpenTunnel(target, protocol.TunnelRequest{Origin: proto, Network: network})
	if err != nil {
		log.Warn(proto+": open tunnel failed", "client", id.Client, "target", tgt, "err", err)
		_ = conn.Close()
		return
	}
	log.Debug(proto+": tunnel established", "client", id.Client, "target", tgt, "network", network)
	if network == "udp" {
		transport.ProxyDatagramBidirectional(
			udpInbound(d.proto, conn), protocol.NewDatagramConn(tunnel),
			pumpSingle+".udp", udpIdle, d.options(id.Key, id.Client))
	} else {
		transport.ProxyBidirectional(conn, tunnel, pumpSingle, d.options(id.Key, id.Client))
	}
	_ = conn.Close()
	_ = tunnel.Close()
	log.Debug(proto+": tunnel closed", "client", id.Client, "target", tgt)
}

// udpInbound 把入站侧连接适配成数据报端点：vmess 的 chunk 边界天然是数据报（*vmess.Conn
// 原生实现 DatagramConn），vless 是 [2B len] 明文流，用 protocol.NewDatagramConn 包裹。
func udpInbound(proto store.Protocol, conn net.Conn) transport.DatagramConn {
	if proto == store.ProtocolVMess {
		return conn.(*vmess.Conn)
	}
	return protocol.NewDatagramConn(conn)
}

// serveMuxConn mux 主连接路径：把已认证的主连接交给 mux 服务端，每条子连接
// 各开一条隧道，真实目标在各子连接帧里。
func serveMuxConn(d relayDeps, log *logging.Scoped, id identity.Identity, conn net.Conn) {
	proto := string(d.proto)
	sess := d.boundClient(log, proto, id.Client, vmess.MuxCoolHost)
	if sess == nil {
		_ = conn.Close()
		return
	}
	srv := mux.NewServer(&muxTunnel{
		log: log, client: id.Client, sess: sess,
		limits: d.limits, identity: ratelimit.Identity(d.proto, id.Key),
		opts: d.options(id.Key, id.Client),
	})
	log.Info(proto+": mux session started", "client", id.Client, "target", vmess.MuxCoolHost)
	if err := srv.Serve(conn); err != nil {
		log.Warn(proto+": mux session ended",
			"client", id.Client, "target", vmess.MuxCoolHost, "err", err)
		return
	}
	log.Info(proto+": mux session closed", "client", id.Client, "target", vmess.MuxCoolHost)
}

// muxTunnel 实现 mux.Handler：为每条 mux 子连接开隧道并按 uuid 限速搬运。
type muxTunnel struct {
	log      *logging.Scoped
	client   string
	sess     transport.Exit
	limits   *ratelimit.Registry
	identity string
	opts     transport.ProxyOptions
}

// Open 开隧道并双向搬运；返回时子连接结束，mux.Conn 随之发出 End 帧。
// 建连失败时以带 OptionError 的 End 收尾，对端立即知道该子连接未成立。
func (m *muxTunnel) Open(target mux.Target, conn *mux.Conn) {
	// 用户级并发：每条 mux 子连接占一个并发位，超限排队。
	if m.limits != nil {
		if err := m.limits.Gate(m.identity).Acquire(nil); err != nil {
			m.log.Warn("mux: user concurrency limit reached", "client", m.client, "target", target.String(), "err", err)
			_ = conn.CloseWithError()
			return
		}
		defer m.limits.Gate(m.identity).Release()
	}
	tunnel, err := m.sess.OpenTunnel(
		dispatch.Destination{Host: target.Host, Port: target.Port, Network: target.Network},
		protocol.TunnelRequest{Origin: "vmess-mux", Network: target.Network})
	if err != nil {
		m.log.Warn("mux: open tunnel failed",
			"client", m.client, "target", target.String(), "err", err)
		_ = conn.CloseWithError()
		return
	}
	m.log.Info("mux: tunnel established", "client", m.client, "target", target.String(), "network", target.Network)
	if target.Network == "udp" {
		transport.ProxyDatagramBidirectional(
			conn, protocol.NewDatagramConn(tunnel), pumpMux+".udp", udpIdle, m.opts)
	} else {
		transport.ProxyBidirectional(conn, tunnel, pumpMux, m.opts)
	}
	_ = conn.Close()
	_ = tunnel.Close()
	m.log.Info("mux: tunnel closed", "client", m.client, "target", target.String())
}
