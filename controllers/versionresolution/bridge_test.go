package versionresolution

import "testing"

func TestBridgeQueriesRealAuthorizedChannel(t *testing.T) {
	for _, tc := range []struct{ current, fleet, want string }{
		{"4.22.21", "5.0", "5.0.0"},
		{"4.21.9", "5.1", "4.22.0"},
		{"4.23.0", "5.1", "5.1.0"},
		{"5.0.0", "5.1", "5.1.0"},
		{"4.22.21", "4.23", "4.23.0"},
	} {
		got, err := nextChannelVersion(tc.current, tc.fleet)
		if err != nil || got != tc.want {
			t.Fatalf("%s / %s: got %q %v want %q", tc.current, tc.fleet, got, err, tc.want)
		}
	}
}
