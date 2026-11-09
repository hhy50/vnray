package traffic

import (
	"context"
	"errors"
	"sync"

	"github.com/hhy50/vnray/internal/routine"
)

// Service 管理定期落库，Drain 必须在数据面停止之后调用。
type Service struct {
	meter   *Meter
	writer  Writer
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool
}

// NewService 构造流量落库服务。
func NewService(meter *Meter, writer Writer) *Service {
	return &Service{meter: meter, writer: writer}
}

// Name 返回服务名。
func (s *Service) Name() string { return "TrafficService" }

// Start 启动落库任务，由 Drain 负责取消并等待。
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil || s.stopped {
		return errors.New("traffic: service already started or stopped")
	}
	// 退出信号不能抢在数据面收尾前触发最后一次落库。
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	s.done = make(chan struct{})
	routine.Go("traffic.service", func() {
		defer close(s.done)
		s.meter.run(runCtx, s.writer)
	})
	return nil
}

// Drain 等待周期任务退出，再同步写入最后的流量增量。
func (s *Service) Drain(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return s.meter.Flush(ctx, s.writer)
	case <-ctx.Done():
		return ctx.Err()
	}
}
