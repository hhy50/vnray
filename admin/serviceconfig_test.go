package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/config"
)

// TestServiceConfigMetricsEnabled 断言指标开关的保存与回读。
func TestServiceConfigMetricsEnabled(t *testing.T) {
	fake := &fakeSettings{cur: config.DefaultServiceSettings()}
	service := &AdminService{opts: AdminOptions{Settings: fake}}
	for _, enabled := range []bool{false, true} {
		w, r := apiContext(http.MethodPut, "/admin/ops/api/service-config", apiDurationFields{MetricsEnabled: enabled}, nil)
		service.apiServiceConfigSave(w, r)
		if w.Code != http.StatusOK || fake.cur.MetricsEnabled != enabled {
			t.Fatalf("save metrics=%v: status=%d, settings=%+v", enabled, w.Code, fake.cur)
		}
		w, r = apiContext(http.MethodGet, "/admin/ops/api/service-config", nil, nil)
		service.apiServiceConfig(w, r)
		var response struct {
			Data apiDurationFields `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Data.MetricsEnabled != enabled {
			t.Fatalf("get metrics=%v: %s", enabled, w.Body.String())
		}
	}
}

func TestServiceConfigTransportEncryption(t *testing.T) {
	fake := &fakeSettings{cur: config.DefaultServiceSettings()}
	service := &AdminService{opts: AdminOptions{Settings: fake}}
	for _, enabled := range []bool{false, true} {
		w, r := apiContext(http.MethodPut, "/admin/ops/api/service-config", apiDurationFields{TransportEncryption: &enabled}, nil)
		service.apiServiceConfigSave(w, r)
		if w.Code != http.StatusOK || fake.cur.TransportEncryption != enabled {
			t.Fatalf("save encryption=%v: status=%d, settings=%+v", enabled, w.Code, fake.cur)
		}
		w, r = apiContext(http.MethodGet, "/admin/ops/api/service-config", nil, nil)
		service.apiServiceConfig(w, r)
		var response struct {
			Data apiDurationFields `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Data.TransportEncryption == nil || *response.Data.TransportEncryption != enabled {
			t.Fatalf("get encryption=%v: %s", enabled, w.Body.String())
		}
		w, r = apiContext(http.MethodPut, "/admin/ops/api/service-config", apiDurationFields{LogLevel: "warn"}, nil)
		service.apiServiceConfigSave(w, r)
		if w.Code != http.StatusOK || fake.cur.TransportEncryption != enabled {
			t.Fatal("omitted encryption setting changed the saved value")
		}
	}
}

// fakeSettings 是 ServiceSettings 的测试实现：记录最后一次 Apply 的值。
type fakeSettings struct {
	cur     config.ServiceSettings
	applied bool
}

func (f *fakeSettings) Current() config.ServiceSettings { return f.cur }

func (f *fakeSettings) Apply(_ context.Context, next config.ServiceSettings) error {
	f.cur, f.applied = next, true
	return nil
}

// TestServiceConfigSave 断言接口层的解析、校验与落库。
func TestServiceConfigSave(t *testing.T) {
	fake := &fakeSettings{cur: config.DefaultServiceSettings()}
	service := &AdminService{opts: AdminOptions{Settings: fake}}

	save := func(body apiDurationFields) int {
		w, r := apiContext(http.MethodPut, "/admin/ops/api/service-config", body, nil)
		service.apiServiceConfigSave(w, r)
		return w.Code
	}

	for name, body := range map[string]apiDurationFields{
		"bad duration":    {HeartbeatInterval: "nope"},
		"bad level":       {LogLevel: "verbose", HeartbeatInterval: "2s"},
		"public host url": {PublicHost: "https://x"},
		"zero flush":      {TrafficFlushInterval: "0s"},
		"small window":    {MaxStreamWindowKB: intPtr(128)},
	} {
		if code := save(body); code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, code)
		}
	}

	code := save(apiDurationFields{
		HeartbeatEnabled:     true,
		HeartbeatInterval:    "2s",
		HeartbeatTimeout:     "1s",
		PublicHost:           "vpn.example.com",
		LogLevel:             "warn",
		TrafficFlushInterval: "3s",
		MaxStreamWindowKB:    intPtr(256),
	})
	if code != http.StatusOK {
		t.Fatalf("save: status = %d, want 200", code)
	}
	if !fake.applied {
		t.Fatal("settings were not applied")
	}
	if fake.cur.PublicHost != "vpn.example.com" || fake.cur.LogLevel != "warn" {
		t.Fatalf("applied = %+v", fake.cur)
	}
	if fake.cur.Heartbeat.Interval.Std() != 2*time.Second || !fake.cur.Heartbeat.Enabled {
		t.Fatalf("applied heartbeat = %+v", fake.cur)
	}
	if fake.cur.Session.MaxStreamWindow != 256*1024 {
		t.Fatalf("applied window = %d, want %d", fake.cur.Session.MaxStreamWindow, 256*1024)
	}
}

// TestServiceConfigSaveNoSettings 断言未装配 Settings 时写入返回 503。
func TestServiceConfigSaveNoSettings(t *testing.T) {
	service := &AdminService{}
	w, r := apiContext(http.MethodPut, "/admin/ops/api/service-config", apiDurationFields{}, nil)
	service.apiServiceConfigSave(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}
