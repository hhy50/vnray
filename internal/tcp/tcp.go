package tcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/routine"
)

// Server 是事件驱动的多路复用监听器（功能级 extension.Feature）：打开监听、为每条连接
// 建立会话并派发成对事件；会话登记由 SessionManager 负责，事件接收方在构造时给定。
type Server struct {
	addr     string
	cfg      atomic.Pointer[SessionConfig]
	muxer    Muxer
	listener EventListener

	ln     atomic.Pointer[net.Listener]
	closed atomic.Bool
}

// Option 覆盖监听器的可选参数。
type Option func(*serverOptions)

// serverOptions 集中可选参数，避免构造签名随可选项增加而变长。
type serverOptions struct {
	sessionConfig *SessionConfig
	muxer         Muxer
}

// WithSessionConfig 覆盖多路复用层参数（keepalive 周期/超时、单流窗口）；nil 用默认值。
func WithSessionConfig(cfg *SessionConfig) Option {
	return func(o *serverOptions) { o.sessionConfig = cfg }
}

// WithMuxer 选择多路复用后端（当前内置 yamux）；nil 用 DefaultMuxer。线格式随后端
// 而异，一条链路的两端必须选同一后端。
func WithMuxer(m Muxer) Option {
	return func(o *serverOptions) { o.muxer = m }
}

// NewServer 创建监听器；listener 接收会话与流事件，opts 覆盖可选参数。
func NewServer(addr string, listener EventListener, opts ...Option) *Server {
	var o serverOptions
	for _, opt := range opts {
		opt(&o)
	}
	cfg := o.sessionConfig
	if cfg == nil {
		cfg = DefaultSessionConfig()
	}
	muxer := o.muxer
	if muxer == nil {
		muxer = DefaultMuxer()
	}
	s := &Server{addr: addr, muxer: muxer, listener: listener}
	s.cfg.Store(cfg)
	return s
}

// SetSessionConfig 热更新多路复用参数：只对之后建立的新连接生效，既存会话沿用旧参数。
// nil 视为回到默认值。
func (s *Server) SetSessionConfig(cfg *SessionConfig) {
	if cfg == nil {
		cfg = DefaultSessionConfig()
	}
	s.cfg.Store(cfg)
}

// 默认会话参数见 DefaultSessionConfig：keepalive 间隔 10s、等待上限 30s，
// 单流窗口 256KiB、半关闭强拔禁用。keepalive 只负责存活检测；应用层 Ping/Pong
// 负责延迟可观测，两者参数互不影响。

// Start 打开监听并驱动事件循环，然后立即返回；重复 Start 返回错误。
func (s *Server) Start(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("tcp: listen %s: %w", s.addr, err)
	}
	if !s.ln.CompareAndSwap(nil, &ln) {
		_ = ln.Close() // 并发 Start：败者关闭自己刚开的监听
		return fmt.Errorf("tcp: already started")
	}

	routine.Go("tcp.eventloop", func() {
		if err := s.serve(); err != nil {
			logging.Errorf("tcp event loop stopped: %v", err)
		}
	})
	return nil
}

// Addr 返回实际监听地址；未 Start 时返回配置地址（传 ":0" 用于取内核分配端口）。
func (s *Server) Addr() string {
	if lnp := s.ln.Load(); lnp != nil {
		return (*lnp).Addr().String()
	}
	return s.addr
}

// Name 返回功能名，用于启停日志与诊断。
func (s *Server) Name() string { return "tcp" }

// Stop 关闭监听；幂等。ctx 未被使用（关闭监听不阻塞），参数只为满足 extension.Feature 契约。
func (s *Server) Stop(context.Context) error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	if lnp := s.ln.Load(); lnp != nil {
		return (*lnp).Close()
	}
	return nil
}

// EventListener 接收接入事件：先握手产出会话元数据，登记由 SessionManager 在会话
// 事件里完成。成对触发（先 Open、其返回后再 Close）；On*Open 可阻塞整个生命周期。
type EventListener interface {
	// Handshake 完成握手并返回会话元数据；返回 error 则关闭会话、不登记、不派发任何事件。
	Handshake(session Session) (SessionInfo, error)

	// OnSessionOpen 在会话登记后、开始受理流之前调用；须快速返回，返回 error 视为拒绝该会话。
	OnSessionOpen(info *SessionInfo) error
	// OnSessionClose 在 accept 循环退出（会话终结）后调用，此时会话仍登记在表里。
	OnSessionClose(info *SessionInfo)

	// OnStreamOpen 在收到所属会话的一条入站流后调用；可阻塞该流整个生命周期。
	OnStreamOpen(info *SessionInfo, stream Stream) error
	// OnStreamClose 在 OnStreamOpen 返回后、流关闭前调用，用于收尾。
	OnStreamClose(info *SessionInfo, stream Stream)
}

// serve 是 Accept 循环，为每条新连接建立会话并派发成对事件；监听关闭引起的错误视为正常退出。
func (s *Server) serve() error {
	lnp := s.ln.Load()
	if lnp == nil {
		return fmt.Errorf("tcp: not started")
	}
	ln := *lnp

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		routine.Go("tcp.session", func() { s.serveSession(conn) })
	}
}

// serveSession 把一条已接受的连接转成多路复用会话并驱动其一生；
// 只有握手通过并登记（OnSessionOpen）之后才派发流事件，握手失败只关会话、不发任何事件。
func (s *Server) serveSession(conn net.Conn) {
	listener := s.listener
	r := conn.RemoteAddr()
	session, err := s.muxer.Server(conn, s.cfg.Load())
	if err != nil {
		logging.Errorf("tcp: create %s session remote=%s, error: %v", s.muxer.Name(), r, err)
		_ = conn.Close()
		return
	}

	info, err := listener.Handshake(session)
	if err != nil {
		logging.Warnf("tcp: handshake failed, remote=%s: %v", r, err)
		_ = session.Close()
		return
	}
	defer func() {
		// 先等在途流事件派发完再发 session.close：保证关闭事件排在所有流事件之后。
		logging.Warnf("tcp session closed, remote=%s", r)
		listener.OnSessionClose(&info) // 登记的注销由 SessionManager 在此事件里完成
		_ = session.Close()
	}()

	if err := listener.OnSessionOpen(&info); err != nil {
		logging.Errorf("tcp: on session open error: %v", err)
		return
	}
	acceptStreams(listener, &info)
}

// acceptStreams 是会话内的 Accept 循环：为每条入站流派发一对事件，会话关闭即退出。
func acceptStreams(listener EventListener, info *SessionInfo) {
	session := info.Session
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		// select 随机性：会话关闭瞬间 chAccepts 里可能仍有余流，此时 AcceptStream
		// 仍可能返回 (流, nil)，不能给已关闭的会话再派发事件。
		if session.IsClosed() {
			_ = stream.Close()
			return
		}
		routine.Go("tcp.stream", func() {
			serveStream(listener, info, stream)
		})
	}
}

// serveStream 驱动一条入站流：交给 OnStreamOpen 阻塞到流结束，再收尾关闭。
func serveStream(listener EventListener, info *SessionInfo, stream Stream) {
	// 接受与派发之间会话可能恰好关闭，同样不再派发事件。
	if info.Session.IsClosed() {
		return
	}
	defer func() {
		listener.OnStreamClose(info, stream)
		_ = stream.Close()
	}()
	if err := listener.OnStreamOpen(info, stream); err != nil {
		logging.Errorf("tcp: on stream open error: %v", err)
	}
}
