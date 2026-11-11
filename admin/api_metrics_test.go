package admin

import (
	"encoding/json"
	"net/http"
	"testing"

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
	h := s.wrapOps(s.buildOpsMux())
	rec := doRequest(t, h, s.auth, http.MethodGet, "/admin/ops/api/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics = %d, want 200", rec.Code)
	}

	var out struct {
		Data struct {
			UptimeSeconds int64 `json:"uptime_seconds"`
			Runtime       struct {
				Goroutines int     `json:"goroutines"`
				HeapAlloc  uint64  `json:"heap_alloc"`
				Gomaxprocs int     `json:"gomaxprocs"`
				Threads    int     `json:"threads"`
				GCCPUFrac  float64 `json:"gc_cpu_fraction"`
			} `json:"runtime"`
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
	if out.Data.Runtime.Goroutines <= 0 {
		t.Fatalf("goroutines = %d, want > 0", out.Data.Runtime.Goroutines)
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
