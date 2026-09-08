package hub

import (
	"testing"
)

func TestDashboardHostSupplementalCases(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   string
	}{
		{name: "host without scheme is not a URL host", rawURL: "hive.example.com", want: ""},
		{name: "whitespace only is empty", rawURL: "   ", want: ""},
		{name: "strips user info and port", rawURL: "https://operator@HIVE.EXAMPLE.COM:9443/dashboard", want: "hive.example.com"},
		{name: "ipv6 literal host", rawURL: "http://[2001:db8::1]:8080/api", want: "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dashboardHost(tt.rawURL); got != tt.want {
				t.Fatalf("dashboardHost(%q) = %q, want %q", tt.rawURL, got, tt.want)
			}
		})
	}
}
