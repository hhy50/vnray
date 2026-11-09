package tcp

import (
	"context"
	"net"
	"time"

	"github.com/hhy50/vnray/internal/logging"
)

// Session 是一条已建立的多路复用会话；认证、心跳与转发都只依赖本抽象。
// 具体后端（当前为 yamux）由 Muxer 在建会话时注入，本包不直接依赖任何多路复用库。
type Session interface {
	RemoteAddr() net.Addr
	OpenStream() (Stream, error)
	AcceptStream() (Stream, error)
	CloseChan() <-chan struct{}
	IsClosed() bool
	Close() error
	// SetDeadline 是会话级设置：只影响 AcceptStream 的等待，不作用于底层连接。
	SetDeadline(t time.Time) error
}

type connectionSession struct {
	Session
	conn   net.Conn
	cancel context.CancelFunc
}

func (s *connectionSession) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	return s.Session.Close()
}

// OpenStreamContext 使用后端的可取消开流能力。
func OpenStreamContext(ctx context.Context, session Session) (Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opener, ok := session.(interface {
		OpenStreamContext(context.Context) (Stream, error)
	}); ok {
		return opener.OpenStreamContext(ctx)
	}
	return session.OpenStream()
}

// OpenStreamContext 保留连接包装层下的可取消开流能力。
func (s *connectionSession) OpenStreamContext(ctx context.Context) (Stream, error) {
	return OpenStreamContext(ctx, s.Session)
}

// Connection returns the connection used by a server-side session, or nil.
func Connection(session Session) net.Conn {
	if s, ok := session.(*connectionSession); ok {
		return s.conn
	}
	return nil
}

// Stream 是会话内的一条逻辑流，帧与裸字节都经它读写。Close 是半关闭：向对端发
// FIN 结束本端写方向，读方向继续可用；整条流的回收由对端 FIN 或会话关闭完成
// （yamux 后端语义，方向分离的转发依赖这一点）。
type Stream interface {
	net.Conn
	// ID 返回流标识，用于日志关联。
	ID() uint32
}

// Kind 是会话在握手首帧里确定的种类。
type Kind uint8

const (
	// KindClient 是通过安全握手并完成首次配置上报的内网客户端。
	KindClient Kind = iota + 1
	// KindUser 是转发用户：握手发 UserAuth 帧出示 user_key。
	KindUser
)

// SessionInfo 是一条已建立会话的元数据：握手通过后由 Server 登记，会话终结时移除。
type SessionInfo struct {
	// Context 在会话终止或所属服务取消时结束。
	Context context.Context
	// Session 是底层会话。
	Session Session
	// Remote 是对端地址文本。
	Remote string
	// Kind 是握手判定的会话种类。
	Kind Kind
	// Name 是这条会话关联的名字：客户端会话是自己上报的标识（按名选路的依据），
	// 用户会话是它绑定的客户端名。
	Name string
	// Since 是握手通过的时刻。
	Since time.Time
	// Log 是这条会话的日志器：级别取用户/客户端的运维配置（默认 info），由握手方装配。
	// 为 nil 时调用方退回全局日志。
	Log *logging.Scoped
	// Meta 是握手方附加的会话数据，由上层解释；tcp 只负责保管。
	Meta any
}
