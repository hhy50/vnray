package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/tcp"
)

// TunnelTimeout 是隧道建立阶段的等待上限（出口拨号 + 回结果）；建立后不再设超时。
const TunnelTimeout = 5 * time.Second

// ErrTunnelRejected 表示出口侧没能建立隧道（拨号失败、被 ACL 拒绝等）。
var ErrTunnelRejected = errors.New("tunnel rejected")

// UserAccess 是一条 user_key 解析出的接入信息。
type UserAccess struct {
	// Client 是绑定的客户端名，用于选路。
	Client string
	// Name 是用户的管理台显示名，只用于日志。
	Name string
	// Active 为 false 表示用户已停用：认得出是谁，但不允许接入。
	Active bool
}

// UserRegistry 把用户出示的 user_key 解析成接入信息。未注册或读库失败返回 false（握手即拒）；
// 已停用的用户返回 ok=true 但 Active=false，让调用方能给出"已停用"而不是"未注册"的应答
// （由 admin.Bindings 实现）。user_key 既是认证凭据也是选路依据。
type UserRegistry interface {
	UserFor(key string) (access UserAccess, ok bool)
}

// OpenTunnel 在会话上开一条隧道流：发 Tunnel 帧（目标在帧头）并等出口侧的 TunnelResult，
// 成功后返回裸字节流，由调用方双向搬运并负责关闭。若配置了对本客户端的并发上限，
// 超限时排队等待空位（会话关闭即中断）。
func (s *Session) OpenTunnel(dest dispatch.Destination, req protocol.TunnelRequest) (net.Conn, error) {
	if !dest.IsValid() {
		return nil, fmt.Errorf("transport: invalid destination %s", dest)
	}
	frame, err := protocol.NewTunnelFrame(req, protocol.Address{Host: dest.Host, Port: uint16(dest.Port)})
	if err != nil {
		return nil, err
	}
	return s.openRelayStream(frame)
}

// OpenPortMap 在会话上开一条端口映射流：发 PortMap 帧（目标为客户端侧的本地拨号
// 地址），客户端本地直连、不经其 dispatcher（映射规则由服务端运维下发，不走客户端
// 的 ACL 与路由）。应答复用 TunnelResult，成功后返回裸字节流。并发上限同 OpenTunnel。
func (s *Session) OpenPortMap(dest dispatch.Destination) (net.Conn, error) {
	if !dest.IsValid() {
		return nil, fmt.Errorf("transport: invalid destination %s", dest)
	}
	frame, err := protocol.NewPortMapFrame(protocol.Address{Host: dest.Host, Port: uint16(dest.Port)})
	if err != nil {
		return nil, err
	}
	return s.openRelayStream(frame)
}

// openRelayStream 是 OpenTunnel / OpenPortMap 的共用骨架：先占并发位（客户端级），
// 再开流、发请求帧、等 TunnelResult，建立后清掉握手期 deadline 并返回裸字节流，
// 由调用方负责关闭（关闭时归还并发位）。
func (s *Session) openRelayStream(frame *protocol.Frame) (net.Conn, error) {
	if err := s.gate.Acquire(s.CloseSignal()); err != nil {
		return nil, fmt.Errorf("transport: client concurrency limit: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			s.gate.Release()
		}
	}()

	stream, err := s.session.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("transport: open tunnel stream: %w", err)
	}
	if err := stream.SetDeadline(time.Now().Add(TunnelTimeout)); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("transport: set tunnel deadline: %w", err)
	}

	if err := protocol.NewEncoder(stream).WriteFrame(frame); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("transport: write tunnel frame: %w", err)
	}

	if err := awaitTunnelResult(stream); err != nil {
		_ = stream.Close()
		return nil, err
	}
	// 隧道建立后清掉 deadline：长连接不能被握手期的超时打断。
	if err := stream.SetDeadline(time.Time{}); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("transport: clear tunnel deadline: %w", err)
	}
	ok = true
	// 返回的流在关闭时归还并发位，保证拥堵位跟随隧道的整个生命周期。
	return &gateStream{Stream: stream, release: s.gate.Release}, nil
}

// gateStream 包装隧道流：Close 时归还客户端级并发位（幂等）。
type gateStream struct {
	tcp.Stream
	once    sync.Once
	release func()
}

