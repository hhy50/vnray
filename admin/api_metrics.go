package admin

import (
	"net/http"
	"time"

	"github.com/hhy50/vnray/internal/metrics"
)

// apiMetrics 返回进程指标快照：runtime 读数（goroutine/内存/GC）与全部注册序列。
// ReadMemStats 有短暂 STW，本接口只应低频调用。采集开关关闭时按 404 处理，
// 与未注册路由不可区分。
func (s *AdminService) apiMetrics(w http.ResponseWriter, r *http.Request) {
	if !metrics.Enabled() {
		http.NotFound(w, r)
		return
	}
	apiOK(w, map[string]any{
		"uptime_seconds": int64(time.Since(s.started).Seconds()),
		"runtime":        metrics.RuntimeSnapshot(),
		"metrics":        metrics.Snapshot(),
	})
}
