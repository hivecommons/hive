package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDashboardGitHubSignInEnabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		yml  string
		want bool
	}{
		{"unset keeps it on", "port: 8080\n", true},
		{"explicit true", "github_sign_in: true\n", true},
		{"explicit false", "github_sign_in: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d DashboardConfig
			if err := yaml.Unmarshal([]byte(tc.yml), &d); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := d.GitHubSignInEnabled(); got != tc.want {
				t.Fatalf("GitHubSignInEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfigHubSignInLink(t *testing.T) {
	for _, tc := range []struct {
		name, hubURL, signInURL, hiveID, want string
	}{
		{"hub.url fallback", "https://hub.example.com", "", "h1", "https://hub.example.com/api/saas/hives/h1/open"},
		{"browser URL wins", "http://hub.svc:8080", "https://hub.example.com/", "h1", "https://hub.example.com/api/saas/hives/h1/open"},
		{"hive id escaped", "https://hub.example.com", "", "a b/c", "https://hub.example.com/api/saas/hives/a%20b%2Fc/open"},
		{"no hive id", "https://hub.example.com", "", "", ""},
		{"no hub", "", "", "h1", ""},
		{"non-http scheme", "", "javascript:alert(1)", "h1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{HiveID: tc.hiveID}
			c.Hub.URL = tc.hubURL
			c.Dashboard.HubSignInURL = tc.signInURL
			if got := c.HubSignInLink(); got != tc.want {
				t.Fatalf("HubSignInLink() = %q, want %q", got, tc.want)
			}
		})
	}
	var nilCfg *Config
	if got := nilCfg.HubSignInLink(); got != "" {
		t.Fatalf("nil config: got %q, want empty", got)
	}
}
