package config

import (
	"testing"
	"time"
)

func TestMonitoringSettingsValidation(t *testing.T) {
	for _, tc := range []struct {
		name                string
		interval, retention time.Duration
		valid               bool
	}{
		{"default", 2 * time.Second, time.Hour, true},
		{"maximum", time.Hour, 7 * 24 * time.Hour, true},
		{"zero", 0, time.Hour, false},
		{"too fast", time.Millisecond, time.Hour, false},
		{"too slow", 2 * time.Hour, 3 * time.Hour, false},
		{"too short", time.Minute, time.Second, false},
		{"too long", time.Hour, 8 * 24 * time.Hour, false},
		{"too many samples", time.Second, 10000*time.Second + time.Millisecond, false},
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
