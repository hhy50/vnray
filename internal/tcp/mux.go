package tcp

import (
	"net"
)

// Muxer 是多路复用后端：把一条已建立的连接包装成 Session。线格式随后端而异，
// 一条链路的两端必须使用同一后端，由部署方保证对称。
type Muxer interface {
	// Name 是后端标识，用于日志与诊断。
	Name() string
	// Server 包装服务端侧连接（监听方 Accept 得到）。
	Server(conn net.Conn, cfg *SessionConfig) (Session, error)
	// Client 包装客户端侧连接（主动拨号得到）。
	Client(conn net.Conn, cfg *SessionConfig) (Session, error)
}

// YamuxMuxer 返回 hashicorp/yamux 后端。
func YamuxMuxer() Muxer { return yamuxBackend{} }

// DefaultMuxer 返回默认后端（yamux）。Muxer 为 nil 的调用方（客户端拨号、
// 用户接入）都经此取后端，换后端只需改这里。
func DefaultMuxer() Muxer { return YamuxMuxer() }
