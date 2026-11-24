package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// A hive booted without usable GitHub credentials has nothing to enumerate.
// The rescan must say that plainly: reporting a "successful" scan of zero
// issues and zero PRs would repaint the cards empty and read as "your repos
// went quiet" rather than "this hive cannot see GitHub".
func TestRescanRepos_NoCredentials(t *testing.T) {
	var last atomic.Pointer[github.ActionableResult]
	var refreshed bool

	got, err := rescanRepos(context.Background(), &config.Config{}, nil, &last, func() { refreshed = true }, slog.Default())

	if !errors.Is(err, errNoForgeCredentials) {
		t.Fatalf("err = %v, want errNoForgeCredentials", err)
	}
	if got != nil {
		t.Errorf("result = %+v, want nil", got)
	}
	if refreshed {
		t.Error("published a dashboard refresh for a scan that never happened")
	}
	if last.Load() != nil {
		t.Error("overwrote the cached actionable result with nothing")
	}
}

// The boot-time reader and both writers of the cached enumeration must agree
// on one path, or a restart repaints from a file nothing is writing.
func TestLastActionablePathIsTheDataPVCFile(t *testing.T) {
	if lastActionablePath != "/data/last-actionable.json" {
		t.Fatalf("lastActionablePath = %q; the /data PVC restore path changed", lastActionablePath)
	}
}

// A manual rescan must publish the structured breakdown from the very same
// enumeration it uses for raw totals. Otherwise the button can refresh the
// headline number while leaving its explanation stale.
func TestRescanRepos_PublishesWorkBreakdown(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/repo/issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"number":1,"title":"Dependency Dashboard","user":{"login":"renovate[bot]"},"created_at":"2026-09-08T12:00:00Z"}]`))
	})
	mux.HandleFunc("/repos/testorg/repo/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := github.NewClientForTest(server.URL, "testorg", []string{"repo"}, slog.Default())
	cfg := &config.Config{Project: config.ProjectConfig{Org: "testorg", Repos: []string{"repo"}}}
	var last atomic.Pointer[github.ActionableResult]
	refreshed := false

	got, err := rescanRepos(context.Background(), cfg, client, &last, func() { refreshed = true }, slog.Default())
	if err != nil {
		t.Fatalf("rescanRepos: %v", err)
	}
	if !refreshed {
		t.Fatal("manual rescan did not republish dashboard status")
	}
	if got.TotalByRepo["repo"].Issues != 1 {
		t.Fatalf("raw issue total = %d, want 1", got.TotalByRepo["repo"].Issues)
	}
	if got.WorkBreakdownByRepo["repo"].Issues.DependencyDashboard != 1 {
		t.Fatalf("breakdown = %+v, want one dependency dashboard", got.WorkBreakdownByRepo["repo"].Issues)
	}
	if last.Load() != got {
		t.Fatal("manual rescan did not store the classified result used for the dashboard refresh")
	}
}

// A hive whose repos are all on another forge has no GitHub client, and the
// manual rescan must still enumerate them through their forge instead of
// reporting missing credentials (#19494).
func TestRescanRepos_ForgeOnlyHiveNeedsNoGitHubClient(t *testing.T) {
	t.Setenv(forgejoTestTokenEnv, forgejoTestToken)
	fake := &fakeForgejo{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	cfg := forgejoOnlyConfig(srv.URL)

	var last atomic.Pointer[github.ActionableResult]
	got, err := rescanRepos(context.Background(), cfg, nil, &last, nil, slog.Default())
	if err != nil {
		t.Fatalf("rescan of a forge-only hive: %v", err)
	}
	if got == nil || got.Issues.Count != 1 {
		t.Fatalf("rescan result = %+v, want the one actionable forge issue", got)
	}
	if last.Load() == nil {
		t.Error("rescan did not publish its snapshot")
	}
}
