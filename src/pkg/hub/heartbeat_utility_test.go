package hub

import (
	"testing"
)

func TestDashboardHost(t *testing.T) {
	tests := []struct {
		rawURL string
		want   string
	}{
		{"https://my-hive.example.com/dashboard", "my-hive.example.com"},
		{"https://MY-HIVE.EXAMPLE.COM:8443/path", "my-hive.example.com"},
		{"http://localhost:9090", "localhost"},
		{"", ""},
		{"://invalid", ""},
		{"   https://padded.io   ", "padded.io"},
	}
	for _, tt := range tests {
		got := dashboardHost(tt.rawURL)
		if got != tt.want {
			t.Errorf("dashboardHost(%q) = %q, want %q", tt.rawURL, got, tt.want)
		}
	}
}
