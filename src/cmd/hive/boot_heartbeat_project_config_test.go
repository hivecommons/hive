package main

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

// These tests drive the spoke.ProjectConfigCallback that bootHeartbeatWith
// registers — the ONLY channel by which a heartbeat-only spoke receives its
// project claim (org/repos/primary_repo/ACMM level), the vanity dashboard
// URL, the AI author, the issue filter, and a GHE API URL. The callback is
// captured through bootHeartbeatFake and invoked directly, so the
// reconciliation logic runs without a hub or a ticker.

func projectConfigCallback(t *testing.T, f *bootHeartbeatFake) spoke.ProjectConfigCallback {
	t.Helper()
	return heartbeatCallback[spoke.ProjectConfigCallback](t, f)
}

func TestBootHeartbeatProjectConfig_NilPushIsANoOp(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	b := newBootHeartbeatBoot(t, f, cfg)
	b.bootHeartbeatWith(f.deps)

	before := cfg.Project
	projectConfigCallback(t, f)(nil)

	if cfg.Project.Org != before.Org || !sameStringSlice(cfg.Project.Repos, before.Repos) {
		t.Fatalf("nil push mutated the project: %+v", cfg.Project)
	}
}

func TestBootHeartbeatProjectConfig_URLOnlyPushAdoptsVanityURLWithoutTouchingProject(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	b := newBootHeartbeatBoot(t, f, cfg)
	b.bootHeartbeatWith(f.deps)

	projectConfigCallback(t, f)(&spoke.HeartbeatProjectConfig{
		DashboardURL: "https://hive.example.org",
	})

	if cfg.Hub.DashboardURL != "https://hive.example.org" {
		t.Fatalf("vanity URL not adopted from url-only push: %q", cfg.Hub.DashboardURL)
	}
	if cfg.Project.Org != "acme" || !sameStringSlice(cfg.Project.Repos, []string{"widgets", "gadgets"}) {
		t.Fatalf("url-only push (empty org) must not touch the project: %+v", cfg.Project)
	}
	if !strings.Contains(f.log.String(), "url-only push") {
		t.Fatalf("adoption not logged:\n%s", f.log.String())
	}

	// The hub echoes the same URL on every beat: an unchanged URL must not be
	// re-logged as an adoption.
	f.log.Reset()
	projectConfigCallback(t, f)(&spoke.HeartbeatProjectConfig{
		DashboardURL: "https://hive.example.org",
	})
	if strings.Contains(f.log.String(), "adopting vanity dashboard URL") {
		t.Fatalf("unchanged vanity URL logged as an adoption:\n%s", f.log.String())
	}
}

func TestBootHeartbeatProjectConfig_RefusesMisconfiguredRepoTarget(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	b := newBootHeartbeatBoot(t, f, cfg)
	b.bootHeartbeatWith(f.deps)

	// An org spelled as org/repo is exactly the misconfiguration
	// ValidateProjectRepoTargets exists to catch; the push must be refused
	// wholesale, leaving the working project untouched.
	projectConfigCallback(t, f)(&spoke.HeartbeatProjectConfig{
		Org:         "acme/widgets",
		Repos:       []string{"widgets"},
		PrimaryRepo: "widgets",
		ACMMLevel:   3,
	})

	if cfg.Project.Org != "acme" || cfg.ACMMLevel != nil {
		t.Fatalf("misconfigured push was applied: org=%q acmm=%v", cfg.Project.Org, cfg.ACMMLevel)
	}
	if !strings.Contains(f.log.String(), "REFUSING hub project config") {
		t.Fatalf("refusal not logged:\n%s", f.log.String())
	}
}

func TestBootHeartbeatProjectConfig_AlreadyReconciledIsANoOp(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	lvl := 2
	cfg.ACMMLevel = &lvl
	b := newBootHeartbeatBoot(t, f, cfg)
	b.bootHeartbeatWith(f.deps)

	f.log.Reset()
	projectConfigCallback(t, f)(&spoke.HeartbeatProjectConfig{
		Org:         "acme",
		Repos:       []string{"widgets", "gadgets"},
		PrimaryRepo: "widgets",
		ACMMLevel:   2,
	})

	if strings.Contains(f.log.String(), "project config updated") {
		t.Fatalf("matching push (the hub's every-beat echo) logged as an update:\n%s", f.log.String())
	}
}

