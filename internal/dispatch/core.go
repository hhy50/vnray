package dispatch

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/hhy50/vnray/internal/pipe"
)

// Destination 是代理请求的目标地址。
type Destination struct {
	Host string
	Port int
	// Network 是目标的数据类型：空或 "tcp"（默认）走字节流出站，取 "udp" 走数据报
	// 出站。只影响出站拨号与搬运语义，不参与路由规则匹配（规则只读 Host/Port）。
	Network string
}

// netName 归一化网络类型：空视为 tcp。
func (d Destination) netName() string {
	if d.Network == "" {
		return "tcp"
	}
	return d.Network
}

func (d Destination) String() string {
	return net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
}

// ParseDestination 解析并校验标准 host:port 地址，包括带方括号的 IPv6。
func ParseDestination(address string) (Destination, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return Destination{}, fmt.Errorf("parse destination: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return Destination{}, fmt.Errorf("parse destination port: %w", err)
	}
	dest := Destination{Host: host, Port: port}
	if !dest.IsValid() {
		return Destination{}, fmt.Errorf("invalid destination %q", address)
	}
	return dest, nil
}

// IsValid 判断目标是否合法。
func (d Destination) IsValid() bool {
	return d.Host != "" && d.Port > 0 && d.Port < 65536
}

// Link 是一对管道端口，代表一条双向数据通道的一个端点视图。
type Link struct {
	// Reader 读对面写进来的数据；Writer 写出的数据由对面读走。
	Reader *pipe.Reader
	Writer *pipe.Writer
}

// Dispatcher 是核心路由分发器：给定目标地址，返回一条通往某个出站的管道。
type Dispatcher interface {
	Dispatch(ctx context.Context, dest Destination) (*Link, error)
}

// OutboundHandler 是出站处理器：dispatcher 选中后，把 Link 的另一端交给它
// 连接真正的目标。
type OutboundHandler interface {
	Tag() string
	Process(ctx context.Context, link *Link, dest Destination) error
}

// OutboundError 是出站侧返回给对端的结构化错误原因，
// 协议层会把它作为 Error 帧的载荷传给对端。
type OutboundError struct {
	Reason string
}

func (e *OutboundError) Error() string { return e.Reason }

// Dialer 抽象出站拨号，便于上层注入受控拨号器。
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}
