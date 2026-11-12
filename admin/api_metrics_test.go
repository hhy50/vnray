package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/monitor"
)

type monitorReader struct {
	data  []byte
	err   error
	query monitor.Query
}

func (m *monitorReader) HostJSON() ([]byte, error) { return m.data, m.err }
func (m *monitorReader) UsersJSON(_ context.Context, q monitor.Query) ([]byte, error) {
	m.query = q
	return m.data, m.err
}

func TestMonitoringAPI(t *testing.T) {
	s := newTestAdminService(t)
	reader := &monitorReader{data: []byte(`{"sampled_at":"2026-11-12T00:00:00Z","users":[]}`)}
	s.opts.Monitor = reader
	h := s.wrapOps(s.buildOpsMux())
	for _, path := range []string{"/admin/ops/api/metrics", "/admin/ops/api/monitoring"} {
		if rec := anonRequest(h, http.MethodGet, path); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s: %d", path, rec.Code)
		}
		for _, tc := range []struct {
			err    error
			status int
		}{
			{nil, http.StatusOK}, {monitor.ErrDisabled, http.StatusNotFound}, {monitor.ErrWarming, http.StatusServiceUnavailable}, {errors.New("private database error"), http.StatusServiceUnavailable},
		} {
			reader.err = tc.err
			rec := doRequest(t, h, s.auth, http.MethodGet, path)
			if rec.Code != tc.status || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
			}
			if tc.err == nil && !json.Valid(rec.Body.Bytes()) {
				t.Fatal("invalid envelope")
			}
		}
	}
}

func TestMonitoringQueryParsing(t *testing.T) {
	reader := &monitorReader{data: []byte(`{}`)}
	s := &AdminService{opts: AdminOptions{Monitor: reader}}
	for _, tc := range []struct {
		query          string
		status, points int
		window         time.Duration
	}{
		{"", 200, 0, 7 * 24 * time.Hour},
		{"?history=1&window=1h", 200, 120, time.Hour},
		{"?points=10&window=5m", 200, 10, 5 * time.Minute},
		{"?points=1", 400, 0, 0}, {"?points=121", 400, 0, 0},
		{"?window=bad", 400, 0, 0}, {"?window=-1s", 400, 0, 0}, {"?window=169h", 400, 0, 0},
	} {
		w := httptest.NewRecorder()
		s.apiMonitoring(w, httptest.NewRequest(http.MethodGet, "/api/monitoring"+tc.query, nil))
		if w.Code != tc.status {
			t.Fatalf("%s: %d", tc.query, w.Code)
		}
		if tc.status == 200 && (reader.query.Points != tc.points || reader.query.Window != tc.window) {
			t.Fatalf("query: %+v", reader.query)
		}
	}
}
