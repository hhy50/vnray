package core

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/monitor"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/traffic"
)

func TestMonitoringAssemblyPersistenceAndConfiguration(t *testing.T) {
	dir := os.Getenv("VNRAY_MONITOR_ASSEMBLY_DIR")
	if dir == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMonitoringAssemblyPersistenceAndConfiguration$")
		cmd.Env = append(os.Environ(), "VNRAY_MONITOR_ASSEMBLY_DIR="+t.TempDir())
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("assembly: %v\n%s", err, output)
		}
		return
	}
	dataPath := filepath.Join(dir, "bindings.db")
	storage := config.StorageConfig{MonitoringDir: filepath.Join(dir, "history")}
	ctx := context.Background()
	st, err := store.Open(ctx, "sqlite", dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	meter := traffic.NewMeter(nil)
	for _, proto := range []store.Protocol{store.ProtocolSS, store.ProtocolVMess} {
		if err := st.SetUser(ctx, store.UserBinding{Protocol: proto, Key: "private-user-credential", Name: "same-name", Client: "edge", Status: store.UserEnabled}); err != nil {
			t.Fatal(err)
		}
		meter.RestoreUser(proto, "private-user-credential")
	}
	meter.UserStats(store.ProtocolVMess, "private-user-credential").Request()
	settings := config.DefaultServiceSettings()
	settings.MetricsEnabled = true
	s, err := newMonitoring(st, meter, settings, storage)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Drain(ctx)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	var data []byte
	for len(data) == 0 {
		data, _ = s.UsersJSON(ctx, monitor.Query{Window: time.Hour, Points: 120})
		if len(data) != 0 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("no sample published")
		case <-tick.C:
		}
	}
	if strings.Contains(string(data), "private-user-credential") {
		t.Fatal("credential leaked in snapshot")
	}
	var response monitor.Response
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Users) != 2 || len(response.History) != 1 || !response.HistoryStored || response.Users[0].ID == response.Users[1].ID {
		t.Fatalf("response: %s", data)
	}
	if response.Users[1].Requests != 1 {
		t.Fatal("wrong protocol counter")
	}
	if response.History[0].Users[1].Requests != 1 || response.History[0].Users[1].Name != "same-name" {
		t.Fatal("history lost user counters or directory metadata")
	}
	sc := newTestServiceConfig(st, settings)
	sc.monitor = s
	next := sc.Current()
	next.MetricsEnabled = false
	next.MetricsInterval = config.Duration(2 * time.Second)
	next.MetricsRetention = config.Duration(7 * 24 * time.Hour)
	if err := sc.Apply(ctx, next); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HostJSON(); err != monitor.ErrDisabled {
		t.Fatalf("disable: %v", err)
	}
	loaded := loadServiceSettings(ctx, st, &config.Server{})
	if loaded.MetricsInterval != next.MetricsInterval || loaded.MetricsRetention != next.MetricsRetention {
		t.Fatal("monitoring settings not persisted")
	}
	if err := s.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, "sqlite", dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	history, err := monitor.NewHistory(storage.MonitoringDir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	rows, err := history.Read(ctx, time.Now().Add(-time.Hour), time.Now(), 120, []uint64{response.Users[0].ID, response.Users[1].ID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("history: %v, %v", rows, err)
	}
	if rows[0].Users[1].Requests != 1 {
		t.Fatal("tstorage did not preserve counters across reopen")
	}
	if users, err := reopened.ListUsers(ctx); err != nil || len(users) != 2 {
		t.Fatalf("business data: %d users, %v", len(users), err)
	}
}