func (g *gateStream) Close() error {
	err := g.Stream.Close()
	g.once.Do(g.release)
	return err
}

// awaitTunnelResult 读流上的 TunnelResult 应答并判定成败；失败时返回可读错误。
func awaitTunnelResult(stream tcp.Stream) error {
	resp, err := protocol.NewDecoder(stream).ReadFrame()
	if err != nil {
		return fmt.Errorf("transport: read tunnel result: %w", err)
	}
	if resp.Type != protocol.TunnelResult {
		_ = resp.CloseBody()
		return fmt.Errorf("%w: unexpected frame %d on tunnel stream", protocol.ErrProtocol, resp.Type)
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
		return fmt.Errorf("%w: %s", ErrTunnelRejected, res.Reason)
	}
	return nil
}

// Limiter 在转发前等待字节额度，连接结束时由 context 取消等待。
type Limiter interface {
	Wait(ctx context.Context, n int) error
}

// PumpStats 汇集双向搬运的字节统计：四个计数都是原子的，可跨协程读写。
// Up* 对应 a→b 方向（调用方约定 a 是用户侧），Down* 对应 b→a。
type PumpStats struct {
	UpRead    atomic.Int64 // 从 a（用户侧）读到的字节
	UpWrite   atomic.Int64 // 写到 b（客户端侧）的字节
	DownRead  atomic.Int64 // 从 b（客户端侧）读到的字节
	DownWrite atomic.Int64 // 写到 a（用户侧）的字节
}

// 四个 nil 安全的取址方法：Stats 为 nil（未启用统计）时管线拿到空指针、直接跳过计数。

func (s *PumpStats) upRead() *atomic.Int64 {
	if s == nil {
		return nil
	}
	return &s.UpRead
}

func (s *PumpStats) upWrite() *atomic.Int64 {
	if s == nil {
		return nil
	}
	return &s.UpWrite
}

func (s *PumpStats) downRead() *atomic.Int64 {
	if s == nil {
		return nil
	}
	return &s.DownRead
}

func (s *PumpStats) downWrite() *atomic.Int64 {
	if s == nil {
		return nil
	}
	return &s.DownWrite
}

// ProxyOptions 是双向泵的可选参数：两个方向的限速器与字节统计。
// 限速按用户要求挂在各方向的**读端**：上行限用户侧 reader（用户发多快），
// 下行限客户端侧 reader（客户端送多快）——不读，TCP 背压才会真正传回源头。
// Stats 与 ClientStats 是同一份字节的两个视角：前者记到用户头上，后者记到
// 出口客户端头上，方向约定一致（Up = a→b，Down = b→a）。
type ProxyOptions struct {
	UpLimit     Limiter    // a→b 方向（用户上传）
	DownLimit   Limiter    // b→a 方向（用户下载）
	Stats       *PumpStats // 用户维度字节统计（可选）
	ClientStats *PumpStats // 客户端维度字节统计（可选）
}

// 隧道转发量与并发数：管理台图表的采样数据源，atomic 免锁、热路径只付一次加法。
var (
	tunnelTraffic atomic.Int64
	tunnelActive  atomic.Int64
)

// TunnelStats 返回累计转发字节数与当前活跃隧道数。
func TunnelStats() (traffic int64, active int64) {
	return tunnelTraffic.Load(), tunnelActive.Load()
}

// pumpBufSize 是泵的搬运块大小：16KiB 在低速率下节流更平滑，高速率下系统调用次数也可接受。
const pumpBufSize = 16 * 1024

// writeAll 完整写入 b，避免底层 Writer 部分写。
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// pumpBufs 池化搬运缓冲：泵是常驻热路径，逐块分配是纯浪费。
var pumpBufs = sync.Pool{
	New: func() any { b := make([]byte, pumpBufSize); return &b },
}

// pipeline 是一条单向搬运管线：从 src 池化读 → （可选）限速 → 写 dst，并累计字节。
type pipeline struct {
	name      string
	src       io.Reader
	dst       io.Writer
	limiter   Limiter
	readCnt   *atomic.Int64 // 可为 nil
	writeCnt  *atomic.Int64 // 可为 nil
	clientCnt *atomic.Int64 // 可为 nil
	globalCnt *atomic.Int64
	// teardown 是这一方向干净结束（源读到 EOF）后的收尾；abort 是出错后的收尾。
	teardown func()
	abort    func()
}

