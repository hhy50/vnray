package core

import (
	"context"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/transport"
)

// HeartbeatOptions 是服务端侧的心跳策略。
//
// 心跳解决的是**延迟可观测**：多路复用层的 keepalive 只发不回显时间戳的 NOP 帧，
// 能测出断连却测不出时延，所以延迟由应用层自己的 Ping/Pong 测。
// Interval 或 Timeout 不为正数时心跳整体关闭，存活检测仍由 keepalive 兜底。
type HeartbeatOptions struct {
	Interval time.Duration
	// Timeout 是单次 Pong 的等待上限。超时只记录并跳过，**不主动断连**：
	// 偶发卡顿不该被放大成抖动，断连交给多路复用层的 keepalive 判定。
	Timeout time.Duration
}

// Enabled 判断心跳是否开启。
func (o HeartbeatOptions) Enabled() bool { return o.Interval > 0 && o.Timeout > 0 }

// runHeartbeatScheduler 是全体客户端会话共享的心跳调度（单协程多路复用）：
// 一个 ticker 每拍对当时在线的所有会话并行发起探测，替代每会话一个常驻协程。
// 由 Start 以组件 ctx 启动，ctx 取消即退出；会话级启停由登记表天然表达。
//
// 协程常驻（即使初始未启用心跳）：策略可经 ApplyHeartbeat 在运行期热改，
// 收到 reload 信号即按当前间隔重建 ticker；禁用时 ticker 通道置 nil，不再探测。
func (s *ConnectorService) runHeartbeatScheduler(ctx context.Context) {
	var tk *time.Ticker
	var tick <-chan time.Time
	apply := func(o HeartbeatOptions) {
		if tk != nil {
			tk.Stop()
			tk = nil
		}
		if o.Enabled() {
			tk = time.NewTicker(o.Interval)
			tick = tk.C
		} else {
			tick = nil
		}
	}
	// 装配期 ApplyHeartbeat 已落值并唤醒一次，调度器首拍与紧随其后的 reload 拿到的是同一
	// 策略；值未变就不重复输出，避免同一策略连打两条。
	var lastLogged HeartbeatOptions
	logged := false
	logPolicy := func(o HeartbeatOptions) {
		if logged && o == lastLogged {
			return
		}
		lastLogged, logged = o, true
		logging.Infof("heartbeat policy, enabled=%v, interval=%s, timeout=%s", o.Enabled(), o.Interval, o.Timeout)
	}
	apply(s.currentHeartbeat())
	logPolicy(s.currentHeartbeat())
	defer func() {
		if tk != nil {
			tk.Stop()
		}
		logging.Infof("heartbeat scheduler stopped")
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.heartbeatReload:
			o := s.currentHeartbeat()
			apply(o)
			logPolicy(o)
		case <-tick:
			s.pingClients()
		}
	}
}

// currentHeartbeat 返回当前心跳策略；尚未设置（如直接构造的零值服务）时用零值（禁用）。
func (s *ConnectorService) currentHeartbeat() HeartbeatOptions {
	if o := s.heartbeat.Load(); o != nil {
		return *o
	}
	return HeartbeatOptions{}
}

// pingClients 对当时在线的全部客户端会话并行发起一次探测。
func (s *ConnectorService) pingClients() {
	for _, sess := range s.clientSessions() {
		if sess.IsClosed() {
			continue
		}
		// Timeout > Interval 时同一会话的前一次探测可能未归，跳过本拍避免重叠。
		if !sess.BeginPing() {
			continue
		}
		routine.Go("core.session.ping", func() {
			defer sess.EndPing()
			s.pingSession(sess)
		})
	}
}

// clientSessions 快照当前在线的客户端会话（用户会话不跑心跳）。
func (s *ConnectorService) clientSessions() []*transport.Session {
	return s.sessions.ClientSessions()
}

// pingSession 对单个会话做一次探测，把成功结果写回会话。
func (s *ConnectorService) pingSession(sess *transport.Session) {
	rtt, err := sess.PingRTT(s.currentHeartbeat().Timeout)
	if err != nil {
		// 会话已关闭时这里必然报错，不再重复记日志。
		if sess.IsClosed() {
			return
		}
		logging.Warnf("heartbeat failed: %v", err)
		return
	}
	sess.RecordHeartbeat(rtt)
	logging.Debugf("heartbeat ok, rtt=%s", rtt)
}
