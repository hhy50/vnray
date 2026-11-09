package client

// 客户端运行时：主动连接服务端，接收服务端开启的流并本地分发。
// 服务端侧的接入逻辑在 internal/transport（Acceptor / Session / ForwardHub）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/secure"
	"github.com/hhy50/vnray/internal/tcp"
)

// DialOptions 是客户端拨号选项。
type DialOptions struct {
	Auth AuthOptions
	// Info 是客户端上报的自身配置与环境摘要。
	Info protocol.ClientInfo
	// ReportInfo 每次上报时采集最新信息；nil 时沿用 Info。
	ReportInfo func() protocol.ClientInfo
	// SessionConfig 覆盖多路复用层参数；nil 用默认值，须与服务端一致。
	SessionConfig *tcp.SessionConfig
	// Muxer 选择多路复用后端；nil 用 tcp.DefaultMuxer。必须与服务端一致
	// （线格式随后端而异）。
	Muxer tcp.Muxer
	// OnConfig 在收到服务端下发的全量配置快照时被调用（配置上报应答或下发帧）；
	// nil 表示客户端不维护服务端下发的配置。实现方负责并发安全。
	OnConfig func(cfg protocol.PushConfig)
	// Direct 是"直连"分发器：隧道帧带 Direct 标志时用它出站，跳过本客户端的路由
	// 规则（服务端出站规则 direct 名单命中）。nil 表示不支持直连，仍走 dispatcher。
	Direct dispatch.Dispatcher
}

// DialTimeout 是建立 TCP 连接的等待上限。
// 不断重连的前提是每次尝试都有终点：目标地址黑洞（丢包不回 RST）时，
// 无超时的 Dial 会永久挂住，重连循环也就再也不会有下一次尝试。
const DialTimeout = 10 * time.Second

// maxFramePayload 是单个 DataResult 分段帧的最大载荷。
const maxFramePayload = 32 * 1024

// Client 主动连接服务端，接收服务端发起的流并在本地代理分发。
type Client struct {
	session            tcp.Session
	dispatcher         dispatch.Dispatcher
	direct             dispatch.Dispatcher
	onConfig           func(protocol.PushConfig)
	configMu           sync.Mutex
	lastConfigStream   uint32
	lastConfigRevision uint64
	reportInfo         func() protocol.ClientInfo
	reportMu           sync.Mutex
}

// Dial 连接服务端、建立多路复用会话并完成认证握手；之后用 Serve 接收服务端发起的请求流。
func Dial(addr string, dispatcher dispatch.Dispatcher, opts DialOptions) (*Client, error) {
	return DialContext(context.Background(), addr, dispatcher, opts)
}

// DialContext establishes an authenticated session in the server-selected transport mode.
func DialContext(ctx context.Context, addr string, dispatcher dispatch.Dispatcher, opts DialOptions) (*Client, error) {
	key, err := authkey.Parse(opts.Auth.Key)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAuthRejected, err)
	}
	dialer := net.Dialer{Timeout: DialTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := secure.Client(ctx, raw, key)
	if err != nil {
		return nil, err
	}
	muxer := opts.Muxer
	if muxer == nil {
		muxer = tcp.DefaultMuxer()
	}
	session, err := muxer.Client(conn, sessionConfigOrDefault(opts.SessionConfig))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	var watcher sync.WaitGroup
	watcher.Add(1)
	reportCtx, finish := context.WithCancel(ctx)
	routine.Go("client.report.cancel", func() {
		defer watcher.Done()
		<-reportCtx.Done()
		if ctx.Err() != nil {
			_ = session.Close()
		}
	})
	reportInfo := opts.ReportInfo
	if reportInfo == nil {
		reportInfo = func() protocol.ClientInfo { return opts.Info }
	}
	c := &Client{session: session, dispatcher: dispatcher, direct: opts.Direct, onConfig: opts.OnConfig, reportInfo: reportInfo}
	err = c.reportConfig()
	finish()
	watcher.Wait()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) reportConfig() error {
	c.reportMu.Lock()
	defer c.reportMu.Unlock()
	stream, err := c.session.OpenStream()
	if err != nil {
		return fmt.Errorf("client: open config report stream: %w", err)
	}
	defer func() { _ = stream.Close() }()
	if err := stream.SetDeadline(time.Now().Add(AuthTimeout)); err != nil {
		return err
	}
	frame, err := protocol.NewConfigReportFrame(protocol.ConfigReportRequest{Info: c.reportInfo()})
	if err != nil {
		return err
	}
	if err := protocol.NewEncoder(stream).WriteFrame(frame); err != nil {
		return err
	}
	resp, err := protocol.NewDecoder(stream).ReadFrame()
	if err != nil {
		return err
	}
	if resp.Type != protocol.ConfigReportResult {
		_ = resp.CloseBody()
		return fmt.Errorf("%w: expected config report result, got %d", protocol.ErrProtocol, resp.Type)
	}
	payload, err := protocol.ReadPayload(resp)
	if err != nil {
		return err
	}
	res, err := protocol.ParseReply(payload)
	if err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("%w: %s", ErrAuthRejected, res.Reason)
	}
	if res.Config != nil {
		c.applyConfig(0, *res.Config)
	}
	return nil
}

