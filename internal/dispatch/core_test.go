package dispatch

import "testing"

func TestParseDestination(t *testing.T) {
	for _, tc := range []struct {
		address   string
		want      Destination
		wantError bool
	}{
		{"127.0.0.1:9001", Destination{Host: "127.0.0.1", Port: 9001}, false},
		{" [::1]:9001\n", Destination{Host: "::1", Port: 9001}, false},
		{"example.com:443", Destination{Host: "example.com", Port: 443}, false},
		{":80", Destination{}, true},
		{"localhost:0", Destination{}, true},
		{"localhost:65536", Destination{}, true},
		{"localhost:no", Destination{}, true},
		{"localhost", Destination{}, true},
	} {
		t.Run(tc.address, func(t *testing.T) {
			got, err := ParseDestination(tc.address)
			if (err != nil) != tc.wantError || got != tc.want {
				t.Fatalf("ParseDestination(%q) = %v, %v; want %v, error=%v", tc.address, got, err, tc.want, tc.wantError)
			}
		})
	}
}
