package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/taskmcp"
)

// #10282: the task MCP URL handed to hub-launched agents must name a host this
// spoke is actually served on. A hub-disabled spoke still carries a default
// hub.url, so synthesising "<hiveID>.<hub host>" sent agents to a host with a
// mismatched TLS cert (or to the hosted hub, which refuses the launch token).
func TestResolveSpokeDashboardURL_10282(t *testing.T) {
	noHost := func() string { return "" }
	tests := []struct {
		name       string
		mutate     func(*config.Config)
		servedHost func() string
		want       string
	}{
		{
			name: "hub disabled falls back to localhost despite a default hub.url",
			mutate: func(c *config.Config) {
				c.Hub.Enabled = false
				c.Hub.URL = "https://hive.kubestellar.io"
			},
			servedHost: noHost,
			want:       "http://localhost:3002",
		},
		{
			name: "hub disabled ignores a stale hub-owned dashboard_url",
			mutate: func(c *config.Config) {
				c.Hub.Enabled = false
				c.Hub.DashboardURL = "https://hive-wild-mole.hive.kubestellar.io"
			},
			servedHost: noHost,
			want:       "http://localhost:3002",
		},
		{
			name: "dashboard.public_url wins over hub synthesis",
			mutate: func(c *config.Config) {
				c.Dashboard.PublicURL = "https://hive.example.org"
			},
			servedHost: func() string { return "served.apps.example.org" },
			want:       "https://hive.example.org",
		},
		{
			name: "dashboard.public_url used on a hub-disabled spoke",
			mutate: func(c *config.Config) {
				c.Hub.Enabled = false
				c.Dashboard.PublicURL = "https://hive.example.org"
			},
			servedHost: noHost,
			want:       "https://hive.example.org",
		},
		{
			name:       "live served host wins over hub synthesis",
			mutate:     func(c *config.Config) {},
			servedHost: func() string { return "hive.apps.cluster.example" },
			want:       "https://hive.apps.cluster.example",
		},
		{
			name:       "served under hub wildcard synthesises hive-id host",
			mutate:     func(c *config.Config) {},
			servedHost: noHost,
			want:       "https://hive-wild-mole.hive.hivecommons.dev",
		},
		{
			name: "hub-owned dashboard_url wins when hub enabled",
			mutate: func(c *config.Config) {
				c.Hub.DashboardURL = "https://wild-mole.hive.hivecommons.dev"
			},
			servedHost: func() string { return "served.example" },
			want:       "https://wild-mole.hive.hivecommons.dev",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.HiveID = "hive-wild-mole"
			cfg.Hub.Enabled = true
			cfg.Hub.URL = "https://hive.hivecommons.dev"
			cfg.Dashboard.Port = 3002
			tt.mutate(cfg)
			if got := resolveSpokeDashboardURL(cfg, tt.servedHost); got != tt.want {
				t.Fatalf("resolveSpokeDashboardURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTaskMCPURLForAgentsHubDisabledUsesLocalhost_10282(t *testing.T) {
	cfg := &config.Config{}
	cfg.HiveID = "hive-wild-mole"
	cfg.Hub.Enabled = false
	cfg.Hub.URL = "https://hive.kubestellar.io"
	cfg.Dashboard.Port = 3002
	b := &boot{cfg: cfg}
	b.dashboardURLForFreshHeartbeat = func() string {
		return resolveSpokeDashboardURL(b.cfg, func() string { return "" })
	}
	want := "http://localhost:3002" + taskmcp.EndpointPath
	if got := b.taskMCPURLForAgents(); got != want {
		t.Fatalf("taskMCPURLForAgents = %q, want %q", got, want)
	}
}
