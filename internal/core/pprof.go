package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/routine"
)

type pprofService struct {
	srv  *http.Server
	done chan struct{}
}

func newPprofService(addr string) *pprofService {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
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
	return &pprofService{srv: &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}}
}

// Name 返回诊断服务名。
func (s *pprofService) Name() string { return "DiagnosticsService" }

// Start 同步打开诊断监听，失败交由宿主回滚启动。
func (s *pprofService) Start(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("diagnostics listen: %w", err)
	}
	s.done = make(chan struct{})

	routine.Go("core.pprof.serve", func() {
		defer close(s.done)
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Error("pprof: serve stopped", "err", err)
		}
	})
	logging.Info("pprof: diagnostics listening", "addr", ln.Addr().String())
	return nil
}

// Drain 停止诊断请求并等待 HTTP 循环退出。
func (s *pprofService) Drain(ctx context.Context) error {
	if s.done == nil {
		return nil
	}
	err := s.srv.Shutdown(ctx)
	if err != nil {
		err = errors.Join(err, s.srv.Close())
	}
	select {
	case <-s.done:
		return err
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
}