const configReportInterval = 5 * time.Minute

func (c *Client) runConfigReports(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.session.CloseChan():
			return
		case <-ticker.C:
			if err := c.reportConfig(); err != nil {
				logging.Warn("client: config report failed", "err", err)
				_ = c.Close()
				return
			}
		}
	}
}

// Close 关闭底层多路复用会话，进而关闭 TCP 连接。
func (c *Client) Close() error { return c.session.Close() }

// Serve 接收服务端开启的流；会话结束后等待所有请求处理完成。
func (c *Client) Serve() error {
	return c.ServeContext(context.Background())
}

// ServeContext 服务到父级取消或会话结束，并等待全部流处理退出。
func (c *Client) ServeContext(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	routine.Go("client.session.cancel", func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
		case <-c.session.CloseChan():
		}
		cancel()
		_ = c.Close()
	})
	routine.Go("client.config.report", func() {
		defer wg.Done()
		c.runConfigReports(ctx, configReportInterval)
	})
	defer func() {
		cancel()
		_ = c.Close()
		wg.Wait()
	}()
	for {
		stream, err := c.session.AcceptStream()
		if err != nil {
			if err == io.EOF || c.session.IsClosed() {
				return nil
			}
			return err
		}
		wg.Add(1)
		routine.Go("client.stream", func() {
			defer wg.Done()
			c.handleStream(ctx, stream)
		})
	}
}

// handleStream 处理一条服务端开启的流。
//
// 流的第一帧决定这条流的用途：Ping 是心跳探测（回 Pong 后即结束），
// Data 是请求流（首帧的 Host/Port 为目标地址）。
func (c *Client) handleStream(ctx context.Context, stream tcp.Stream) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer stream.Close()
	remote := stream.RemoteAddr().String()
	sid := stream.ID()
	dec := protocol.NewDecoder(stream)
	enc := protocol.NewEncoder(stream)

	first, err := dec.ReadFrame()
	if err != nil {
		if !errors.Is(err, io.EOF) {
			logging.Warn("client: read first frame failed", "remote", remote, "stream_id", sid, "err", err)
		}
		return
	}
	if first.Type == protocol.Ping {
		c.handleHeartbeat(stream, enc, first)
		return
	}
	if first.Type == protocol.Tunnel {
		c.handleTunnel(ctx, stream, first)
		return
	}
	if first.Type == protocol.PortMap {
		c.handlePortMap(ctx, stream, first)
		return
	}
	if first.Type == protocol.ConfigPush {
		c.handleConfigPush(sid, first)
		return
	}
	logging.Warn("client: unexpected frame type on client link",
		"remote", remote, "stream_id", sid, "frame_type", uint16(first.Type))
}

