package dispatch

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/hhy50/vnray/internal/routine"
)

// DirectOutbound 直连出站：拨目标连接，在管道和目标之间双向搬运。
type DirectOutbound struct {
	// tag 为空时 Tag 回退 "direct"，保持零值可用。
	tag string
	// dialer 为 nil 时回退标准 net.Dialer。
	dialer Dialer
	// hosts 为 nil 时不启用自定义域名表；非 nil 时拨号前先查表，命中的域名改用
	// 表里的 IP 直连，未命中的域名照常走 DNS。
	hosts *Hosts
	// ipMaps 为 nil 时不启用 IP 段映射；非 nil 时目标为 IPv4 且命中模式时先重写 IP。
	ipMaps *IPMaps
}

// NewDirectOutbound 创建带自定义 tag 的直连出站，使用默认拨号器。
func NewDirectOutbound(tag string) *DirectOutbound { return &DirectOutbound{tag: tag} }

// NewDirectOutboundWithDialer 创建带自定义 tag 与拨号器的直连出站。
func NewDirectOutboundWithDialer(tag string, dialer Dialer) *DirectOutbound {
	return &DirectOutbound{tag: tag, dialer: dialer}
}

// SetHosts 设置拨号时查询的自定义域名表；nil 表示不启用。
func (d *DirectOutbound) SetHosts(h *Hosts) { d.hosts = h }

// SetIPMaps 设置拨号时应用的 IP 段映射；nil 表示不启用。
func (d *DirectOutbound) SetIPMaps(m *IPMaps) { d.ipMaps = m }

func (d *DirectOutbound) dial(ctx context.Context, address string) (net.Conn, error) {
	dialer := d.dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	if d.hosts != nil || d.ipMaps != nil {
		address = d.resolveAddress(address)
	}
	return dialer.DialContext(ctx, "tcp", address)
}

// resolveAddress 拨号前改写目标：host 命中自定义域名表换成表里的 IP，否则目标是
// IPv4 且命中 IP 段映射时按模式重写；都不命中原样返回。
func (d *DirectOutbound) resolveAddress(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	replaced := ""
	if d.ipMaps != nil {
		if ip, ok := d.ipMaps.Rewrite(host); ok {
			replaced = ip
		}
	}
	if replaced == "" && d.hosts != nil {
		if ip, ok := d.hosts.LookupHost(host); ok {
			replaced = ip
		}
	}
	if replaced == "" {
		return address
	}
	return net.JoinHostPort(replaced, port)
}

func (d *DirectOutbound) Tag() string {
	if d.tag != "" {
		return d.tag
	}
	return "direct"
}

// Process 起两个泵：上行（管道→目标）和下行（目标→管道）。
// 方向分离：上行读到入站 FIN 时只对目标 TCP 写方向 CloseWrite，让目标收到请求 EOF 后
// 开始返回响应；下行把响应送完、管道优雅关闭后才整体收尾。任一方向出错才另一条也断。
func (d *DirectOutbound) Process(ctx context.Context, link *Link, dest Destination) error {
	conn, err := d.dial(ctx, dest.String())
	if err != nil {
		return fmt.Errorf("direct: dial %s: %w", dest, err)
	}
	tcpConn, isTCP := conn.(*net.TCPConn)

	// 会话取消时同时解除网络读写和管道读取。
	done := make(chan struct{})
	defer close(done)
	routine.Go("core.direct.watchCancel", func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
			link.Reader.Interrupt()
		case <-done:
		}
	})

	var once sync.Once
	teardown := func() {
		once.Do(func() {
			_ = conn.Close()
			link.Reader.Interrupt()
			link.Writer.Interrupt()
		})
	}

	// 上行泵：管道 → 目标；读到入站 FIN 时对目标 CloseWrite（发 TCP FIN），
	// 让源站收到请求 EOF 后开始返回响应，下行方向保留。
	// panic 在本地转换为 error 送入 upErr：若交给 routine 统一 recover，
	// 结果将永远不会送达，Process 会在 <-upErr 处永久阻塞。
	upErr := make(chan error, 1)
	routine.Go("core.direct.upstream", func() {
		defer func() {
			if r := recover(); r != nil {
				upErr <- fmt.Errorf("core: upstream pump panicked: %v", r)
			}
		}()
		_, err := io.Copy(conn, link.Reader)
		if isTCP {
			// 上行 FIN：源站据此认为请求发完，开始返回响应
			_ = tcpConn.CloseWrite()
		}
		upErr <- err
	})

	// 下行泵：目标 → 管道；目标 EOF（Connection: close 响应读完）后优雅关闭写端，
	// 让入站方向拿到 EOF；上行泵的收尾由它自己的协程完成。
	_, downErr := io.Copy(link.Writer, conn)
	_ = link.Writer.Close()
	_ = conn.Close()
	link.Reader.Interrupt()

	if uerr := <-upErr; uerr != nil && downErr == nil {
		downErr = uerr
	}
	teardown()
	return downErr
}

// RejectOutbound 拒绝出站：通过结构化错误把拒绝原因回传给对端。
type RejectOutbound struct {
	// tag 为空时 Tag 回退 "reject"。
	tag string
	// Reason 作为拒绝消息返回给对端。
	Reason string
}

// NewRejectOutbound 创建带自定义 tag 与理由的拒绝出站。
func NewRejectOutbound(tag, reason string) *RejectOutbound {
	return &RejectOutbound{tag: tag, Reason: reason}
}

func (r *RejectOutbound) Tag() string {
	if r.tag != "" {
		return r.tag
	}
	return "reject"
}

func (r *RejectOutbound) Process(_ context.Context, link *Link, dest Destination) error {
	reason := r.Reason
	if reason == "" {
		reason = "rejected by outbound " + r.Tag()
	}
	link.Writer.CloseWithError(&OutboundError{Reason: reason})
	link.Reader.Interrupt()
	return nil
}
