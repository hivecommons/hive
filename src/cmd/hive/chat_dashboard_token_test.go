package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The chat services must present the same bearer token the dashboard
// middleware accepts, whichever of the three sources resolved it; hosted
// spokes mount a token file and set no HIVE_DASHBOARD_TOKEN (issue: every
// dashboard-chat `!runs` call answered 401).
func TestChatDashboardToken_PrefersResolvedConfigToken(t *testing.T) {
	t.Setenv("HIVE_DASHBOARD_TOKEN", "env-token")
	b := &boot{cfg: &config.Config{}}
	b.cfg.Dashboard.AuthToken = "file-token"
	if got := b.chatDashboardToken(); got != "file-token" {
		t.Fatalf("want resolved config token, got %q", got)
	}
	b.cfg.Dashboard.AuthToken = ""
	if got := b.chatDashboardToken(); got != "env-token" {
		t.Fatalf("want env fallback, got %q", got)
	}
	var nilBoot *boot
	if got := nilBoot.chatDashboardToken(); got != "env-token" {
		t.Fatalf("nil boot must fall back to env, got %q", got)
	}
}
