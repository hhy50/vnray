package tcp

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/hhy50/vnray/internal/logging"
)

// yamuxBackend 把 hashicorp/yamux 适配到 Muxer。
type yamuxBackend struct{}

// Name 实现 Muxer。
func (yamuxBackend) Name() string { return "yamux" }

// Server 实现 Muxer。
func (yamuxBackend) Server(conn net.Conn, cfg *SessionConfig) (Session, error) {
	s, err := yamux.Server(conn, yamuxConfig(cfg))
	if err != nil {
		return nil, err
	}
	return &yamuxSession{Session: s}, nil
}

// Client 实现 Muxer。
func (yamuxBackend) Client(conn net.Conn, cfg *SessionConfig) (Session, error) {
	s, err := yamux.Client(conn, yamuxConfig(cfg))
	if err != nil {
		return nil, err
	}
	return &yamuxSession{Session: s}, nil
}

// minYamuxStreamWindow 是 yamux 允许的最小单流窗口（其 initialStreamWindow，
// 包内未导出，这里复刻数值；VerifyConfig 会拒绝更小的值）。
const minYamuxStreamWindow = 256 * 1024

// yamuxConfig 把中性配置映射到 yamux。三处非对称映射：
//   - KeepAliveTimeout → ConnectionWriteTimeout：yamux 的 Ping 等待上限就是
//     写超时，没有独立字段；
//   - StreamCloseTimeout 恒为 0：半关闭窗口可能覆盖整个下载时长，见
//     SessionConfig 的字段注释；
//   - MaxStreamWindow 下限 256KiB：yamux 的硬性要求。
func yamuxConfig(cfg *SessionConfig) *yamux.Config {
	c := &yamux.Config{
		AcceptBacklog:          256,
		EnableKeepAlive:        true,
		KeepAliveInterval:      30 * time.Second,
		ConnectionWriteTimeout: 10 * time.Second,
		MaxStreamWindowSize:    minYamuxStreamWindow,
		StreamOpenTimeout:      75 * time.Second,
		StreamCloseTimeout:     0,
		Logger:                 yamuxLogger{},
	}
	if cfg == nil {
		return c
	}
	if cfg.KeepAliveInterval > 0 {
		c.KeepAliveInterval = cfg.KeepAliveInterval
	} else {
		c.EnableKeepAlive = false
	}
	if cfg.KeepAliveTimeout > 0 {
		c.ConnectionWriteTimeout = cfg.KeepAliveTimeout
	}
	if cfg.MaxStreamWindow >= minYamuxStreamWindow {
		c.MaxStreamWindowSize = cfg.MaxStreamWindow
	}
	if cfg.AcceptBacklog > 0 {
		c.AcceptBacklog = cfg.AcceptBacklog
	}
	c.StreamOpenTimeout = cfg.StreamOpenTimeout
	c.StreamCloseTimeout = 0
	return c
}

// yamuxLogger 把 yamux 的内部日志接进 internal/logging：yamux 的协议级提示
// （丢弃未知流数据、窗口溢出等）对本项目只是诊断信息，统一走 Debug 级别。
type yamuxLogger struct{}

// Print 实现 yamux.Logger。
func (yamuxLogger) Print(v ...any) { logging.Debugf("yamux: %s", fmt.Sprint(v...)) }

// Printf 实现 yamux.Logger。
func (yamuxLogger) Printf(format string, v ...any) { logging.Debugf("yamux: "+format, v...) }

// Println 实现 yamux.Logger。
func (yamuxLogger) Println(v ...any) { logging.Debugf("yamux: %s", fmt.Sprintln(v...)) }

// yamuxSession 把 *yamux.Session 适配成 Session。
type yamuxSession struct {
	*yamux.Session
	// acceptDeadline 是会话级 AcceptStream 等待上限（零值表示不设限）：
	// yamux 没有 Session.SetDeadline，用 AcceptStreamWithContext 等价实现。
	acceptDeadline atomic.Value // time.Time
}

// OpenStream 开一条出站逻辑流。
func (s *yamuxSession) OpenStream() (Stream, error) {
	st, err := s.Session.OpenStream()
	if err != nil {
		return nil, err
	}
	return yamuxStream{st}, nil
}

// AcceptStream 接受一条入站逻辑流：未设期限时阻塞等待，已设期限时用 context 限定。
func (s *yamuxSession) AcceptStream() (Stream, error) {
	if t, ok := s.acceptDeadline.Load().(time.Time); ok && !t.IsZero() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(t))
		defer cancel()
		st, err := s.Session.AcceptStreamWithContext(ctx)
		if err != nil {
			return nil, err
		}
		return yamuxStream{st}, nil
	}
	st, err := s.Session.AcceptStream()
	if err != nil {
		return nil, err
	}
	return yamuxStream{st}, nil
}

// SetDeadline 是会话级设置：只影响 AcceptStream 的等待，不作用于底层连接。
func (s *yamuxSession) SetDeadline(t time.Time) error {
	s.acceptDeadline.Store(t)
	return nil
}

// yamuxStream 把 *yamux.Stream 适配成 Stream。yamux 的 Close 本身就是半关闭：
// 发 FIN、读方向保留，因此无需额外的 CloseWrite 入口；整条流的回收由对端 FIN
// 或会话关闭完成。
type yamuxStream struct{ *yamux.Stream }

// ID 返回流标识（yamux 侧名为 StreamID）。
func (s yamuxStream) ID() uint32 { return s.StreamID() }
