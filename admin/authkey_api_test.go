package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hhy50/vnray/internal/authkey"
)

func TestAPIKeyGeneration(t *testing.T) {
	s := newTestAdminService(t)
	s.opts.Bindings = NewBindings(s.opts.Store)
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"scope":"team:*"}`, http.StatusOK},
		{`{"scope":"team:*"}`, http.StatusOK},
		{`{"scope":"*","key":"manual-password"}`, http.StatusBadRequest},
		{`{"scope":"*","key":"` + authkey.Generate() + `"}`, http.StatusBadRequest},
		{`{"scope":"bad:*:*"}`, http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodPost, "/admin/ops/api/keys", strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		s.apiKeySave(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("key create status=%d, want %d", rec.Code, tc.status)
		}
		if tc.status != http.StatusOK {
			continue
		}
		var response struct {
			Data struct {
				Key   string
				Scope string
			}
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if _, err := authkey.Parse(response.Data.Key); err != nil {
			t.Fatal(err)
		}
		if response.Data.Scope != "team:*" {
			t.Fatal("scope not preserved")
		}
	}
	keys := s.opts.Bindings.AuthKeySnapshot()
	if len(keys) != 2 || keys[0].Key == keys[1].Key {
		t.Fatal("creation reused or accepted an external key")
	}
}
