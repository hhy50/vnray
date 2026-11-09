package inbound

import (
	"fmt"

	"github.com/hhy50/vnray/internal/store"
)

// InitialStates 返回首次数据库初始化使用的插件记录，不写库或启动监听。
// 只负责本包实现的插件；包外实现（如 sqlite-web）的默认值由装配层追加。
func InitialStates(names []string) ([]store.PluginState, error) {
	var states []store.PluginState
	for _, name := range names {
		cfg := store.PluginConfig{}
		switch name {
		case pluginV2Ray:
			cfg.Ports = map[store.Protocol]int{store.ProtocolVMess: defaultVMessPort, store.ProtocolVLESS: defaultVLESSPort}
		case pluginSS, pluginVNRayUser:
			continue
		default:
			return nil, fmt.Errorf("plugin: unknown plugin %q in config", name)
		}
		states = append(states, store.PluginState{Name: name, Status: store.PluginEnabled, Config: cfg})
	}
	return states, nil
}
