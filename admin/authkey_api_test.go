package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/store"
)

func TestAPIKeyGeneration(t *testing.T) {
	s := newTestAdminService(t)
	s.opts.Bindings = NewBindings(s.opts.Store.(store.Store))
	generated := make(map[string]bool)
	for _, tc := range []struct {
		body   string
		status int
		scope  string
	}{
		{`{"scope":"*"}`, http.StatusOK, "*"},
		{`{"scope":"team:*"}`, http.StatusOK, "team:*"},
		{`{"scope":"team:*"}`, http.StatusOK, "team:*"},
		{`{"scope":"team:edge"}`, http.StatusOK, "team:edge"},
		{`{"scope":"*","key":"manual-password"}`, http.StatusBadRequest, ""},
		{`{"scope":"*","key":"` + authkey.Generate() + `"}`, http.StatusBadRequest, ""},
		{`{"scope":"bad:*:*"}`, http.StatusBadRequest, ""},
		{`{"scope":"team:group:edge"}`, http.StatusBadRequest, ""},
		{`{"scope":"edge"}`, http.StatusBadRequest, ""},
		{`{"scope":"team:"}`, http.StatusBadRequest, ""},
		{`{"scope":":edge"}`, http.StatusBadRequest, ""},
		{`{"scope":"team:ed*"}`, http.StatusBadRequest, ""},
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
		if response.Data.Scope != tc.scope {
			t.Fatal("scope not preserved")
		}
		if generated[response.Data.Key] {
			t.Fatal("creation reused a key")
		}
		generated[response.Data.Key] = true
	}
	keys := s.opts.Bindings.AuthKeySnapshot(context.Background())
	if len(keys) != len(generated) || len(keys) != 4 {
		t.Fatal("creation reused or accepted an external key")
	}
}
