package client

// 客户端侧的握手认证配置与小工具：仅共享 internal/tcp 的会话抽象，
// 不依赖服务端专用包（core/admin/store）。

import (
	"errors"
	"time"

	"github.com/hhy50/vnray/internal/tcp"
)

// AuthTimeout 是握手各阶段的等待上限（两端等对端帧）。
const AuthTimeout = 5 * time.Second

// ErrAuthRejected 表示服务端拒绝了这次认证。
var ErrAuthRejected = errors.New("authentication rejected")

// AuthOptions 是握手认证配置：两端共用同一个密钥，客户端上报、服务端比对。
type AuthOptions struct {
	Key string
}

// sessionConfigOrDefault 返回生效的多路复用会话配置：未指定时用默认值。
func sessionConfigOrDefault(cfg *tcp.SessionConfig) *tcp.SessionConfig {
	if cfg != nil {
		return cfg
	}
	return tcp.DefaultSessionConfig()
}
