package mux

import (
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/routine"
)

// Handler 由装配方实现：为一条新子连接建立通往目标的通道并完成双向搬运，
// 返回时该子连接结束（Conn.Close 随之发出 End 帧）。target 已通过基本校验
// （主机非空、端口合法、网络为 TCP）。
type Handler interface {
	Open(target Target, conn *Conn)
}

// Server 在一条已认证的主连接上服务 mux 协议：读帧、分发子连接、回收。
// 服务端永不主动发 New 帧（v2ray 语义：子连接只由客户端发起）。
type Server struct {
	handler Handler
}

// NewServer 创建 mux 服务端。
func NewServer(handler Handler) *Server {
	return &Server{handler: handler}
}

// Serve 读帧并驱动子连接，阻塞到主连接结束；返回时全部子连接已收尾。
// 对端正常关闭（EOF）返回 nil，协议错误返回错误供调用方记录。
func (s *Server) Serve(conn net.Conn) error {
	w := NewWriter(conn)
	mgr := NewSessionManager()
	defer mgr.closeAll()

	remote := conn.RemoteAddr().String()
	logging.Debug("mux: session started", "remote", remote)
	for {
		f, err := readFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("mux: read frame: %w", err)
		}
		switch f.Status {
		case StatusNew:
			s.handleNew(mgr, w, f)
		case StatusKeep:
			s.handleKeep(mgr, w, f)
		case StatusEnd:
			s.handleEnd(mgr, f)
		case StatusKeepAlive:
			// 保活帧：附带数据一律丢弃（协议规定），不做任何会话动作。
		default:
			return fmt.Errorf("mux: unknown frame status %d", f.Status)
		}
	}
}

// handleNew 开子连接：校验目标后登记会话，由 Handler 协程建连与搬运。
// New 帧附带的首数据随登记一起进入输入缓冲，Handler 开隧道的耗时不会丢数据。
// 网络类型为 TCP 或 UDP；UDP 子连接按数据报语义搬运。
func (s *Server) handleNew(mgr *SessionManager, w *Writer, f *Frame) {
	network := "tcp"
	if f.Network == NetworkUDP {
		network = "udp"
	} else if f.Network != NetworkTCP {
		logging.Warn("mux: rejecting sub-connection with bad network",
			"session", f.SessionID, "network", f.Network)
		_ = w.writeEnd(f.SessionID, true)
		return
	}
	if f.Target.Port <= 0 || f.Target.Port >= 65536 || f.Target.Host == "" {
		logging.Warn("mux: rejecting sub-connection with bad target",
			"session", f.SessionID, "target", f.Target.String())
		_ = w.writeEnd(f.SessionID, true)
		return
	}
	c, ok := mgr.add(f.SessionID, network, w)
	if !ok {
		logging.Warn("mux: rejecting sub-connection: id reuse or too many sessions",
			"session", f.SessionID)
		_ = w.writeEnd(f.SessionID, true)
		return
	}
	// New 帧附带的首数据（v2ray 客户端把应用的第一个报文放在这里）随登记一起
	// 进输入缓冲：Handler 开隧道的耗时不会丢数据，也不需要它自己特殊处理首帧。
	if f.HasData() {
		if err := c.feedInput(f.Data); err != nil {
			logging.Debug("mux: dropping first payload for finished sub-connection",
				"session", f.SessionID)
		}
	}
	target := f.Target
	target.Network = network
	routine.Go("mux.session", func() {
		defer c.Close()
		s.handler.Open(target, c)
	})
}

// handleKeep 把 Keep 帧的数据投递到子连接输入；会话已结束（本地收尾或对端
// 先发过 End）时回一个 End 让对端也收尾，数据丢弃。
func (s *Server) handleKeep(mgr *SessionManager, w *Writer, f *Frame) {
	c, ok := mgr.get(f.SessionID)
	if !ok {
		_ = w.writeEnd(f.SessionID, false)
		return
	}
	if !f.HasData() {
		return
	}
	if err := c.feedInput(f.Data); err != nil {
		// 输入管道已关闭：Handler 已收尾，End 帧由 Conn.Close 发出过，这里只丢数据。
		logging.Debug("mux: dropping data for finished sub-connection", "session", f.SessionID)
	}
}

// handleEnd 结束子连接：先交付帧上可能附带的尾数据，再优雅关闭输入
// （缓冲可读、读空 EOF），Handler 随后自然走完搬运并发回 End。
// OptionError 置位时立即中断输入，不等尾数据。
func (s *Server) handleEnd(mgr *SessionManager, f *Frame) {
	c, ok := mgr.get(f.SessionID)
	if !ok {
		return
	}
	mgr.remove(f.SessionID)
	if f.Option&OptionError != 0 {
		c.inW.Interrupt()
		return
	}
	if f.HasData() {
		if err := c.feedInput(f.Data); err != nil {
			return
		}
	}
	c.finishInput()
}
