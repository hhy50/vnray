package cli

// 连接失败后的重连循环：客户端应长期在线，拨号失败或会话中断都不该让进程退出，
// 而是按固定间隔重连，直到 ctx 被取消。

import (
	"context"
	"errors"
	"time"

	"github.com/hhy50/vnray/internal/client"
	"github.com/hhy50/vnray/internal/logging"
)

// retryLoop 反复调用 connect 直到成功、遇到不可重试的错误或 ctx 结束。
//
// 认证被拒视为不可重试：密钥不匹配时再连一万次结果一样，只能由人改配置。
// 其余错误（拒连、超时、会话中断）都当作可自愈，按 interval 固定等待后重试。
//
// connect 返回 nil 表示会话正常结束（服务端主动关闭），同样重连——客户端的职责
// 是保持在线，而不是把断线当作退出理由。
func retryLoop(ctx context.Context, interval time.Duration, connect func(context.Context) error) error {
	for attempt := 1; ; attempt++ {
		err := connect(ctx)
		switch {
		case ctx.Err() != nil:
			logging.Info("reconnect: shutdown requested; stopped reconnecting")
			return nil
		case err == nil:
			logging.Info("reconnect: server session closed; reconnecting", "attempt", attempt)
		case errors.Is(err, client.ErrAuthRejected):
			return err
		default:
			logging.Warn("reconnect: server connection failed; reconnecting",
				"attempt", attempt,
				"interval", interval,
				"err", err)
		}
		if err := sleepCtx(ctx, interval); err != nil {
			logging.Info("reconnect: shutdown requested; stopped reconnecting")
			return nil
		}
	}
}

// sleepCtx 等待 d；ctx 先结束则立刻返回错误，用于让退出信号能打断退避等待。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
