package admin

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/hhy50/vnray/internal/monitor"
)

func (s *AdminService) apiMetrics(w http.ResponseWriter, r *http.Request) {
	if s.opts.Monitor == nil {
		s.writeMonitoring(w, r, nil, monitor.ErrDisabled)
		return
	}
	data, err := s.opts.Monitor.HostJSON()
	s.writeMonitoring(w, r, data, err)
}

func (s *AdminService) apiMonitoring(w http.ResponseWriter, r *http.Request) {
	query := monitor.Query{Window: 7 * 24 * time.Hour}
	if r.URL.Query().Get("history") == "1" {
		query.Points = 120
	}
	if raw := r.URL.Query().Get("window"); raw != "" {
		window, err := time.ParseDuration(raw)
		if err != nil || window <= 0 || window > 7*24*time.Hour {
			apiFail(w, http.StatusBadRequest, "invalid monitoring window")
			return
		}
		query.Window = window
	}
	if raw := r.URL.Query().Get("points"); raw != "" {
		points, err := strconv.Atoi(raw)
		if err != nil || points < 2 || points > 120 {
			apiFail(w, http.StatusBadRequest, "points must be 2..120")
			return
		}
		query.Points = points
	}
	if s.opts.Monitor == nil {
		s.writeMonitoring(w, r, nil, monitor.ErrDisabled)
		return
	}
	data, err := s.opts.Monitor.UsersJSON(r.Context(), query)
	s.writeMonitoring(w, r, data, err)
}

func (s *AdminService) writeMonitoring(w http.ResponseWriter, r *http.Request, data []byte, err error) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case errors.Is(err, monitor.ErrDisabled):
		http.NotFound(w, r)
	case errors.Is(err, monitor.ErrWarming):
		w.Header().Set("Retry-After", "2")
		apiFail(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		apiFail(w, http.StatusServiceUnavailable, "monitoring history unavailable")
	default:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"code":0,"data":`))
		_, _ = w.Write(data)
		_, _ = w.Write([]byte(`}`))
	}
}
