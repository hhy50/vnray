package transport

// udp_pump.go — datagram 双向搬运：与 ProxyBidirectional 同构，但每方向只搬运完整
// 数据报（不拆、不拼），且 UDP 无半关闭语义——任一方向读到源 EOF/出错即整体关闭两端，
// 另加空闲超时兜底关停。

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hhy50/vnray/internal/routine"
)

// DatagramConn 是一个双向数据报端点，读出一个完整 datagram、写入一个完整 datagram。
// 与 protocol.DatagramConn 同形；transport 不依赖 protocol，故在此各自声明。
type DatagramConn interface {
	ReadPacket() ([]byte, error)
	WritePacket([]byte) error
	Close() error
}

// ProxyDatagramBidirectional 在两个 datagram 端点之间双向搬运（调用方约定 a 是用户侧、
// b 是客户端侧）。up 管线读 a 写 b，down 管线读 b 写 a。任一方向读到源 EOF 或出错、
// 或超过 idle 无数据流动时，整体关闭两端返回（UDP 无半关闭）。opts 可选限速与统计。
func ProxyDatagramBidirectional(a, b DatagramConn, name string, idle time.Duration, opts ...ProxyOptions) {
	var o ProxyOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	tunnelActive.Add(1)
	defer tunnelActive.Add(-1)

	parent := o.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			cancel()
			abortConnection(a)
			abortConnection(b)
		})
	}
	// 请求取消即中断两端；AfterFunc 只在真取消时起协程，不必为每条隧道常驻一个观察者。
	stopCancelWatch := context.AfterFunc(ctx, closeBoth)
	defer stopCancelWatch()

	// 活动信号：任一方读到数据都重置空闲定时器；静默超时就整体关停（UDP 无 EOF 靠它兜底）。
	var activity chan struct{}
	var wg sync.WaitGroup
	if idle > 0 {
		activity = make(chan struct{}, 4)
		wg.Add(1)
		routine.Go(name+".idle", func() {
			defer wg.Done()
			timer := time.NewTimer(idle)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
					closeBoth()
					return
				case <-activity:
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(idle)
				}
			}
		})
	}

	up := &datagramPipe{
		name: name + ".up", src: a, dst: b,
		limiter: o.UpLimit, readCnt: o.Stats.UpReadRef(), writeCnt: o.Stats.UpWriteRef(),
		clientCnt: o.ClientStats.UpWriteRef(), globalCnt: &tunnelTraffic,
		activity: activity, onErr: closeBoth,
	}
	down := &datagramPipe{
		name: name + ".down", src: b, dst: a,
		limiter: o.DownLimit, readCnt: o.Stats.DownReadRef(), writeCnt: o.Stats.DownWriteRef(),
		clientCnt: o.ClientStats.DownWriteRef(), globalCnt: &tunnelTraffic,
		activity: activity, onErr: closeBoth,
	}
	wg.Add(2)
	routine.Go(up.name, func() { defer wg.Done(); up.run(ctx) })
	routine.Go(down.name, func() { defer wg.Done(); down.run(ctx) })
	wg.Wait()
	closeBoth()
}

// datagramPipe 是一条单向数据报管线：从 src 读一个数据报，限速后原样写到 dst，按数据报
// 字节数累计统计。读到源 EOF/出错即触发整体关闭；写失败同样关闭（closeBoth 一并收）。
type datagramPipe struct {
	name      string
	src       DatagramConn
	dst       DatagramConn
	limiter   Limiter
	readCnt   *atomic.Int64
	writeCnt  *atomic.Int64
	clientCnt *atomic.Int64
	globalCnt *atomic.Int64
	activity  chan struct{}
	onErr     func()
}

// run 跑到源 EOF（干净结束）或出错为止，两者都走 onErr 整体关闭。
func (p *datagramPipe) run(ctx context.Context) {
	defer p.onErr()
	for {
		packet, rerr := p.src.ReadPacket()
		if len(packet) > 0 || rerr == nil {
			select {
			case p.activity <- struct{}{}:
			default:
			}
			if p.limiter != nil {
				if err := p.limiter.Wait(ctx, len(packet)); err != nil {
					p.onErr()
					return
				}
			}
			werr := p.dst.WritePacket(packet)
			if p.readCnt != nil {
				p.readCnt.Add(int64(len(packet)))
			}
			if werr == nil {
				if p.writeCnt != nil {
					p.writeCnt.Add(int64(len(packet)))
				}
				if p.clientCnt != nil {
					p.clientCnt.Add(int64(len(packet)))
				}
				p.globalCnt.Add(int64(len(packet)))
			} else {
				p.onErr()
				return
			}
		}
		if rerr != nil {
			p.onErr()
			return
		}
	}
}
