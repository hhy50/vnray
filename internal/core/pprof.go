package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	_ "net/http/pprof" // 注册 /debug/pprof 处理器到 DefaultServeMux
	"runtime"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/routine"
)

// startPprof 启动诊断端点（diagnostics.pprof_listen 非空时）：
//   - /debug/pprof/：CPU/堆/协程等标准画像（net/http/pprof）。
//   - /debug/runtime：runtime.MemStats 与协程数的 JSON 快照，供脚本采样。
//
// 端点随 app ctx 退出而关闭；只应绑定回环地址，调用方不做再校验。
func startPprof(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.Handle("/debug/pprof/", http.DefaultServeMux)
	mux.HandleFunc("/debug/runtime", func(w http.ResponseWriter, _ *http.Request) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"num_goroutine":    runtime.NumGoroutine(),
			"heap_alloc":       m.HeapAlloc,
			"heap_objects":     m.HeapObjects,
			"heap_inuse":       m.HeapInuse,
			"heap_sys":         m.HeapSys,
			"heap_idle":        m.HeapIdle,
			"heap_released":    m.HeapReleased,
			"stack_inuse":      m.StackInuse,
			"sys":              m.Sys,
			"num_gc":           m.NumGC,
			"gc_cpu_fraction":  m.GCCPUFraction,
			"last_gc_delta_ms": time.Since(time.Unix(0, int64(m.LastGC))).Milliseconds(),
		})
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	routine.Go("core.pprof.serve", func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logging.Errorf("pprof: serve %s: %v", addr, err)
		}
	})
	routine.Go("core.pprof.watchClose", func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logging.Warnf("pprof: shutdown: %v", err)
		}
	})
	logging.Info(fmt.Sprintf("pprof: diagnostics listening on http://%s/debug/pprof", addr))
}
