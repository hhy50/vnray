package portmap

import (
	"context"
	"time"

	"github.com/hhy50/vnray/internal/store"
)

const syncTimeout = 5 * time.Second

// MappingStore 是端口映射控制面所需的持久化接口。
type MappingStore interface {
	ListPortMappings(context.Context) ([]store.PortMapping, error)
	GetPortMapping(context.Context, uint64) (store.PortMapping, bool, error)
	SetPortMapping(context.Context, store.PortMapping) error
	DeletePortMapping(context.Context, uint64) (bool, error)
}

// Syncer 是已提交端口映射的运行态发布接口。
type Syncer interface {
	Sync(context.Context)
}

// Control 串行提交端口映射变更，并在返回前发布到运行态。
type Control struct {
	store   MappingStore
	runtime Syncer
	writes  chan struct{}
}

// NewControl 创建端口映射控制面，runtime 为 nil 时只持久化。
func NewControl(st MappingStore, runtime Syncer) *Control {
	return &Control{store: st, runtime: runtime, writes: make(chan struct{}, 1)}
}

// List 返回持久化的端口映射快照。
func (c *Control) List(ctx context.Context) ([]store.PortMapping, error) {
	return c.store.ListPortMappings(ctx)
}

func (c *Control) lock(ctx context.Context) error {
	select {
	case c.writes <- struct{}{}:
		if err := ctx.Err(); err != nil {
			c.unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Control) unlock() { <-c.writes }

// Save 保存完整映射；局部变更须使用 Update，避免覆盖并发编辑。
func (c *Control) Save(ctx context.Context, mapping store.PortMapping) error {
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer c.unlock()
	if err := c.store.SetPortMapping(ctx, mapping); err != nil {
		return err
	}
	c.publish(ctx)
	return nil
}

// Update 在同一写入序列中读取、修改、保存并发布映射，bool 表示目标存在。
func (c *Control) Update(ctx context.Context, id uint64, mutate func(*store.PortMapping) error) (store.PortMapping, bool, error) {
	if err := c.lock(ctx); err != nil {
		return store.PortMapping{}, false, err
	}
	defer c.unlock()
	mapping, ok, err := c.store.GetPortMapping(ctx, id)
	if err != nil || !ok {
		return mapping, ok, err
	}
	if err := mutate(&mapping); err != nil {
		return mapping, true, err
	}
	mapping.ID = id
	if err := c.store.SetPortMapping(ctx, mapping); err != nil {
		return mapping, true, err
	}
	c.publish(ctx)
	return mapping, true, nil
}

// Delete 删除映射并发布当前持久化快照。
func (c *Control) Delete(ctx context.Context, id uint64) (bool, error) {
	if err := c.lock(ctx); err != nil {
		return false, err
	}
	defer c.unlock()
	deleted, err := c.store.DeletePortMapping(ctx, id)
	if err != nil {
		return false, err
	}
	c.publish(ctx)
	return deleted, nil
}

func (c *Control) publish(ctx context.Context) {
	if c.runtime == nil {
		return
	}
	// 提交后的发布必须完成，HTTP 断开只取消提交前的工作。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncTimeout)
	defer cancel()
	c.runtime.Sync(ctx)
}
