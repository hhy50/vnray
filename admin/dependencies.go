package admin

import (
	"context"

	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
)

// TrafficReader 提供管理面展示所需的流量快照。
type TrafficReader interface {
	ListUserTraffic(context.Context) ([]store.UserTraffic, error)
	ListClientTraffic(context.Context) ([]store.ClientTraffic, error)
}

// PluginRegistry 提供管理面所需的插件查询能力。
type PluginRegistry interface {
	List() []plugin.Status
	Get(string) (plugin.Plugin, error)
}

// MappingSettings 提供端口映射的提交与运行态发布入口。
type MappingSettings interface {
	List(context.Context) ([]store.PortMapping, error)
	Save(context.Context, store.PortMapping) error
	Update(context.Context, uint64, func(*store.PortMapping) error) (store.PortMapping, bool, error)
	Delete(context.Context, uint64) (bool, error)
}

// MappingStatus 提供端口映射监听状态。
type MappingStatus interface {
	ListeningIDs() map[uint64]bool
}

// UserBindings 提供管理面所需的用户、客户端与密钥操作。
type UserBindings interface {
	Snapshot(context.Context) []Binding
	Lookup(context.Context, store.Protocol, string) (Binding, bool)
	UpdateUser(context.Context, store.Protocol, string, func(*Binding) error) (Binding, error)
	DeleteUser(context.Context, store.Protocol, string) (bool, error)
	ClientSnapshot(context.Context) []ClientBinding
	LookupClient(context.Context, string) (ClientBinding, bool)
	SaveClientServerConfig(context.Context, string, string) error
	UpdateClientServerConfig(context.Context, string, func(string) (string, error)) error
	PushClientConfig(context.Context, *transport.Session) error
	AuthKeySnapshot(context.Context) []store.AuthKey
	SetAuthKey(context.Context, string, string) error
	DeleteAuthKey(context.Context, string) (bool, error)
}