func TestBootHeartbeatProjectConfig_ClaimAdoptsProjectAndPreservesLocalFields(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	cfg.Project.AIAuthor = "local-author"
	cfg.Project.IssueFilter = config.IssueFilterConfig{RequireLabels: []string{"approved"}}
	b := newBootHeartbeatBoot(t, f, cfg)
	b.bootHeartbeatWith(f.deps)

	projectConfigCallback(t, f)(&spoke.HeartbeatProjectConfig{
		Org:          "claimed",
		Repos:        []string{"rockets"},
		PrimaryRepo:  "rockets",
		ACMMLevel:    3,
		DashboardURL: "https://claimed.example.org",
		// Empty author and nil filter mean "the hub is not speaking to these
		// fields": both incidents (fleet-wide ai_author reset disabling the
		// stats collector; every-beat echo blanking a local filter) are the
		// behavior these assertions pin down.
	})

	if cfg.Project.Org != "claimed" || !sameStringSlice(cfg.Project.Repos, []string{"rockets"}) || cfg.Project.PrimaryRepo != "rockets" {
		t.Fatalf("claim not adopted: %+v", cfg.Project)
	}
	if cfg.ACMMLevel == nil || *cfg.ACMMLevel != 3 {
		t.Fatalf("ACMM level not adopted: %v", cfg.ACMMLevel)
	}
	if cfg.Hub.DashboardURL != "https://claimed.example.org" {
		t.Fatalf("vanity URL not adopted on claim: %q", cfg.Hub.DashboardURL)
	}
	if cfg.Project.AIAuthor != "local-author" {
		t.Fatalf("empty pushed author blanked the local ai_author: %q", cfg.Project.AIAuthor)
	}
	if !cfg.Project.IssueFilter.Equal(config.IssueFilterConfig{RequireLabels: []string{"approved"}}) {
		t.Fatalf("nil pushed filter blanked the local issue filter: %+v", cfg.Project.IssueFilter)
	}
	if !strings.Contains(f.log.String(), "project config updated from hub heartbeat") {
		t.Fatalf("claim not logged:\n%s", f.log.String())
	}
}

func TestBootHeartbeatProjectConfig_AdoptsAuthorAndFilterWhenSent(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	cfg.Project.AIAuthor = "local-author"
	cfg.Project.IssueFilter = config.IssueFilterConfig{RequireLabels: []string{"approved"}}
	b := newBootHeartbeatBoot(t, f, cfg)
	b.bootHeartbeatWith(f.deps)

	projectConfigCallback(t, f)(&spoke.HeartbeatProjectConfig{
		Org:         "claimed",
		Repos:       []string{"rockets"},
		PrimaryRepo: "rockets",
		ACMMLevel:   1,
		AIAuthor:    "hub-author",
		IssueFilter: &config.IssueFilterConfig{RequireLabels: []string{"ok-to-work"}},
	})

	if cfg.Project.AIAuthor != "hub-author" {
		t.Fatalf("pushed author not adopted: %q", cfg.Project.AIAuthor)
	}
	if !cfg.Project.IssueFilter.Equal(config.IssueFilterConfig{RequireLabels: []string{"ok-to-work"}}) {
		t.Fatalf("pushed filter not adopted: %+v", cfg.Project.IssueFilter)
	}

	// A non-nil but EMPTY filter is an explicit clear, unlike nil.
	projectConfigCallback(t, f)(&spoke.HeartbeatProjectConfig{
		Org:         "claimed",
		Repos:       []string{"rockets"},
		PrimaryRepo: "rockets",
		ACMMLevel:   2, // change something so the already-reconciled early return doesn't skip the clear
		IssueFilter: &config.IssueFilterConfig{},
	})
	if !cfg.Project.IssueFilter.IsZero() {
		t.Fatalf("explicit empty filter did not clear the local one: %+v", cfg.Project.IssueFilter)
	}
}

func TestBootHeartbeatProjectConfig_RefusesAPIURLNamingTheWrongForge(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	cfg.GitHub.AppID = config.PublicGitHubAppID // App registered on github.com
	b := newBootHeartbeatBoot(t, f, cfg)
	b.bootHeartbeatWith(f.deps)

	projectConfigCallback(t, f)(&spoke.HeartbeatProjectConfig{
		Org:          "claimed",
		Repos:        []string{"rockets"},
		PrimaryRepo:  "rockets",
		ACMMLevel:    3,
		GitHubAPIURL: "https://ghe.example.com/api/v3", // resolves to a different forge than app_id
	})

	if cfg.GitHub.APIURL != "" {
		t.Fatalf("half-applied identity: mismatched api_url adopted: %q", cfg.GitHub.APIURL)
	}
	if !strings.Contains(f.log.String(), "REFUSING hub GitHub API URL") {
		t.Fatalf("api_url refusal not logged:\n%s", f.log.String())
	}
	// The guard skips ONLY the api_url field — the claim around it must land.
	if cfg.Project.Org != "claimed" || cfg.ACMMLevel == nil || *cfg.ACMMLevel != 3 {
		t.Fatalf("api_url refusal blocked the unrelated project adoption: %+v acmm=%v", cfg.Project, cfg.ACMMLevel)
	}
}

func TestBootHeartbeatProjectConfig_AdoptsMatchingAPIURL(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	cfg.GitHub.AppID = config.PublicGitHubAppID
	b := newBootHeartbeatBoot(t, f, cfg)
	b.bootHeartbeatWith(f.deps)

	projectConfigCallback(t, f)(&spoke.HeartbeatProjectConfig{
		Org:          "claimed",
		Repos:        []string{"rockets"},
		PrimaryRepo:  "rockets",
		ACMMLevel:    3,
		GitHubAPIURL: "https://api.github.com", // same forge as the public App
	})

	if cfg.GitHub.APIURL != "https://api.github.com" {
		t.Fatalf("matching api_url not adopted: %q", cfg.GitHub.APIURL)
	}
	if !strings.Contains(f.log.String(), "adopting GitHub API URL from hub heartbeat") {
		t.Fatalf("api_url adoption not logged:\n%s", f.log.String())
	}
}
