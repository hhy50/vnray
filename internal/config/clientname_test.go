package config

import "testing"

func TestValidateClientIdentity(t *testing.T) {
	for _, tt := range []struct {
		group string
		name  string
		valid bool
	}{
		{"default", "host-192.168.0.2", true},
		{"office", "edge-1", true},
		{"", "edge-1", false},
		{"office", "", false},
		{"", "", false},
		{"", "default:edge-1", false},
		{"office", "default:edge-1", false},
		{"office:lab", "edge-1", false},
		{"team*", "edge-1", false},
		{"office", "edge*", false},
		{"office", "*", false},
	} {
		err := ValidateClientIdentity(tt.group, tt.name)
		if (err == nil) != tt.valid {
			t.Errorf("ValidateClientIdentity(%q, %q) = %v, want valid=%v", tt.group, tt.name, err, tt.valid)
		}
	}
}

func TestClientValidateNameFormat(t *testing.T) {
	for _, tc := range []struct {
		label string
		group string
		name  string
		valid bool
	}{
		{"defaults", "", "", true},
		{"group only", "office", "", true},
		{"name only", "", "edge-1", true},
		{"explicit identity", "office", "edge-1", true},
		{"group separator", "office:lab", "edge-1", false},
		{"group wildcard", "office*", "edge-1", false},
		{"group wildcard with generated name", "*", "", false},
		{"combined name", "", "office:edge-1", false},
		{"conflicting group", "office", "default:edge-1", false},
		{"legacy name", "", "default:default:edge-1", false},
		{"name wildcard", "office", "edge*", false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			c := DefaultClient()
			c.Group = tc.group
			c.Name = tc.name
			if err := c.Validate(); (err == nil) != tc.valid {
				t.Errorf("Validate group %q name %q = %v, want valid=%v", tc.group, tc.name, err, tc.valid)
			}
		})
	}
}

func TestLoadClientIdentity(t *testing.T) {
	for _, tc := range []struct {
		label string
		yaml  string
		group string
		name  string
	}{
		{"defaults", "{}", DefaultClientGroup, ""},
		{"group only", "group: office\n", "office", ""},
		{"name only", "name: edge-1\n", DefaultClientGroup, "edge-1"},
		{"explicit identity", "group: office\nname: edge-1\n", "office", "edge-1"},
		{"empty group", "group: ''\nname: edge-1\n", "", "edge-1"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			cfg := DefaultClient()
			if err := Load(writeYAML(t, tc.yaml), &cfg); err != nil {
				t.Fatal(err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			if cfg.Group != tc.group || cfg.Name != tc.name {
				t.Fatalf("loaded group=%q name=%q, want group=%q name=%q", cfg.Group, cfg.Name, tc.group, tc.name)
			}
		})
	}
}
