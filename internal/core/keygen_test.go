package core

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hhy50/vnray/internal/authkey"
)

func TestKeygenCommand(t *testing.T) {
	var previous string
	for range 2 {
		cmd := NewServerCommand()
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs([]string{"keygen"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		value := strings.TrimSpace(output.String())
		if _, err := authkey.Parse(value); err != nil {
			t.Fatal(err)
		}
		if value == previous {
			t.Fatal("keygen reused a key")
		}
		previous = value
	}
}
