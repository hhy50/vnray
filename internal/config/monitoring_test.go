package config

import (
	"testing"
	"time"
)

func TestMonitoringDirectoryConfig(t *testing.T) {
	path := writeYAML(t, "storage:\n  driver: sqlite\n  dsn: metrics.db\n  monitoring_dir: metrics-history\n")
	s := DefaultServer()
	if err := Load(path, &s); err != nil {
		t.Fatal(err)
	}
	if s.Storage.DSN != "metrics.db" || s.Storage.ResolvedDriver() != "sqlite" || s.Storage.ResolvedMonitoringDir() != "metrics-history" {
		t.Fatal("monitoring directory or database configuration lost")
	}
	if got := (StorageConfig{}).ResolvedMonitoringDir(); got != "data/monitoring" {
		t.Fatalf("default monitoring directory: %s", got)
	}
}

func TestMonitoringSettingsValidation(t *testing.T) {
	for _, tc := range []struct {
		name                string
		interval, retention time.Duration
		valid               bool
	}{
		{"default", 2 * time.Second, time.Hour, true},
		{"maximum", time.Hour, 7 * 24 * time.Hour, true},
		{"seven days at two seconds", 2 * time.Second, 7 * 24 * time.Hour, true},
		{"seven days at one second", time.Second, 7 * 24 * time.Hour, true},
		{"zero", 0, time.Hour, false},
		{"too fast", time.Millisecond, time.Hour, false},
		{"too slow", 2 * time.Hour, 3 * time.Hour, false},
		{"too short", time.Minute, time.Second, false},
		{"too long", time.Hour, 8 * 24 * time.Hour, false},
		{"above former sample limit", time.Second, 10000*time.Second + time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := DefaultServiceSettings()
			s.MetricsInterval, s.MetricsRetention = Duration(tc.interval), Duration(tc.retention)
			if err := s.Validate(); (err == nil) != tc.valid {
				t.Fatalf("validation: %v", err)
			}
		})
	}
}
