package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/metrics"
	"github.com/hhy50/vnray/internal/pipe"
	"github.com/hhy50/vnray/internal/transport"
)

const metricsInterval = 2 * time.Second

type metricsSnapshot struct {
	encoded []byte
	metrics.Sample
	UptimeSeconds      int64                  `json:"uptime_seconds"`
	Transfer           transport.TransferInfo `json:"transfer"`
	PipeAllocations    uint64                 `json:"pipe_allocations"`
	PipeAllocatedBytes uint64                 `json:"pipe_allocated_bytes"`
}

func (s *AdminService) collectMetrics(sampler *metrics.Sampler) {
	sample := sampler.Collect()
	allocations, allocated := pipe.BufferAllocations()
	snapshot := &metricsSnapshot{
		Sample: sample, UptimeSeconds: int64(time.Since(s.started).Seconds()),
		Transfer: transport.TransferSnapshot(), PipeAllocations: allocations, PipeAllocatedBytes: allocated,
	}
	encoded, err := json.Marshal(apiEnvelope{Data: snapshot})
	if err != nil {
		s.metricsCache.Store(nil)
		logging.Warn("metrics: encode sample", "err", err)
		return
	}
	snapshot.encoded = encoded
	s.metricsCache.Store(snapshot)
}

func (s *AdminService) runMetrics(ctx context.Context) {
	sampler := metrics.NewSampler()
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if metrics.Enabled() {
			s.collectMetrics(sampler)
		} else {
			sampler.Reset()
			s.metricsCache.Store(nil)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *AdminService) apiMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !metrics.Enabled() {
		http.NotFound(w, r)
		return
	}
	snapshot := s.metricsCache.Load()
	if snapshot == nil || !snapshot.Sample.Current() || time.Since(snapshot.SampledAt) > 3*metricsInterval {
		w.Header().Set("Retry-After", "2")
		apiFail(w, http.StatusServiceUnavailable, "metrics sample is warming up")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 慢管理台连接只占用自己的 HTTP handler，不持有采样器或业务锁。
	_, _ = w.Write(snapshot.encoded)
}
