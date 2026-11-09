package inbound_test

// testmanager_test.go 复现生产装配的插件注册表：生产路径是 core 逐个构造插件再交给
// NewRegistry，测试也照做，避免维护一个只在测试里存在的构造函数。

import (
	"context"
	"github.com/hhy50/vnray/internal/inbound"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/sqliteweb"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
)

// newTestManager 装配内置的四个插件。
func newTestManager(st store.Store, users inbound.UserStore, exits transport.ExitResolver, limits *ratelimit.Registry) *plugin.Manager {
	return plugin.NewRegistry(context.Background(),
		inbound.NewV2RayPlugin(st, users, exits, limits),
		inbound.NewSSPlugin(st, users, exits, limits),
		sqliteweb.New(st),
		inbound.NewVNRayUserPlugin(st, inbound.VNRayUserDependencies{}),
	)
}
