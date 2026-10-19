package core

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/traffic"
	"github.com/hhy50/vnray/internal/transport"
)

// forwardHub 把已认证用户会话上的 Forward 流桥接到其绑定客户端（远程会话或服务端本机出口）的隧道；
// 它不自持监听（用户与客户端共用主端口），也没有独立生命周期——流关闭即结束。
// 出口按名字由 ExitResolver 解析，因此它不依赖会话登记表的具体实现。
type forwardHub struct {
	exits transport.ExitResolver
	// limits 是用户限速注册表；nil 时不限速。
	limits *ratelimit.Registry
	// traffic 是流量记账器；nil 时只统计到限速注册表的内存计数里。
	traffic *traffic.Meter
}

// handleUserStream 处理一条用户流：按帧类型分发——Ping 回 Pong 并续期等待上限后
// 继续读，Forward 校验目标、经绑定客户端开隧道并双向搬运，其余类型拒绝并结束流。
// 它在 OnStreamOpen 事件里被调用，阻塞该流的整个生命周期。
// log 是该会话的日志器（级别取自用户配置）；userKey 是握手时出示的用户密钥：
// 限速与统计都按它取桶（同一用户的所有隧道共享配额）。
func (h *forwardHub) handleUserStream(log *logging.Scoped, userKey, clientName string, stream tcp.Stream, stop <-chan struct{}) {
	remote := stream.RemoteAddr().String()

	dec := protocol.NewDecoder(stream)
	enc := protocol.NewEncoder(stream)

	_ = stream.SetDeadline(time.Now().Add(transport.TunnelTimeout))
	frame, err := dec.ReadFrame()
	if err != nil {
		if !errors.Is(err, io.EOF) {
			log.Warn("forward: read user frame failed",
				"remote", remote, "client", clientName, "user_key", userKey, "err", err)
		}
		return
	}
	switch frame.Type {
	case protocol.Ping:
		replyHeartbeat(log, remote, enc, frame)
	case protocol.Forward:
		h.handleForward(log, userKey, clientName, stream, frame, stop)
		return
	default:
		_ = frame.CloseBody()
		protocol.WriteReply(stream, protocol.ForwardResult, protocol.Reply{OK: false, Reason: "expect forward frame"})
		return
	}
}

// replyHeartbeat 校验 Ping 载荷并回 Pong（原样回显时间戳）；载荷校验失败不回包。
func replyHeartbeat(log *logging.Scoped, remote string, enc *protocol.Encoder, ping *protocol.Frame) {
	sent, err := protocol.ReadHeartbeatTimestamp(ping)
	if err != nil {
		log.Warn("forward: bad heartbeat frame", "remote", remote, "err", err)
		return
	}
	if err := enc.WriteFrame(protocol.NewHeartbeatFrame(protocol.Pong, sent)); err != nil {
		log.Debug("forward: write pong failed", "remote", remote, "err", err)
		return
	}
	log.Debug("forward: user heartbeat replied", "remote", remote, "timestamp_ms", sent)
}

// handleForward 处理 Forward 帧：目标在载荷开头的地址块里，校验后经绑定客户端
// 开隧道，随后该流承载裸字节直至任一侧关闭。
func (h *forwardHub) handleForward(log *logging.Scoped, userKey, clientName string, stream tcp.Stream, frame *protocol.Frame, stop <-chan struct{}) {
	remote := stream.RemoteAddr().String()
	// 用户级并发：占一个并发位再建隧道，超限排队；会话关闭即中断等待。
	if h.limits != nil && userKey != "" {
		if err := h.limits.Gate(ratelimit.Identity(store.ProtocolVNV1, userKey)).Acquire(stop); err != nil {
			protocol.WriteReply(stream, protocol.ForwardResult,
				protocol.Reply{OK: false, Reason: "user concurrency limit reached"})
			log.Warn("forward: user concurrency limit",
				"remote", remote, "client", clientName, "user_key", userKey, "err", err)
			return
		}
		defer h.limits.Gate(ratelimit.Identity(store.ProtocolVNV1, userKey)).Release()
	}
	addr, err := protocol.ReadAddress(frame.Body)
	if err != nil {
		_ = frame.CloseBody()
		protocol.WriteReply(stream, protocol.ForwardResult, protocol.Reply{OK: false, Reason: "bad forward address"})
		log.Warn("forward: read address failed",
			"remote", remote, "client", clientName, "user_key", userKey, "err", err)
		return
	}
	dest := dispatch.Destination{Host: addr.Host, Port: int(addr.Port)}
	if !dest.IsValid() {
		_ = frame.CloseBody()
		protocol.WriteReply(stream, protocol.ForwardResult, protocol.Reply{OK: false, Reason: fmt.Sprintf("invalid destination %s", dest)})
		log.Warn("forward: destination denied",
			"remote", remote, "client", clientName, "user_key", userKey, "destination", dest.String())
		return
	}
	_ = frame.CloseBody()

	exit, ok := h.exits.Resolve(clientName)
	if !ok {
		reason := fmt.Sprintf("client %q is offline", clientName)
		msg := "forward: bound exit unavailable"
		if clientName == transport.LocalClientName {
			reason = "server:self is unavailable (server_proxy disabled)"
			msg = "forward: server:self bound but server_proxy is disabled"
		}
		protocol.WriteReply(stream, protocol.ForwardResult, protocol.Reply{OK: false, Reason: reason})
		log.Warn(msg,
			"remote", remote, "client", clientName, "user_key", userKey, "destination", dest.String())
		return
	}

	tunnel, err := exit.OpenTunnel(dest, protocol.TunnelRequest{Origin: remote})
	if err != nil {
		protocol.WriteReply(stream, protocol.ForwardResult, protocol.Reply{OK: false, Reason: err.Error()})
		log.Warn("forward: open tunnel failed",
			"remote", remote, "client", clientName, "user_key", userKey, "destination", dest.String(), "err", err)
		return
	}
	defer func() { _ = tunnel.Close() }()

	protocol.WriteReply(stream, protocol.ForwardResult, protocol.Reply{OK: true})
	_ = stream.SetDeadline(time.Time{}) // 进入隧道：长连接不能带超时
	log.Info("forward: tunnel established",
		"remote", remote, "client", clientName, "user_key", userKey, "destination", dest.String())
	// 用户限速与统计：上行限用户侧 reader（用户发多快），下行限客户端侧 reader
	// （客户端送多快）；字节按 user_key 与 client 两个维度各记一份。后台改限速即时
	// 生效（桶是活的，泵拿的是指针）。
	var o transport.ProxyOptions
	if h.limits != nil && userKey != "" {
		pair := h.limits.Pair(ratelimit.Identity(store.ProtocolVNV1, userKey))
		o.UpLimit, o.DownLimit, o.Stats = pair.Up, pair.Down, pair.Stats
	}
	if h.traffic != nil {
		o.Stats = h.traffic.UserStats(store.ProtocolVNV1, userKey)
		o.ClientStats = h.traffic.ClientStats(clientName)
	}
	transport.ProxyBidirectional(stream, tunnel, "core.forward.pump", o)
	log.Info("forward: tunnel closed",
		"remote", remote, "client", clientName, "user_key", userKey, "destination", dest.String())
}
