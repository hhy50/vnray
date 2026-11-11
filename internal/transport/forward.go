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
	"github.com/hhy50/vnray/internal/stats"
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
	UserFor(ctx context.Context, key string) (access UserAccess, ok bool)
}

// ClientRuntime 是握手与配置下发要用的客户端运行参数。三者同源同表，合并成一次读取，
// 免得握手路径为每项各查一次库。
type ClientRuntime struct {
	// LogLevel 是服务端下发的日志级别，空串视为 info。
	LogLevel string
	// Concurrency 是该客户端的并发上限，0 表示不限。
	Concurrency int
	// Push 是随应答下发的分层配置快照。
	Push protocol.PushConfig
}

// OpenTunnel 在会话上开一条隧道流：发 Tunnel 帧（目标在帧头）并等出口侧的 TunnelResult，
// 成功后返回裸字节流，由调用方双向搬运并负责关闭。若配置了对本客户端的并发上限，
// 超限时排队等待空位（会话关闭即中断）。
func (s *Session) OpenTunnel(dest dispatch.Destination, req protocol.TunnelRequest) (net.Conn, error) {
	return s.OpenTunnelContext(context.Background(), dest, req)
}

// OpenTunnelContext 在请求取消时终止并发排队及隧道握手。
func (s *Session) OpenTunnelContext(ctx context.Context, dest dispatch.Destination, req protocol.TunnelRequest) (net.Conn, error) {
	if !dest.IsValid() {
		return nil, fmt.Errorf("transport: invalid destination %s", dest)
	}
	frame, err := protocol.NewTunnelFrame(req, protocol.Address{Host: dest.Host, Port: uint16(dest.Port)})
	if err != nil {
		return nil, err
	}
	return s.openRelayStream(ctx, frame)
}

// OpenPortMap 在会话上开一条端口映射流：发 PortMap 帧（目标为客户端侧的本地拨号
// 地址），客户端本地直连、不经其 dispatcher（映射规则由服务端运维下发，不走客户端
// 的 ACL 与路由）。应答复用 TunnelResult，成功后返回裸字节流。并发上限同 OpenTunnel。
func (s *Session) OpenPortMap(dest dispatch.Destination) (net.Conn, error) {
	return s.OpenPortMapContext(context.Background(), dest)
}

// OpenPortMapContext 在请求取消时终止端口映射排队及握手。
func (s *Session) OpenPortMapContext(ctx context.Context, dest dispatch.Destination) (net.Conn, error) {
	if !dest.IsValid() {
		return nil, fmt.Errorf("transport: invalid destination %s", dest)
	}
	frame, err := protocol.NewPortMapFrame(protocol.Address{Host: dest.Host, Port: uint16(dest.Port)})
	if err != nil {
		return nil, err
	}
	return s.openRelayStream(ctx, frame)
}

// openRelayStream 是 OpenTunnel / OpenPortMap 的共用骨架：先占并发位（客户端级），
// 再开流、发请求帧、等 TunnelResult，建立后清掉握手期 deadline 并返回裸字节流，
// 由调用方负责关闭（关闭时归还并发位）。
func (s *Session) openRelayStream(ctx context.Context, frame *protocol.Frame) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan struct{})
	var watcher sync.WaitGroup
	watcher.Add(1)
	var pending atomic.Pointer[tcp.Stream]
	routine.Go("transport.tunnel.cancel", func() {
		defer watcher.Done()
		select {
		case <-ctx.Done():
		case <-s.CloseSignal():
			cancel()
		case <-finished:
			return
		}
		if stream := pending.Load(); stream != nil {
			abortConnection(*stream)
		}
	})
	var finishOnce sync.Once
	finish := func() { finishOnce.Do(func() { close(finished); watcher.Wait() }) }
	defer finish()
	if err := s.gate.AcquireContext(ctx); err != nil {
		return nil, fmt.Errorf("transport: client concurrency limit: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			s.gate.Release()
		}
	}()

	opened, err := tcp.OpenStreamContext(ctx, s.session)
	if err != nil {
		return nil, fmt.Errorf("transport: open tunnel stream: %w", err)
	}
	if err := opened.SetDeadline(time.Now().Add(TunnelTimeout)); err != nil {
		abortConnection(opened)
		return nil, fmt.Errorf("transport: set tunnel deadline: %w", err)
	}
	stream := opened
	pending.Store(&stream)
	if err := ctx.Err(); err != nil {
		abortConnection(stream)
		return nil, err
	}
	if err := protocol.NewEncoder(stream).WriteFrame(frame); err != nil {
		abortConnection(stream)
		return nil, fmt.Errorf("transport: write tunnel frame: %w", err)
	}

	if err := awaitTunnelResult(stream); err != nil {
		abortConnection(stream)
		return nil, err
	}
	finish()
	if err := ctx.Err(); err != nil {
		abortConnection(stream)
		return nil, err
	}
	// 隧道建立后清掉 deadline：长连接不能被握手期的超时打断。
	if err := stream.SetDeadline(time.Time{}); err != nil {
		abortConnection(stream)
		return nil, fmt.Errorf("transport: clear tunnel deadline: %w", err)
	}
	ok = true
	// 返回的流在关闭时归还并发位，保证拥堵位跟随隧道的整个生命周期。
	return &gateStream{Stream: stream, release: s.gate.Release}, nil
}

