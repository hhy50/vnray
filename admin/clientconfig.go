package admin

// clientconfig.go 是「客户端配置」页的读写口：只碰下发给所有远程客户端的
// 全局 Host / IP 段规则。单独一个接口是为了不和 service-config 的全量字段
// 互相覆盖——两个页面各自只提交自己负责的那一段。

import (
	"net/http"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/logging"
)

// apiGlobalClient 是全局客户端配置的传输体；数组字段始终非 nil，前端表格可直接渲染。
type apiGlobalClient struct {
	CustomHosts []config.CustomHost `json:"custom_hosts"`
	IPMaps      []config.IPMap      `json:"ip_maps"`
}

func toAPIGlobalClient(c config.GlobalClientSettings) apiGlobalClient {
	out := apiGlobalClient{CustomHosts: c.CustomHosts, IPMaps: c.IPMaps}
	if out.CustomHosts == nil {
		out.CustomHosts = []config.CustomHost{}
	}
	if out.IPMaps == nil {
		out.IPMaps = []config.IPMap{}
	}
	return out
}

// apiGlobalClientConfig 返回当前的全局客户端配置。
func (s *AdminService) apiGlobalClientConfig(w http.ResponseWriter, r *http.Request) {
	global := s.currentServiceSettings().GlobalClient.Clone()
	apiOK(w, map[string]any{"global_client": toAPIGlobalClient(global)})
}

// apiGlobalClientConfigSave 归一化后整段替换全局配置；空数组即清空。落库成功后
// 由 ServiceSettings.Apply 推送给在线客户端，离线客户端下次连接时取到。
func (s *AdminService) apiGlobalClientConfigSave(w http.ResponseWriter, r *http.Request) {
	if s.opts.Settings == nil {
		apiFailKey(w, r, http.StatusServiceUnavailable, msgRuntimeMissing)
		return
	}
	var in struct {
		GlobalClient apiGlobalClient `json:"global_client"`
	}
	if !apiDecode(w, r, &in) {
		return
	}
	global, err := normalizeGlobalClient(config.GlobalClientSettings{
		CustomHosts: in.GlobalClient.CustomHosts,
		IPMaps:      in.GlobalClient.IPMaps,
	})
	if err != nil {
		apiFailKey(w, r, http.StatusBadRequest, msgOpFailedDetail, err)
		return
	}
	next := s.currentServiceSettings()
	next.GlobalClient = global
	if err := s.opts.Settings.Apply(next); err != nil {
		logging.Error("admin: client config apply failed", "err", err)
		apiFailKey(w, r, http.StatusBadRequest, msgSaveFailedDetail, err)
		return
	}
	logging.Info("admin: client config saved", "actor", actorOf(r))
	apiOK(w, map[string]any{"saved": true})
}
