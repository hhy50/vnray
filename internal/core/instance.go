package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/extension"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/portmap"
)

// drainTimeout 是等待各服务收尾的默认上限。
const drainTimeout = 10 * time.Second

// Instance 是系统宿主：持有唯一的 app ctx 与全部服务。
type Instance struct {
	ctx      context.Context
	services []extension.Service
}

// newInstance 组装服务列表；管理面可为 nil（admin.enabled 为 false）。
// 端口映射排在插件之后：Drain 逆序先关映射监听，再收数据面与管理面。
func newInstance(appctx context.Context, connector *ConnectorService, adminService *admin.AdminService, plugins *plugin.Manager, portmaps *portmap.Manager) *Instance {
	// 管理面可选：admin.enabled 为 false 时装配层给 nil，跳过不进服务列表。
	services := make([]extension.Service, 0, 4)
	if adminService != nil {
		services = append(services, adminService)
	}
	services = append(services, connector, plugins, portmaps)
	return &Instance{
		ctx:      appctx,
		services: services,
	}
}

// Run 按序启动全部服务，阻塞到 app ctx 取消（服务没有 Stop，这就是全系统退出信号），再逆序收尾。
func (h *Instance) Run() error {
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()

	for _, s := range h.services {
		logging.Info("instance: starting service", "service", s.Name())
		if err := s.Start(ctx); err != nil {
			return fmt.Errorf("start service %s: %w", s.Name(), err)
		}
	}

	<-h.ctx.Done()
	logging.Info("instance: app context done, draining services")
	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	return h.drain(drainCtx)
}

// drain 逆序调用实现了 Drainer 的服务；ctx 由 Background 派生，因为 app ctx 此时已取消。
func (h *Instance) drain(ctx context.Context) error {
	var errs []error
	for i := len(h.services) - 1; i >= 0; i-- {
		s := h.services[i]
		d, ok := s.(extension.Drainer)
		if !ok {
			continue
		}
		logging.Info("instance: draining service", "service", s.Name())
		if err := d.Drain(ctx); err != nil {
			errs = append(errs, fmt.Errorf("drain service %s: %w", s.Name(), err))
		}
	}
	return errors.Join(errs...)
}