// gateStream 的 CloseWrite 只发 FIN，Close 才结束整个隧道并归还并发位。
type gateStream struct {
	tcp.Stream
	once    sync.Once
	release func()
}

func (g *gateStream) Close() error {
	_ = g.Stream.SetDeadline(time.Now())
	err := g.Stream.Close()
	g.once.Do(g.release)
	return err
}

// CloseWrite 结束上行，保留下行与并发位。
func (g *gateStream) CloseWrite() error { return g.Stream.Close() }

// Abort 中断两个方向并归还并发位。
func (g *gateStream) Abort() { _ = g.Close() }

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

// PumpStats 是泵的字节统计；类型定义在叶子包 stats，因为限速注册表也要持有它，
// 而 ratelimit 反向依赖 transport 会把依赖方向带偏。
type PumpStats = stats.PumpStats

// ProxyOptions 是双向泵的可选参数：两个方向的限速器与字节统计。
// 限速按用户要求挂在各方向的**读端**：上行限用户侧 reader（用户发多快），
// 下行限客户端侧 reader（客户端送多快）——不读，TCP 背压才会真正传回源头。
// Stats 与 ClientStats 是同一份字节的两个视角：前者记到用户头上，后者记到
// 出口客户端头上，方向约定一致（Up = a→b，Down = b→a）。
type ProxyOptions struct {
	// Context 为 nil 时不绑定外部生命周期。
	Context     context.Context
	UpLimit     Limiter
	DownLimit   Limiter
	Stats       *PumpStats
	ClientStats *PumpStats
}

// 隧道转发量与并发数：管理台图表的采样数据源，atomic 免锁、热路径只付一次加法。
var (
	tunnelUp        atomic.Int64
	tunnelDown      atomic.Int64
	tunnelTotal     atomic.Int64
	pumpAllocations atomic.Uint64
	tunnelActive    atomic.Int64
)

// TunnelStats 返回累计转发字节数与当前活跃隧道数。
func TunnelStats() (traffic int64, active int64) {
	return tunnelUp.Load() + tunnelDown.Load(), tunnelActive.Load()
}

// pumpBufSize 是泵的搬运块大小：16KiB 在低速率下节流更平滑，高速率下系统调用次数也可接受。
const pumpBufSize = 16 * 1024

// writeAll 完整写入 b，避免底层 Writer 部分写。
func writeAll(w io.Writer, b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		n, err := w.Write(b)
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
		b = b[n:]
	}
	return total, nil
}

// pumpBufs 池化搬运缓冲：泵是常驻热路径，逐块分配是纯浪费。
var pumpBufs = sync.Pool{
	New: func() any { b := make([]byte, pumpBufSize); pumpAllocations.Add(1); return &b },
}

// pipeline 是一条单向搬运管线：从 src 池化读 → （可选）限速 → 写 dst，并累计字节。
type pipeline struct {
	name      string
	src       io.Reader
	dst       io.Writer
	limiter   Limiter
	readCnt   *atomic.Int64
	writeCnt  *atomic.Int64
	clientCnt *atomic.Int64
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
			written, werr := writeAll(p.dst, buf[:n])
			if p.readCnt != nil {
				p.readCnt.Add(int64(n))
			}
			if written > 0 {
				if p.writeCnt != nil {
					p.writeCnt.Add(int64(written))
				}
				if p.clientCnt != nil {
					p.clientCnt.Add(int64(written))
				}
				p.globalCnt.Add(int64(written))
			}
			if werr != nil {
				return
			}
		}
		if rerr != nil {
			clean = rerr == io.EOF
			return
		}
	}
}

