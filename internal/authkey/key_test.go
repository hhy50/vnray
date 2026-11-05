package authkey

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestGeneratedKeys(t *testing.T) {
	seen := make(map[ID]bool)
	for range 100 {
		value := Generate()
		key, err := Parse(value)
		if err != nil || len(value) != 44 || seen[key.ID()] {
			t.Fatalf("invalid or repeated generated key: %v", err)
		}
		seen[key.ID()] = true
	}
}

func TestRejectInvalidKeys(t *testing.T) {
	valid := Generate()
	for _, value := range []string{"", "human-password", strings.Repeat("a", 64), valid + "\n", strings.TrimRight(valid, "="), base64.StdEncoding.EncodeToString(make([]byte, 31))} {
		if _, err := Parse(value); !errors.Is(err, ErrInvalid) {
			t.Errorf("expected invalid key, got %v", err)
		}
	}
}
