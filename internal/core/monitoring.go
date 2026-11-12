package core

import (
	"context"
	"time"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/monitor"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/traffic"
)

func newMonitoring(st store.Store, meter *traffic.Meter, settings config.ServiceSettings) *monitor.Service {
	s := monitor.New(monitor.Options{
		Users: func(ctx context.Context) ([]monitor.UserSource, error) {
			users, err := st.ListUsers(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]monitor.UserSource, 0, len(users))
			for _, u := range users {
				out = append(out, monitor.UserSource{ID: u.ID, Name: u.Name, Protocol: string(u.Protocol), Stats: meter.UserStats(u.Protocol, u.Key)})
			}
			return out, nil
		},
		History: monitoringHistory{st},
	})
	s.Configure(monitoringSettings(settings))
	return s
}

func monitoringSettings(settings config.ServiceSettings) monitor.Settings {
	return monitor.Settings{Enabled: settings.MetricsEnabled, Interval: settings.MetricsInterval.Std(), Retention: settings.MetricsRetention.Std()}
}

type monitoringHistory struct{ store.Store }

func (h monitoringHistory) Save(ctx context.Context, at time.Time, data []byte, cutoff time.Time) error {
	var sample *store.MonitoringSample
	if data != nil {
		sample = &store.MonitoringSample{Timestamp: at.UnixMilli(), Payload: string(data)}
	}
	return h.SaveMonitoring(ctx, sample, cutoff)
}

func (h monitoringHistory) Read(ctx context.Context, since, until time.Time, limit int) ([][]byte, error) {
	rows, err := h.MonitoringHistory(ctx, since, until, limit)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(rows))
	for _, row := range rows {
		out = append(out, []byte(row.Payload))
	}
	return out, nil
}
