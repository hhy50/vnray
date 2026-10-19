package client

import (
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/tcp"
)

// portMapDialTimeout 是本地拨号的等待上限；服务端只等 TunnelTimeout（5s），
// 留出帧往返余量后取 4s。
const portMapDialTimeout = 4 * time.Second

// handlePortMap 处理一条端口映射流：首帧载荷为地址块（客户端侧本地目标），
// 本地拨号成功回 OK 的 TunnelResult，之后该流承载裸字节直至任一侧关闭。
func (c *Client) handlePortMap(stream tcp.Stream, first *protocol.Frame) {
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
	local, err := net.DialTimeout("tcp", target, portMapDialTimeout)
	if err != nil {
		logging.Warn("client: portmap local dial failed", "remote", remote, "stream_id", sid, "target", target, "err", err)
		protocol.WriteReply(stream, protocol.TunnelResult, protocol.Reply{OK: false, Reason: err.Error()})
		return
	}
	defer func() { _ = local.Close() }()

	protocol.WriteReply(stream, protocol.TunnelResult, protocol.Reply{OK: true})
	logging.Info("client: portmap established", "remote", remote, "stream_id", sid, "target", target)

	// 方向分离同 handleTunnel：上行读尽（服务端对映射流发 FIN = 访问方请求方向结束）
	// 只半关本地连接的写侧，响应方向保留；下行结束（本地服务回完）才整体断开。
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(local, stream)
		closeWrite(local)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(stream, local)
		_ = stream.Close()
	}()
	wg.Wait()
	logging.Info("client: portmap closed", "remote", remote, "stream_id", sid, "target", target)
}

// closeWrite 半关 TCP 连接的写侧（发 FIN 保留读）；非 TCP 连接整体关闭。
func closeWrite(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
		return
	}
	_ = conn.Close()
}
