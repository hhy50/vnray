package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestTransportKeyValidation(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString(make([]byte, 32))
	for _, tc := range []struct {
		name  string
		key   string
		valid bool
	}{
		{"generated format", valid, true},
		{"unconfigured", "", true},
		{"old password", "vnray-shared-key", false},
		{"old hex key", strings.Repeat("a", 48), false},
		{"short decoded key", base64.StdEncoding.EncodeToString(make([]byte, 31)), false},
		{"missing padding", strings.TrimRight(valid, "="), false},
		{"extra newline", valid + "\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := DefaultServer()
			server.Server.Auth.Key = tc.key
			client := DefaultClient()
			client.Server.AuthKey = tc.key
			for _, err := range []error{server.Validate(), client.Validate()} {
				if (err == nil) != tc.valid {
					t.Fatalf("validation = %v, want valid=%v", err, tc.valid)
				}
			}
		})
	}
}
