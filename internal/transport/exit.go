package transport

import (
	"net"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/protocol"
)

// LocalClientName 是服务端本机出口（server_proxy）的保留客户端名。
// 它只有两段，不满足客户端名 namespace:group:name 的三段校验，
// 因此任何真实客户端都无法占用它。
const LocalClientName = "server:self"

// Exit 是一个出口端点：给定目标开一条隧道，返回可双向搬运的裸字节流。
// 远程客户端会话（*Session）与服务端本机出口都实现它。
type Exit interface {
	OpenTunnel(dest dispatch.Destination, req protocol.TunnelRequest) (net.Conn, error)
}

// ExitResolver 按绑定客户端名解析出口；未命中返回 false。
type ExitResolver interface {
	Resolve(name string) (Exit, bool)
}

// Resolve 实现 ExitResolver：按名返回在线客户端会话作为出口；未在线或会话不携带
// 客户端视图时返回 false。
func (m *SessionManager) Resolve(name string) (Exit, bool) {
	info, ok := m.SessionByName(name)
	if !ok {
		return nil, false
	}
	client := clientOf(info)
	if client == nil {
		return nil, false
	}
	return client, true
}