// run 跑到源 EOF（干净结束，走 teardown）或出错（走 abort）为止。
func (p *pipeline) run(ctx context.Context) {
	clean := false
	defer func() {
		if clean {
			p.teardown()
		} else {
			p.abort()
		}
	}()
	bufp := pumpBufs.Get().(*[]byte)
	defer pumpBufs.Put(bufp)
	buf := (*bufp)[:pumpBufSize]
	for {
		n, rerr := p.src.Read(buf)
		if n > 0 {
			if p.limiter != nil {
				if err := p.limiter.Wait(ctx, n); err != nil {
					return
				}
			}
			werr := writeAll(p.dst, buf[:n])
			if p.readCnt != nil {
				p.readCnt.Add(int64(n))
			}
			if werr == nil {
				if p.writeCnt != nil {
					p.writeCnt.Add(int64(n))
				}
				if p.clientCnt != nil {
					p.clientCnt.Add(int64(n))
				}
				p.globalCnt.Add(int64(n))
			} else {
				return
			}
		}
		if rerr != nil {
			clean = rerr == io.EOF
			return
		}
	}
}

// aborter 由出口端（b）可选实现：异常收尾时用它区分"干净半关闭"（Close 保留读方向）
// 与"立刻全收尾"（Abort 一并中断读方向），供本机出口这类非 yamux 出口使用。
type aborter interface{ Abort() }

// ProxyBidirectional 双向搬运 a、b 之间的字节（调用方约定 **a 是用户侧、b 是客户端侧**）：
// up 管线读 a 写 b（用户上传，限 a 的 reader），down 管线读 b 写 a（用户下载，限 b 的
// reader）。方向分离，**不把任一方向 EOF 升级成整条断开**：
//   - up 干净读尽（用户请求方向结束）→ Close b（隧道流）；yamux 的 Close 即半关闭：
//     发 FIN 通知客户端，读方向保留，下载继续。
//   - down 干净读尽（真实连接结束）→ Close a（用户侧）；VMess 连接的带内终结块
//     随 Close 一并发出（见 vmess.Conn），用户据此收到响应结束信号。
//   - 任一方向出错才走 abrupt：两端一起 Close。
//
// 两个方向都退出才返回；期间维护隧道并发与流量总计数。opts 可选限速与统计。
func ProxyBidirectional(a, b io.ReadWriteCloser, name string, opts ...ProxyOptions) {
	var o ProxyOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	tunnelActive.Add(1)
	defer tunnelActive.Add(-1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var abrupt sync.Once
	abruptClose := func() {
		abrupt.Do(func() {
			cancel()
			_ = a.Close()
			// b 可选实现 Abort：本机出口需要在此中断读方向，否则长连接目标会让下行泵永久阻塞。
			if ab, ok := b.(aborter); ok {
				ab.Abort()
			} else {
				_ = b.Close()
			}
		})
	}

	up := &pipeline{
		name: name + ".up", src: a, dst: b,
		limiter: o.UpLimit, readCnt: o.Stats.upRead(), writeCnt: o.Stats.upWrite(),
		clientCnt: o.ClientStats.upWrite(),
		globalCnt: &tunnelTraffic,
		teardown:  func() { _ = b.Close() },
		abort:     abruptClose,
	}
	down := &pipeline{
		name: name + ".down", src: b, dst: a,
		limiter: o.DownLimit, readCnt: o.Stats.downRead(), writeCnt: o.Stats.downWrite(),
		clientCnt: o.ClientStats.downWrite(),
		globalCnt: &tunnelTraffic,
		teardown:  func() { _ = a.Close() },
		abort:     abruptClose,
	}
	var wg sync.WaitGroup
	wg.Add(2)
	routine.Go(up.name, func() { defer wg.Done(); up.run(ctx) })
	routine.Go(down.name, func() { defer wg.Done(); down.run(ctx) })
	wg.Wait()
	abruptClose()
}
