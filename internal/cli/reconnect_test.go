package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/client"
	"github.com/hhy50/vnray/internal/config"
)

// 会话正常结束也算断线：客户端必须继续重连，否则服务端重启后客户端就再也回不来。
func TestRetryLoopKeepsRetryingAfterSessionClose(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := retryLoop(ctx, time.Millisecond, func(context.Context) error {
		calls.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("retryLoop = %v, want nil on shutdown", err)
	}
	if calls.Load() < 2 {
		t.Fatalf("connect called %d times, want >= 2", calls.Load())
	}
}

// 前几次失败后成功：说明失败被重试了，而不是第一次就冒泡出去。
func TestRetryLoopRetriesAfterFailures(t *testing.T) {
	const failFirst = 3
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := retryLoop(ctx, time.Millisecond, func(context.Context) error {
		if calls.Add(1) <= failFirst {
			return errors.New("connection refused")
		}
		cancel()
		return nil
	})
	if err != nil {
		t.Fatalf("retryLoop = %v, want nil on shutdown", err)
	}
	if got := calls.Load(); got < failFirst+1 {
		t.Fatalf("connect called %d times, want >= %d", got, failFirst+1)
	}
}

// 认证被拒是配置问题：重试一万次结果一样，必须立刻把错误交回调用方。
func TestRetryLoopStopsOnAuthRejected(t *testing.T) {
	var calls atomic.Int32
	rejected := fmt.Errorf("dial server: %w", client.ErrAuthRejected)
	err := retryLoop(context.Background(), time.Millisecond, func(context.Context) error {
		calls.Add(1)
		return rejected
	})
	if !errors.Is(err, client.ErrAuthRejected) {
		t.Fatalf("err = %v, want ErrAuthRejected", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("connect called %d times, want 1", got)
	}
}

// 退避必须真的等满间隔，而不是失败后立刻重连。
func TestRetryLoopHonoursInterval(t *testing.T) {
	const interval = 60 * time.Millisecond
	var mu sync.Mutex
	var stamps []time.Time
	ctx, cancel := context.WithTimeout(context.Background(), 5*interval)
	defer cancel()
	_ = retryLoop(ctx, interval, func(context.Context) error {
		mu.Lock()
		stamps = append(stamps, time.Now())
		mu.Unlock()
		return errors.New("connection refused")
	})
	mu.Lock()
	defer mu.Unlock()
	if len(stamps) < 2 {
		t.Fatalf("connect called %d times, want >= 2", len(stamps))
	}
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap < interval {
			t.Fatalf("第 %d 次重连间隔 %v < %v（退避未生效）", i, gap, interval)
		}
	}
}

func TestRetryLoopStopsOnShutdownDuringBackoff(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		// 第一次失败后进入一分钟退避；此时取消 ctx 必须立刻收敛。
		done <- retryLoop(ctx, time.Minute, func(context.Context) error {
			calls.Add(1)
			cancel()
			return errors.New("connection refused")
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("retryLoop = %v, want nil on shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retryLoop 未在 ctx 取消后返回（退避等待不可打断）")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("connect called %d times, want 1", got)
	}
}

// 关闭重连时只尝试一次，拨号失败直接返回给调用方（由进程管理器拉起）。
func TestRunWithoutReconnectReturnsDialError(t *testing.T) {
	app, err := newClientApp(configWithReconnectDisabled())
	if err != nil {
		t.Fatal(err)
	}
	if app.reconnect.Enabled {
		t.Fatal("reconnect 应为关闭状态")
	}
	err = app.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dial server") {
		t.Fatalf("Run = %v, want dial server error", err)
	}
}

// configWithReconnectDisabled 造一个指向必然拒连地址、且关闭重连的客户端配置。
func configWithReconnectDisabled() config.Client {
	cfg := config.DefaultClient()
	cfg.Server.Addr = "127.0.0.1:1"
	cfg.Server.Reconnect = &config.Reconnect{Enabled: false}
	return cfg
}
