package core

import (
	"context"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/monitor"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/traffic"
)

func newMonitoring(st store.Store, meter *traffic.Meter, settings config.ServiceSettings, storage config.StorageConfig) (*monitor.Service, error) {
	history, err := monitor.NewHistory(storage.ResolvedMonitoringDir(), settings.MetricsRetention.Std())
	if err != nil {
		return nil, err
	}
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
		History: history,
	})
	s.Configure(monitoringSettings(settings))
	return s, nil
}

func monitoringSettings(settings config.ServiceSettings) monitor.Settings {
	return monitor.Settings{Enabled: settings.MetricsEnabled, Interval: settings.MetricsInterval.Std(), Retention: settings.MetricsRetention.Std()}
}