// aborter 在异常收尾时中断两个方向，与正常 EOF 的半关闭分开。
type aborter interface{ Abort() }

func abortConnection(conn io.Closer) {
	if ab, ok := conn.(aborter); ok {
		ab.Abort()
		return
	}
	if c, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
		_ = c.SetDeadline(time.Now())
	}
	_ = conn.Close()
}

func closeWrite(conn io.Closer) {
	if c, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = c.CloseWrite()
		return
	}
	_ = conn.Close()
}

// ProxyBidirectional 双向搬运 a、b 之间的字节（调用方约定 **a 是用户侧、b 是客户端侧**）：
// up 管线读 a 写 b（用户上传，限 a 的 reader），down 管线读 b 写 a（用户下载，限 b 的
// reader）。方向分离，**不把任一方向 EOF 升级成整条断开**：
//   - up 干净读尽 → 半关闭 b 的写方向，保留下行和客户端并发位。
//   - down 干净读尽 → 半关闭 a 的写方向；VMess 由 Close 发出带内终结块。
//   - 任一方向出错或请求取消 → 中断两端；两个方向退出后最终关闭并归还资源。
//
// 两个方向都退出才返回；期间维护隧道并发与流量总计数。opts 可选限速与统计。
func ProxyBidirectional(a, b io.ReadWriteCloser, name string, opts ...ProxyOptions) {
	var o ProxyOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	tunnelTotal.Add(1)
	tunnelActive.Add(1)
	defer tunnelActive.Add(-1)

	parent := o.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var abrupt sync.Once
	abruptClose := func() {
		abrupt.Do(func() {
			cancel()
			abortConnection(a)
			abortConnection(b)
		})
	}
	// 请求取消即中断两端；AfterFunc 只在真取消时才起协程，不必为每条隧道常驻一个观察者。
	stopCancelWatch := context.AfterFunc(ctx, abruptClose)
	defer stopCancelWatch()

	up := &pipeline{
		name: name + ".up", src: a, dst: b,
		limiter: o.UpLimit, readCnt: o.Stats.UpReadRef(), writeCnt: o.Stats.UpWriteRef(),
		clientCnt: o.ClientStats.UpWriteRef(),
		globalCnt: &tunnelUp,
		teardown:  func() { closeWrite(b) },
		abort:     abruptClose,
	}
	down := &pipeline{
		name: name + ".down", src: b, dst: a,
		limiter: o.DownLimit, readCnt: o.Stats.DownReadRef(), writeCnt: o.Stats.DownWriteRef(),
		clientCnt: o.ClientStats.DownWriteRef(),
		globalCnt: &tunnelDown,
		teardown:  func() { closeWrite(a) },
		abort:     abruptClose,
	}
	var wg sync.WaitGroup
	wg.Add(2)
	routine.Go(up.name, func() { defer wg.Done(); up.run(ctx) })
	routine.Go(down.name, func() { defer wg.Done(); down.run(ctx) })
	wg.Wait()
	abruptClose()
}

// TransferInfo 是全部 TCP/UDP 泵的进程内累计值，不读取业务锁或持久层。
type TransferInfo struct {
	Requests             int64  `json:"requests"`
	Active               int64  `json:"active"`
	UploadBytes          int64  `json:"upload_bytes"`
	DownloadBytes        int64  `json:"download_bytes"`
	BufferAllocations    uint64 `json:"buffer_allocations"`
	BufferAllocatedBytes uint64 `json:"buffer_allocated_bytes"`
}

// TransferSnapshot 返回已进入搬运阶段的请求与成功写入的载荷字节，不含协议开销。
func TransferSnapshot() TransferInfo {
	allocations := pumpAllocations.Load()
	return TransferInfo{Requests: tunnelTotal.Load(), Active: tunnelActive.Load(), UploadBytes: tunnelUp.Load(), DownloadBytes: tunnelDown.Load(), BufferAllocations: allocations, BufferAllocatedBytes: allocations * pumpBufSize}
}
