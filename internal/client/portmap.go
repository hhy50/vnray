package client

import (
	"context"
	"net"
	"strconv"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/tcp"
)

// handlePortMap 处理一条端口映射流：首帧载荷为地址块（客户端侧本地目标），
// 本地拨号成功回 OK 的 TunnelResult，之后该流承载裸字节直至任一侧关闭。
func (c *Client) handlePortMap(ctx context.Context, stream tcp.Stream, first *protocol.Frame) {
	remote := stream.RemoteAddr().String()
	sid := stream.ID()
	addr, err := protocol.ReadAddress(first.Body)
	if err != nil {
		_ = first.CloseBody()
		logging.Warn("client: bad portmap frame", "remote", remote, "stream_id", sid, "err", err)
		protocol.WriteReply(stream, protocol.TunnelResult, protocol.Reply{OK: false, Reason: "bad portmap frame"})
		return
	}
	if err := first.CloseBody(); err != nil {
		logging.Warn("client: bad portmap frame", "remote", remote, "stream_id", sid, "err", err)
		return
	}
	dest := dispatch.Destination{Host: addr.Host, Port: int(addr.Port)}
	if !dest.IsValid() {
		protocol.WriteReply(stream, protocol.TunnelResult, protocol.Reply{OK: false, Reason: "invalid destination " + dest.String()})
		return
	}
	target := net.JoinHostPort(addr.Host, strconv.Itoa(int(addr.Port)))
	local, err := (&net.Dialer{Timeout: targetDialTimeout}).DialContext(ctx, "tcp", target)
	if err != nil {
		logging.Warn("client: portmap local dial failed", "remote", remote, "stream_id", sid, "target", target, "err", err)
		protocol.WriteReply(stream, protocol.TunnelResult, protocol.Reply{OK: false, Reason: err.Error()})
		return
	}
	defer func() { _ = local.Close() }()

	if err := writeTunnelSuccess(stream); err != nil {
		return
	}
	logging.Info("client: portmap established", "remote", remote, "stream_id", sid, "target", target)

	relayStream(ctx, stream, local, false)
	logging.Info("client: portmap closed", "remote", remote, "stream_id", sid, "target", target)
}
