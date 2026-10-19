package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/config"
)

// fakeSettings 是 ServiceSettings 的测试实现：记录最后一次 Apply 的值。
type fakeSettings struct {
	cur     config.ServiceSettings
	applied bool
}

func (f *fakeSettings) Current() config.ServiceSettings { return f.cur }

func (f *fakeSettings) Apply(next config.ServiceSettings) error {
	f.cur, f.applied = next, true
	return nil
}

// TestHandleServiceConfigPost 断言表单解析、校验与成功重定向。
func TestHandleServiceConfigPost(t *testing.T) {
	fake := &fakeSettings{cur: config.DefaultServiceSettings()}
	service := &AdminService{opts: AdminOptions{Settings: fake}}

	post := func(fields url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/admin/ops/_service-config", strings.NewReader(fields.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		service.handleServiceConfigPost(w, r)
		return w
	}

	for name, fields := range map[string]url.Values{
		"bad duration":    {"heartbeat_interval": {"nope"}},
		"bad level":       {"log_level": {"verbose"}},
		"public host url": {"public_host": {"https://x"}},
		"zero flush":      {"traffic_flush_interval": {"0s"}},
		"small window":    {"max_stream_window_kb": {"128"}},
	} {
		if w := post(fields); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, w.Code)
		}
	}

	w := post(url.Values{
		"heartbeat_enabled":      {"on"},
		"heartbeat_interval":     {"2s"},
		"heartbeat_timeout":      {"1s"},
		"public_host":            {"vpn.example.com"},
		"log_level":              {"warn"},
		"traffic_flush_interval": {"3s"},
		"max_stream_window_kb":   {"256"},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d, want 303", w.Code)
	}
	if !fake.applied {
		t.Fatal("settings were not applied")
	}
	if fake.cur.PublicHost != "vpn.example.com" || fake.cur.LogLevel != "warn" {
		t.Fatalf("applied = %+v", fake.cur)
	}
	if fake.cur.Heartbeat.Interval.Std() != 2*time.Second || !fake.cur.Heartbeat.Enabled {
		t.Fatalf("applied heartbeat = %+v", fake.cur.Heartbeat)
	}
	if fake.cur.Session.MaxStreamWindow != 256*1024 {
		t.Fatalf("applied window = %d, want %d", fake.cur.Session.MaxStreamWindow, 256*1024)
	}
}

// TestHandleServiceConfigPostNoSettings 断言未装配 Settings 时写入返回 503。
func TestHandleServiceConfigPostNoSettings(t *testing.T) {
	service := &AdminService{}
	r := httptest.NewRequest(http.MethodPost, "/admin/ops/_service-config", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	service.handleServiceConfigPost(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}
