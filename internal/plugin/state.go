package plugin

import (
	"context"

	"github.com/hhy50/vnray/internal/store"
)

// SaveState 读改写一条 vn_plugin 记录：status 为空串表示沿用库里原值（只改端口或口令时
// 不该顺手改启停状态），mutate 就地改 Config，记录不存在时以 name 起手。
// 四个插件实现都走这一个写口，避免各自的"读—改—写"出现细微差异。
func SaveState(ctx context.Context, st store.Store, name string, status store.PluginStatus, mutate func(*store.PluginConfig)) error {
	cur, ok, err := st.GetPlugin(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		cur = store.PluginState{Name: name}
	}
	cur.Name = name
	if status != "" {
		cur.Status = status
	}
	if cur.Config.Ports == nil {
		cur.Config.Ports = map[store.Protocol]int{}
	}
	if mutate != nil {
		mutate(&cur.Config)
	}
	return st.SetPlugin(ctx, cur)
}