// handleTunnel 处理一条隧道流：首帧带目标地址，之后该流承载裸字节。出口侧复用
// dispatcher 分发目标，因此 ACL 与路由规则照常生效；帧上带 Direct 标志时改用直连
// 分发器，跳过本客户端的路由规则（服务端出站规则的 direct 名单）。随后把两侧字节双向对接。
func (c *Client) handleTunnel(ctx context.Context, stream tcp.Stream, first *protocol.Frame) {
	remote := stream.RemoteAddr().String()
	sid := stream.ID()
	// 目标在载荷开头的地址块里，其后是 JSON 请求体。
	addr, req, err := protocol.ReadTunnelRequest(first.Body)
	if err != nil {
		logging.Warn("client: bad tunnel frame", "remote", remote, "stream_id", sid, "err", err)
		protocol.WriteReply(stream, protocol.TunnelResult, protocol.Reply{OK: false, Reason: "bad tunnel frame"})
		return
	}
	if err := first.CloseBody(); err != nil {
		logging.Warn("client: bad tunnel frame", "remote", remote, "stream_id", sid, "err", err)
		return
	}
	dest := dispatch.Destination{Host: addr.Host, Port: int(addr.Port)}
	if first.Flags&protocol.FlagUDP != 0 {
		dest.Network = "udp"
	}
	if !dest.IsValid() {
		protocol.WriteReply(stream, protocol.TunnelResult, protocol.Reply{OK: false, Reason: "invalid destination " + dest.String()})
		return
	}
	logging.Info("client: tunnel requested", "remote", remote, "stream_id", sid, "destination", dest.String())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dispatcher := c.dispatcher
	if req.Direct && c.direct != nil {
		dispatcher = c.direct
	}
	link, err := dispatcher.Dispatch(ctx, dest)
	if err != nil {
		logging.Warn("client: tunnel dispatch failed",
			"remote", remote, "stream_id", sid, "destination", dest.String(), "err", err)
		protocol.WriteReply(stream, protocol.TunnelResult, protocol.Reply{OK: false, Reason: errorReason(err)})
		return
	}
	defer link.Reader.Interrupt()
	defer link.Writer.Interrupt()
	var watcher sync.WaitGroup
	watcher.Add(1)
	defer func() { cancel(); watcher.Wait() }()
	routine.Go("client.tunnel.cancel", func() {
		defer watcher.Done()
		<-ctx.Done()
		link.Reader.Interrupt()
		link.Writer.Interrupt()
		_ = stream.Close()
	})

	protocol.WriteReply(stream, protocol.TunnelResult, protocol.Reply{OK: true})
	logging.Info("client: tunnel established", "remote", remote, "stream_id", sid, "destination", dest.String())

	// 方向分离：上行读尽（服务端对隧道 Close 发 FIN = 用户请求方向结束）只优雅关闭
	// link.Writer，把 FIN 传播成目标出站的上行 EOF，下载方向保留；对隧道流的 Close
	// （半关闭 FIN）只在服务端转发侧使用，客户端不对隧道流发 FIN。下行结束（响应搬完）
	// 才整体断开。
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			_ = stream.Close()
			_ = link.Writer.Close()
			link.Reader.Interrupt()
		})
	}
	// UDP 无半关闭：任一方向读完即整体收尾（TCP 依赖半关闭，只在下行结束时收尾）。
	udp := dest.Network == "udp"

	var wg sync.WaitGroup
	wg.Add(2)
	// 上行：外部客户端 → 目标。读尽（服务端对隧道发 FIN）只优雅关闭 link.Writer，
	// 把请求方向结束传播给出站，下行继续下载。
	routine.Go("client.tunnel.up", func() {
		defer wg.Done()
		_, _ = io.Copy(link.Writer, stream)
		_ = link.Writer.Close()
		if udp {
			teardown()
		}
	})
	// 下行：目标 → 外部客户端。响应搬完（真实连接结束）才整体断开。
	routine.Go("client.tunnel.down", func() {
		defer wg.Done()
		_, _ = io.Copy(stream, link.Reader)
		teardown()
	})
	wg.Wait()
	logging.Info("client: tunnel closed", "remote", remote, "stream_id", sid, "destination", dest.String())
}

// handleConfigPush 处理一条全量配置下发流：载荷为 PushConfig 快照，整体应用。
func (c *Client) handleConfigPush(streamID uint32, first *protocol.Frame) {
	if c.onConfig == nil {
		return
	}
	payload, err := protocol.ReadPayload(first)
	if err != nil {
		return
	}
	cfg, err := protocol.ParsePushConfig(payload)
	if err != nil {
		logging.Warn("client: bad push config frame", "err", err)
		return
	}
	c.applyConfig(streamID, cfg)
}

func (c *Client) applyConfig(streamID uint32, cfg protocol.PushConfig) {
	if c.onConfig == nil {
		return
	}
	c.configMu.Lock()
	defer c.configMu.Unlock()
	if cfg.Revision != 0 {
		if cfg.Revision <= c.lastConfigRevision {
			return
		}
		c.lastConfigRevision = cfg.Revision
	} else {
		if c.lastConfigRevision != 0 || (streamID != 0 && streamID <= c.lastConfigStream) {
			return
		}
		c.lastConfigStream = streamID
	}
	c.onConfig(cfg)
}

// handleHeartbeat 应答服务端的心跳探测：回一个 Pong，原样回显 Ping 载荷里的毫秒时间戳，
// 服务端据此配对并在自己一个时钟上算出往返时延。
//
// 不做任何额外工作：心跳只测链路往返，因此这里既不碰 dispatcher 也不碰 ACL。
// errorReason 从错误中提取对端可读的原因文本。
func errorReason(err error) string {
	var oe *dispatch.OutboundError
	if errors.As(err, &oe) {
		return oe.Reason
	}
	return err.Error()
}

func (c *Client) handleHeartbeat(stream tcp.Stream, enc *protocol.Encoder, ping *protocol.Frame) {
	remote := stream.RemoteAddr().String()
	sid := stream.ID()
	sent, err := protocol.ReadHeartbeatTimestamp(ping)
	if err != nil {
		logging.Warn("client: bad heartbeat frame", "remote", remote, "stream_id", sid, "err", err)
		return
	}
	if err := enc.WriteFrame(protocol.NewHeartbeatFrame(protocol.Pong, sent)); err != nil {
		logging.Debug("client: write pong failed", "remote", remote, "stream_id", sid, "err", err)
		return
	}
	logging.Debug("client: heartbeat replied", "remote", remote, "stream_id", sid, "timestamp_ms", sent)
}
