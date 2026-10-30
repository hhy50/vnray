package transport

import (
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/hhy50/vnray/internal/concurrency"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/tcp"
)

// SessionEvents 是数据面对外上报的会话事件集合，由服务端侧的会话登记簿实现
// （见 internal/admin.Sessions）。事件可能在多条会话的协程上并发触发，
// 实现方需自行保证并发安全；为 nil 时表示不上报。
type SessionEvents interface {
	// SessionConnected 在会话建立、开始受理请求前调用。
	SessionConnected(*Session)
	// SessionDisconnected 在会话关闭、不再受理请求后调用。
	SessionDisconnected(*Session)
}

// UserMeta 是用户会话的元数据：user_key 供限速与审计标识用户，Info 是用户随认证上报的自信息。
type UserMeta struct {
	Key string
	// Name 是用户的管理台显示名（非空），会话建立日志里带上它便于对认人。
	Name string
	// Rule 是该用户使用的出站规则名（空按服务端内置 default 处理）。
	Rule string
	Info protocol.UserInfo
}

// Session 是服务端持有的请求发起端，一个请求对应一条逻辑流。
type Session struct {
	session tcp.Session
	remote  string
	info    protocol.ClientInfo

	// gate 是对设备（本客户端）同时开展的隧道流并发闸；max=0 表示不限。
	gate *concurrency.Gate

	// pingInFlight 标记该会话是否有一次心跳探测在途（Timeout > Interval 时防重叠）。
	pingInFlight atomic.Bool

	// 心跳结果由调度方写入、由管理面通过访问器读取，故用原子量：
	// lastHeartbeat 是应答时刻的 unix 毫秒，lastRTT 是往返时延的纳秒数。
	lastHeartbeat atomic.Int64
	lastRTT       atomic.Int64
}

// controlStreamTimeout 是一次性控制流（自定义 host 下发）的写等上限。
const controlStreamTimeout = 5 * time.Second

// NewSession 包装一条握手通过的会话；remote 为对端地址文本，info 为客户端上报的自信息。
func NewSession(session tcp.Session, remote string, info protocol.ClientInfo) *Session {
	return &Session{
		session: session,
		remote:  remote,
		info:    info,
		gate:    concurrency.NewGate(),
	}
}

// SetClientConcurrency 设置路由到本客户端的隧道流并发上限；非正数表示不限。
// 运行期可热改，对在等待的排队者即时生效。
func (s *Session) SetClientConcurrency(max int) { s.gate.SetMax(max) }

// ClientConcurrency 返回当前并发上限。
func (s *Session) ClientConcurrency() int { return s.gate.Max() }

// CloseSignal 返回会话关闭信号，供并发闸等待时中断（会话没了无需继续排队）。
func (s *Session) CloseSignal() <-chan struct{} { return s.session.CloseChan() }

// Close 关闭底层会话。
func (s *Session) Close() error { return s.session.Close() }

// RemoteAddr 返回对端地址文本。
func (s *Session) RemoteAddr() string { return s.remote }

// Info 返回客户端在认证时上报的配置与环境摘要。
func (s *Session) Info() protocol.ClientInfo { return s.info }

// LastHeartbeat 返回最近一次心跳应答的本地时刻；从未测得时为 time.Time 零值。
func (s *Session) LastHeartbeat() time.Time {
	ms := s.lastHeartbeat.Load()
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// LastRTT 返回最近一次心跳测得的往返时延；尚未测得时为 0。
func (s *Session) LastRTT() time.Duration { return time.Duration(s.lastRTT.Load()) }

// BeginPing 标记一次心跳探测开始；已有在途探测时返回 false（Timeout > Interval 时防重叠）。
func (s *Session) BeginPing() bool { return s.pingInFlight.CompareAndSwap(false, true) }

// EndPing 清除在途标记，须与 BeginPing 配对。
func (s *Session) EndPing() { s.pingInFlight.Store(false) }

// RecordHeartbeat 记录一次成功的心跳：应答时刻为此刻，往返时延为 rtt。
func (s *Session) RecordHeartbeat(rtt time.Duration) {
	s.lastRTT.Store(int64(rtt))
	s.lastHeartbeat.Store(time.Now().UnixMilli())
}

// IsClosed 报告会话是否已关闭（心跳调度用它跳过已终结的会话）。
func (s *Session) IsClosed() bool { return s.session.IsClosed() }

// SendPushConfig 向该客户端推送全量配置快照：开一条一次性控制流发下发帧后关闭。
func (s *Session) SendPushConfig(cfg protocol.PushConfig) error {
	stream, err := s.session.OpenStream()
	if err != nil {
		return fmt.Errorf("open push config stream: %w", err)
	}
	defer func() { _ = stream.Close() }()
	if err := stream.SetDeadline(time.Now().Add(controlStreamTimeout)); err != nil {
		return fmt.Errorf("set push config deadline: %w", err)
	}
	frame, err := protocol.NewPushConfigFrame(cfg)
	if err != nil {
		return err
	}
	if err := protocol.NewEncoder(stream).WriteFrame(frame); err != nil {
		return fmt.Errorf("write push config frame: %w", err)
	}
	return nil
}

// Wait 阻塞直到客户端会话结束。
func (s *Session) Wait() {
	<-s.session.CloseChan()
}

// PingRTT 做一次心跳探测：开一条一次性控制流发 Ping、等 Pong，返回往返时延。
//
// 每次心跳单开一条流而不是复用长连接：一次性的"发—等—关"没有跨请求的序号配对
// 问题，等待上限也能直接表达成流上的 deadline，而建流成本仅一个帧头。
func (s *Session) PingRTT(timeout time.Duration) (time.Duration, error) {
	stream, err := s.session.OpenStream()
	if err != nil {
		return 0, fmt.Errorf("open control stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

	if err := stream.SetDeadline(time.Now().Add(timeout)); err != nil {
		return 0, fmt.Errorf("set control deadline: %w", err)
	}

	sent := time.Now()
	ping := protocol.NewHeartbeatFrame(protocol.Ping, sent.UnixMilli())
	if err := protocol.NewEncoder(stream).WriteFrame(ping); err != nil {
		return 0, fmt.Errorf("write ping: %w", err)
	}

	pong, err := protocol.NewDecoder(stream).ReadFrame()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return 0, errors.New("control stream closed before pong")
		}
		return 0, fmt.Errorf("read pong: %w", err)
	}
	if pong.Type != protocol.Pong {
		_ = pong.CloseBody()
		return 0, fmt.Errorf("unexpected frame type %d on control stream", pong.Type)
	}
	// 回显校验：Pong 必须回带同一个时间戳，否则控制流已经错位，这次测量不可信。
	echoed, err := protocol.ReadHeartbeatTimestamp(pong)
	if err != nil {
		return 0, fmt.Errorf("bad pong frame: %w", err)
	}
	if echoed != sent.UnixMilli() {
		return 0, fmt.Errorf("pong echoed timestamp %d, want %d", echoed, sent.UnixMilli())
	}

	// 用本端单调时钟计时：只依赖一个时钟，不受两端时钟差与系统时间跳变影响。
	return time.Since(sent), nil
}
