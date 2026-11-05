package cli

// 配置文件到数据面对象的翻译实现在 internal/routing，client 与 server 共用；
// 这里只保留装配层使用的薄包装。

import (
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/routing"
)

// newRoutingDispatcher 按配置创建出站与路由规则。
func newRoutingDispatcher(rc *config.Routing) *dispatch.DefaultDispatcher {
	return routing.NewDispatcher(rc)
}

// newDirectDispatcher 创建只有直连出站的分发器：不注册任何规则，供服务端出站规则
// 的 direct 名单使用（跳过本客户端的路由规则）。
func newDirectDispatcher() *dispatch.DefaultDispatcher {
	return dispatch.NewDefaultDispatcher(nil, "direct", dispatch.NewDirectOutbound("direct"))
}
