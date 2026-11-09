package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hhy50/vnray/internal/extension"
	"github.com/hhy50/vnray/internal/logging"
)

// drainTimeout 是等待各服务收尾的默认上限。
const drainTimeout = 10 * time.Second

// Instance 是系统宿主：持有唯一的 app ctx 与全部服务。
type Instance struct {
	ctx      context.Context
	services []extension.Service
}

func newInstance(appctx context.Context, services ...extension.Service) *Instance {
	return &Instance{
		ctx:      appctx,
		services: services,
	}
}

// Run 按序启动全部服务，阻塞到 app ctx 取消（服务没有 Stop，这就是全系统退出信号），再逆序收尾。
func (h *Instance) Run() error {
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	started := make([]extension.Service, 0, len(h.services))
	for _, s := range h.services {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, h.drain(context.Background(), started))
		}
		logging.Info("instance: starting service", "service", s.Name())
		// Start 也可能在创建部分资源后失败，失败服务同样参与回收。
		started = append(started, s)
		if err := s.Start(ctx); err != nil {
			cancel()
			return errors.Join(fmt.Errorf("start service %s: %w", s.Name(), err), h.drain(context.Background(), started))
		}
	}

	target := logging.OutputTarget()
	logging.Info("server started successfully", "log", target)
	if !logging.MirrorsConsole() {
		fmt.Fprintf(os.Stdout, "vnray server started successfully, logs: %s\n", target)
	}

	<-h.ctx.Done()
	cancel()
	logging.Info("instance: app context done, draining services")
	return h.drain(context.Background(), started)
}

// drain 逆序调用实现了 Drainer 的服务；ctx 由 Background 派生，因为 app ctx 此时已取消。
func (h *Instance) drain(ctx context.Context, services []extension.Service) error {
	var errs []error
	for i := len(services) - 1; i >= 0; i-- {
		s := services[i]
		d, ok := s.(extension.Drainer)
		if !ok {
			continue
		}
		logging.Info("instance: draining service", "service", s.Name())
		// 每个服务拥有自己的期限，前一服务超时不能耗尽最后落库的预算。
		drainCtx, cancel := context.WithTimeout(ctx, drainTimeout)
		if err := d.Drain(drainCtx); err != nil {
			errs = append(errs, fmt.Errorf("drain service %s: %w", s.Name(), err))
		}
		cancel()
	}
	return errors.Join(errs...)
}
