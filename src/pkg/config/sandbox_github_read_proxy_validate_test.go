package config

import (
	"strings"
	"testing"
)

// Config validation (the load and save paths) enforces the read relay's
// contract: default off, no default bind, https endpoint only (#18865).
func TestConfigValidateSandboxGitHubReadProxy(t *testing.T) {
	withProxy := func(rp *SandboxGitHubReadProxy) *Config {
		c := &Config{}
		c.AgentSandbox = AgentSandboxConfig{Job: &SandboxJobConfig{GitHubReadProxy: rp}}
		return c
	}
	for _, tc := range []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{"no block", &Config{}, ""},
		{"disabled", withProxy(&SandboxGitHubReadProxy{Endpoint: "http://h:1"}), ""},
		{"enabled and complete", withProxy(&SandboxGitHubReadProxy{Enabled: true, ListenAddr: ":8443", Endpoint: "https://hive.ns.svc:8443"}), ""},
		{"enabled without listen_addr", withProxy(&SandboxGitHubReadProxy{Enabled: true, Endpoint: "https://hive.ns.svc:8443"}), "github_read_proxy.listen_addr"},
		{"enabled with http endpoint", withProxy(&SandboxGitHubReadProxy{Enabled: true, ListenAddr: ":8443", Endpoint: "http://hive.ns.svc:8443"}), "https"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validateJobAffinities()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}
