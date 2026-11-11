package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hhy50/vnray/internal/metrics"
)

func TestAPIMetricsRequiresAuth(t *testing.T) {
	s := newTestAdminService(t)
	h := s.wrapOps(s.buildOpsMux())
	if rec := anonRequest(h, http.MethodGet, "/admin/ops/api/metrics"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon metrics = %d, want 401", rec.Code)
	}
}

func TestAPIMetricsDisabledReturns404(t *testing.T) {
	s := newTestAdminService(t)
	h := s.wrapOps(s.buildOpsMux())
	metrics.SetEnabled(false)
	defer metrics.SetEnabled(true)
	if rec := doRequest(t, h, s.auth, http.MethodGet, "/admin/ops/api/metrics"); rec.Code != http.StatusNotFound {
		t.Fatalf("metrics while disabled = %d, want 404", rec.Code)
	}
}

func TestAPIMetricsSnapshot(t *testing.T) {
	s := newTestAdminService(t)
	s.collectMetrics(metrics.NewSampler())
	h := s.wrapOps(s.buildOpsMux())
	rec := doRequest(t, h, s.auth, http.MethodGet, "/admin/ops/api/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics = %d, want 200", rec.Code)
	}

	var out struct {
		Data struct {
			UptimeSeconds int64 `json:"uptime_seconds"`
			Process       struct {
				Goroutines int    `json:"goroutines"`
				HeapAlloc  uint64 `json:"heap_alloc"`
				Gomaxprocs int    `json:"gomaxprocs"`
			} `json:"process"`
			Metrics []struct {
				Name  string `json:"name"`
				Type  string `json:"type"`
				Value int64  `json:"value"`
			} `json:"metrics"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Data.Process.Goroutines <= 0 {
		t.Fatalf("goroutines = %d, want > 0", out.Data.Process.Goroutines)
	}
	if out.Data.UptimeSeconds < 0 {
		t.Fatalf("uptime = %d, want >= 0", out.Data.UptimeSeconds)
	}

	// routine 包已在测试进程注册过协程指标，快照里应能找到。
	found := false
	for _, m := range out.Data.Metrics {
		if m.Name == "routine_panics_total" && m.Type == "counter" {
			found = true
		}
	}
	if !found {
		t.Fatal("routine_panics_total missing from metrics snapshot")
	}
}

func TestAPIMetricsWarmupAndCachedRead(t *testing.T) {
	s := newTestAdminService(t)
	metrics.SetEnabled(true)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/ops/api/metrics", nil)
	s.apiMetrics(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("warmup status = %d", w.Code)
	}
	s.collectMetrics(metrics.NewSampler())
	first := httptest.NewRecorder()
	s.apiMetrics(first, r)
	second := httptest.NewRecorder()
	s.apiMetrics(second, r)
	if first.Code != http.StatusOK || first.Body.String() != second.Body.String() {
		t.Fatal("HTTP reads must reuse the immutable sample")
	}
	metrics.SetEnabled(false)
	metrics.SetEnabled(true)
	stale := httptest.NewRecorder()
	s.apiMetrics(stale, r)
	if stale.Code != http.StatusServiceUnavailable {
		t.Fatal("re-enabled collector returned an old sample")
	}
}

func TestMetricsSamplerLifecycle(t *testing.T) {
	s := newTestAdminService(t)
	metrics.SetEnabled(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.runMetrics(ctx) }()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for s.metricsCache.Load() == nil {
		select {
		case <-deadline.C:
			t.Fatal("sampler did not publish")
		case <-tick.C:
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sampler did not stop")
	}
}

func TestMetricsSamplerDisabledAndReenabled(t *testing.T) {
	metrics.SetEnabled(false)
	defer metrics.SetEnabled(true)
	synctest.Test(t, func(t *testing.T) {
		s := &AdminService{started: time.Now()}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); s.runMetrics(ctx) }()
		synctest.Wait()
		if s.metricsCache.Load() != nil {
			t.Fatal("disabled sampler published")
		}
		metrics.SetEnabled(true)
		time.Sleep(metricsInterval)
		synctest.Wait()
		if s.metricsCache.Load() == nil {
			t.Fatal("enabled sampler did not publish")
		}
		metrics.SetEnabled(false)
		time.Sleep(metricsInterval)
		synctest.Wait()
		if s.metricsCache.Load() != nil {
			t.Fatal("disabled sampler retained cache")
		}
		cancel()
		<-done
	})
}

func TestAPIMetricsHistogramAndExpiredSample(t *testing.T) {
	metrics.SetEnabled(true)
	s := &AdminService{started: time.Now()}
	h := metrics.NewHistogram("test_admin_latency_seconds", []float64{1})
	h.Observe(2)
	s.collectMetrics(metrics.NewSampler())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/ops/api/metrics", nil)
	s.apiMetrics(w, r)
	if w.Code != http.StatusOK || !json.Valid(w.Body.Bytes()) {
		t.Fatalf("histogram response: %d %s", w.Code, w.Body.String())
	}
	stale := *s.metricsCache.Load()
	stale.SampledAt = time.Now().Add(-4 * metricsInterval)
	s.metricsCache.Store(&stale)
	w = httptest.NewRecorder()
	s.apiMetrics(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expired sample: %d", w.Code)
	}
}
