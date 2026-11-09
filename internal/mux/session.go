package mux

import (
	"context"
	"errors"
	"sync"

	"github.com/hhy50/vnray/internal/pipe"
)

// maxSessionDatagram 是单个数据报的上限，与长度前缀 uint16 的表达范围一致。
const maxSessionDatagram = 65535

// maxSessions 是单条主连接上的子连接上限：客户端规范上限 128，放宽到 1024 容忍
// 激进实现，同时把恶意会话数挡在资源耗尽之前。
const maxSessions = 1024

// Conn 是一条 mux 子连接的端点视图，实现 io.ReadWriteCloser：
// Read 读出对端经 Keep 帧发来的数据（读到带内结束返回 io.EOF）；
// Write 写出的数据以 Keep 帧回传；Close 发送 End 帧并结束该子连接（幂等）。
// UDP 子连接（network=="udp"）语义为数据报：ReadPacket/WritePacket 各对应一个
// 数据报，每个 Keep 帧的 Data 恰好是一报（WritePacket 不分包）。
type Conn struct {
	id      uint16
	network string
	target  Target
	in      *pipe.Reader
	inW     *pipe.Writer
	out     *Writer
	mgr     *SessionManager
	once    sync.Once
	ctx     context.Context
	cancel  context.CancelFunc
}

// newConn 创建一条子连接视图。
func newConn(id uint16, network string, out *Writer, mgr *SessionManager) *Conn {
	r, w := pipe.New()
	ctx, cancel := context.WithCancel(mgr.ctx)
	return &Conn{id: id, network: network, in: r, inW: w, out: out, mgr: mgr, ctx: ctx, cancel: cancel}
}

// Context 在子连接终止或主连接断开时取消，正常请求 EOF 不取消。
func (c *Conn) Context() context.Context { return c.ctx }

// Read 读出对端发来的数据；子连接结束后返回 io.EOF（或 io.ErrClosedPipe，
// 视收尾是优雅关闭还是中断）。
func (c *Conn) Read(p []byte) (int, error) { return c.in.Read(p) }

// Write 以 Keep 数据帧回传数据，超过单帧上限自动分帧。
func (c *Conn) Write(p []byte) (int, error) { return c.out.writeData(c.id, p) }

// ReadPacket 读出一个完整数据报，忽略逐包目标。
func (c *Conn) ReadPacket() ([]byte, error) {
	_, payload, err := c.ReadPacketTo()
	return payload, err
}

// ReadPacketTo 读出一个完整数据报及其目标；无显式目标时使用 New 帧目标。
func (c *Conn) ReadPacketTo() (Target, []byte, error) {
	if c.network != "udp" {
		return Target{}, nil, errors.New("mux: ReadPacket on tcp sub-connection")
	}
	f, err := readFrame(c.in)
	if err != nil {
		return Target{}, nil, err
	}
	f.Target.Network = "udp"
	return f.Target, f.Data, nil
}

// WritePacket 以单个 Keep 帧写出一个完整数据报（UDP 子连接用，不分包）。
func (c *Conn) WritePacket(datagram []byte) error { return c.out.writePacket(c.id, datagram) }

// WritePacketFrom 写出一个携带来源地址的 UDP 响应。
func (c *Conn) WritePacketFrom(source Target, datagram []byte) error {
	return c.out.writePacketFrom(c.id, source, datagram)
}

// Close 结束该子连接：中断输入、向对端发 End 帧、从管理器摘除。幂等；
// 重复调用（Handler 收尾与连接级关闭竞争）只发一次 End。
func (c *Conn) Close() error { return c.closeWith(false) }

// CloseWithError 以异常原因结束子连接：End 帧带 OptionError，对端据此立刻
// 收尾而不是等 graceful 关闭。用于建连失败等"从未成立"的场景。
func (c *Conn) CloseWithError() error { return c.closeWith(true) }

// closeWith 发 End 帧并摘除；once 保证重复调用只发一次。
func (c *Conn) closeWith(errFlag bool) error {
	c.once.Do(func() {
		c.cancel()
		c.inW.Interrupt()
		_ = c.out.writeEnd(c.id, errFlag)
		c.mgr.removeConn(c)
	})
	return nil
}

// finishInput 优雅关闭输入（缓冲数据仍可读，读空后 EOF）：对端发来 End 时调用，
// 让 Handler 先把尾数据搬完再收尾。
func (c *Conn) finishInput() {
	_ = c.inW.Close()
}

// feedInput 把一帧数据投递进输入缓冲；管道已关闭（Handler 已收尾）时返回错误。
// UDP 在输入管道中保留逐包目标和数据报边界。
func (c *Conn) feedInput(data []byte) error {
	if c.network != "udp" {
		_, err := c.inW.Write(data)
		return err
	}
	return c.feedPacket(c.target, data)
}

func (c *Conn) feedPacket(target Target, data []byte) error {
	if target.Host == "" {
		target = c.target
	}
	return writeFrameTarget(c.inW, c.id, StatusKeep, OptionData, data, target)
}

// SessionManager 登记一条主连接上的全部子连接，并发安全。
type SessionManager struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	sessions map[uint16]*Conn
	active   map[*Conn]struct{}
}

// NewSessionManager 创建空的子连接管理器。
func NewSessionManager(ctx context.Context) *SessionManager {
	ctx, cancel := context.WithCancel(ctx)
	return &SessionManager{ctx: ctx, cancel: cancel, sessions: make(map[uint16]*Conn), active: make(map[*Conn]struct{})}
}

// add 登记一条新子连接；ID 已占用或数量超限时返回 false（调用方应回 End 帧）。
func (m *SessionManager) add(id uint16, network string, out *Writer) (*Conn, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return nil, false
	}
	if _, exists := m.sessions[id]; exists {
		return nil, false
	}
	if len(m.active) >= maxSessions {
		return nil, false
	}
	c := newConn(id, network, out, m)
	m.sessions[id] = c
	m.active[c] = struct{}{}
	return c, true
}

// get 按 ID 取子连接。
func (m *SessionManager) get(id uint16) (*Conn, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.sessions[id]
	return c, ok
}

// remove 摘除子连接；重复摘除是空操作。
func (m *SessionManager) remove(id uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

func (m *SessionManager) removeConn(conn *Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[conn.id] == conn {
		delete(m.sessions, conn.id)
	}
	delete(m.active, conn)
}

// closeAll 结束全部子连接（主连接断开时调用）：先摘除再逐个 Close，
// 避免 Handler 的 End 帧写竞争把管理器锁带进写路径。
func (m *SessionManager) closeAll() {
	m.cancel()
	m.mu.Lock()
	all := make([]*Conn, 0, len(m.active))
	for c := range m.active {
		all = append(all, c)
	}
	m.sessions = make(map[uint16]*Conn)
	m.active = make(map[*Conn]struct{})
	m.mu.Unlock()
	for _, c := range all {
		_ = c.Close()
	}
}
