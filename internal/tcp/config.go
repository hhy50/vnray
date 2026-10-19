package tcp

import (
	"fmt"
	"time"
)

// SessionConfig 是后端无关的多路复用会话参数：当前内置 yamux 后端，由它自行映射到
// 自己的配置，未列出的参数用后端默认值。一条链路的两端必须使用相同配置，
// 尤其 keepalive 周期要对称，否则存活检测会误杀会话。
type SessionConfig struct {
	// KeepAliveInterval 是存活探测间隔；0 表示关闭 keepalive（不推荐：
	// 半开连接将无法被发现）。
	KeepAliveInterval time.Duration
	// KeepAliveTimeout 是单次存活探测的等待上限。yamux 无独立字段，映射为
	// ConnectionWriteTimeout（其 Ping 的等待上限）。
	KeepAliveTimeout time.Duration
	// MaxStreamWindow 是单流流控窗口（字节）。yamux 要求不小于 256KiB。
	MaxStreamWindow uint32
	// AcceptBacklog 是入站流积压上限（yamux）。
	AcceptBacklog int
	// StreamOpenTimeout 是开流后等待对端确认的上限（yamux）；0 表示不限制。
	StreamOpenTimeout time.Duration
	// StreamCloseTimeout 是半关闭后等待对端 FIN 的上限（yamux）；0 表示禁用，
	// 且必须为 0：本项目的半关闭窗口可能覆盖整个下载时长——服务端在 up EOF
	// 时即 Close 隧道流，客户端要到响应搬完才回 FIN，任何有限值都会把
	// 下载途中的流强拔并 RST，造成截断。
	StreamCloseTimeout time.Duration
}

// DefaultSessionConfig 返回默认会话参数，两端必须一致。
//
// keepalive 间隔 10s、单次等待上限 30s；单流窗口 256KiB；半关闭强拔禁用。
// keepalive 只负责存活检测；应用层 Ping/Pong 负责延迟可观测，两者互不影响。
func DefaultSessionConfig() *SessionConfig {
	return &SessionConfig{
		KeepAliveInterval:  10 * time.Second,
		KeepAliveTimeout:   30 * time.Second,
		MaxStreamWindow:    256 * 1024,
		AcceptBacklog:      256,
		StreamOpenTimeout:  75 * time.Second,
		StreamCloseTimeout: 0,
	}
}

// ValidateSessionConfig 校验会话参数：0 一律表示"用后端默认"；非 0 时单流窗口不得
// 小于 yamux 的硬性下限。校验口径与 config.ServiceSettings 一致，但下限由后端在此给出。
func ValidateSessionConfig(cfg *SessionConfig) error {
	if cfg == nil {
		return nil
	}
	if cfg.KeepAliveInterval < 0 {
		return fmt.Errorf("tcp: keepalive interval must be >= 0, got %s", cfg.KeepAliveInterval)
	}
	if cfg.KeepAliveTimeout < 0 {
		return fmt.Errorf("tcp: keepalive timeout must be >= 0, got %s", cfg.KeepAliveTimeout)
	}
	if cfg.MaxStreamWindow != 0 && cfg.MaxStreamWindow < minYamuxStreamWindow {
		return fmt.Errorf("tcp: max stream window must be 0 (default) or >= %d bytes, got %d",
			minYamuxStreamWindow, cfg.MaxStreamWindow)
	}
	if cfg.AcceptBacklog < 0 {
		return fmt.Errorf("tcp: accept backlog must be >= 0, got %d", cfg.AcceptBacklog)
	}
	if cfg.StreamOpenTimeout < 0 {
		return fmt.Errorf("tcp: stream open timeout must be >= 0, got %s", cfg.StreamOpenTimeout)
	}
	return nil
}
