package admin

import (
	"time"

	"github.com/hhy50/vnray/internal/access"
	"github.com/hhy50/vnray/internal/store"
)

const dbTimeout = 5 * time.Second

// Binding 是用户绑定的兼容视图。
type Binding = access.Binding

// ClientBinding 是客户端注册记录的兼容视图。
type ClientBinding = access.ClientBinding

// UserChange 描述一条用户记录的变更。
type UserChange = access.UserChange

// UserObserver 接收已持久化的用户变更。
type UserObserver = access.UserObserver

// Bindings 保留原管理面 API，业务实现由 access 包提供。
type Bindings = access.Bindings

// NewBindings 创建共享的用户与客户端访问服务。
func NewBindings(st store.Store) *Bindings { return access.NewBindings(st) }
