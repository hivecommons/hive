package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/upstreamwatch"
)

// writeUpstreamWatchState saves state through the package's own FileStore, so
// the fixture can never drift from the on-disk shape the watch writes.
func writeUpstreamWatchState(t *testing.T, state upstreamwatch.State) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upstream-watch.json")
	if err := upstreamwatch.NewFileStore(path).Save(state); err != nil {
		t.Fatalf("save state: %v", err)
	}
	return path
}

func decodeUpstreamWatch(t *testing.T, s *Server) UpstreamWatchStatus {
	t.Helper()
	rec := doOwnerGet(s, "/api/upstream-watch")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/upstream-watch = %d — %s", rec.Code, rec.Body.String())
	}
	var got UpstreamWatchStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	return got
}

func TestUpstreamWatchOwnerGated(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/upstream-watch") // no owner headers
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /api/upstream-watch without owner role = %d, want 403", rec.Code)
	}
}

func TestUpstreamWatchNotConfigured(t *testing.T) {
	s, _ := apiServer(t)
	SetUpstreamWatchStatePathForTest(t, filepath.Join(t.TempDir(), "missing.json"))
	got := decodeUpstreamWatch(t, s)
	if got.Enabled || got.Configured {
		t.Errorf("enabled=%v configured=%v, want both false", got.Enabled, got.Configured)
	}
	if len(got.Repos) != 0 {
		t.Errorf("repos = %d, want 0 when nothing is configured", len(got.Repos))
	}
	if got.StateError != "" {
		t.Errorf("state_error = %q, want empty: a missing state file is not an error", got.StateError)
	}
}

// A configured repo the watch has never run for must still appear, with zero
// counts: "configured but idle" and "not configured" are different answers.
func TestUpstreamWatchConfiguredButIdle(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.UpstreamWatch = config.UpstreamWatchConfig{
		Enabled: true,
		Repos:   map[string]config.UpstreamWatchRepo{"repo1": {Upstream: "up/stream"}},
	}
	SetUpstreamWatchStatePathForTest(t, filepath.Join(t.TempDir(), "missing.json"))
	got := decodeUpstreamWatch(t, s)
	if !got.Enabled || !got.Configured {
		t.Fatalf("enabled=%v configured=%v, want both true", got.Enabled, got.Configured)
	}
	if len(got.Repos) != 1 {
		t.Fatalf("repos = %d, want 1", len(got.Repos))
	}
	repo := got.Repos[0]
	if repo.Repo != "repo1" || repo.Upstream != "up/stream" {
		t.Errorf("repo = %+v, want repo1 following up/stream", repo)
	}
	if repo.Watermark != "" || repo.LastRunAt != "" || repo.Surfaced != 0 || len(repo.Recent) != 0 {
		t.Errorf("idle repo = %+v, want empty watermark/last-run and zero counts", repo)
	}
}

func TestUpstreamWatchDivergenceView(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.UpstreamWatch = config.UpstreamWatchConfig{
		Enabled: true,
		// Two repos, declared out of order: the response must be key-sorted.
		Repos: map[string]config.UpstreamWatchRepo{
			"other/fork": {},
			"repo1":      {},
		},
	}
	watermark := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	lastRun := time.Date(2026, 1, 2, 4, 0, 0, 0, time.UTC)
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	state := upstreamwatch.State{
		"repo1": {
			// Resolved at runtime from the fork parent, not configured.
			Upstream:  "up/stream",
			Watermark: watermark,
			LastRunAt: lastRun,
			Refs: map[string]*upstreamwatch.RefRecord{
				"upstream#12":  {Ref: "upstream#12", Status: upstreamwatch.StatusFiled, IssueNumber: 77, FiledAt: newer},
				"upstream#9":   {Ref: "upstream#9", Status: upstreamwatch.StatusPorted, IssueNumber: 70, FiledAt: older},
				"release:v2.0": {Ref: "release:v2.0", Status: upstreamwatch.StatusDismissed, IssueNumber: 60, FiledAt: older},
				"upstream#3":   {Ref: "upstream#3", Status: upstreamwatch.StatusSkipped, FiledAt: older},
			},
		},
	}
	SetUpstreamWatchStatePathForTest(t, writeUpstreamWatchState(t, state))

	got := decodeUpstreamWatch(t, s)
	if len(got.Repos) != 2 {
		t.Fatalf("repos = %d, want 2", len(got.Repos))
	}
	if got.Repos[0].Repo != "other/fork" || got.Repos[1].Repo != "repo1" {
		t.Fatalf("repos = %q/%q, want key order other/fork, repo1", got.Repos[0].Repo, got.Repos[1].Repo)
	}
	repo := got.Repos[1]
	if repo.Upstream != "up/stream" {
		t.Errorf("upstream = %q, want the resolved up/stream", repo.Upstream)
	}
	if repo.Watermark != watermark.Format(time.RFC3339) {
		t.Errorf("watermark = %q, want %q", repo.Watermark, watermark.Format(time.RFC3339))
	}
	if repo.LastRunAt != lastRun.Format(time.RFC3339) {
		t.Errorf("last_run_at = %q, want %q", repo.LastRunAt, lastRun.Format(time.RFC3339))
	}
	// Surfaced counts every ref that produced a fork issue; the rest count by
	// recorded status.
	if repo.Surfaced != 3 || repo.Ported != 1 || repo.Dismissed != 1 || repo.Skipped != 1 {
		t.Errorf("counts = surfaced %d ported %d dismissed %d skipped %d, want 3/1/1/1",
			repo.Surfaced, repo.Ported, repo.Dismissed, repo.Skipped)
	}
	if len(repo.Recent) != 4 {
		t.Fatalf("recent = %d, want 4", len(repo.Recent))
	}
	first := repo.Recent[0]
	if first.Ref != "upstream#12" {
		t.Errorf("recent[0] = %q, want the newest ref upstream#12", first.Ref)
	}
	if first.State != "surfaced" || first.Status != "filed" {
		t.Errorf("recent[0] state/status = %q/%q, want surfaced/filed", first.State, first.Status)
	}
	if first.UpstreamRef != "up/stream#12" {
		t.Errorf("upstream_ref = %q, want up/stream#12", first.UpstreamRef)
	}
	if first.UpstreamURL != "https://github.com/up/stream/pull/12" {
		t.Errorf("upstream_url = %q", first.UpstreamURL)
	}
	if first.DiffURL != "https://github.com/up/stream/pull/12.diff" {
		t.Errorf("diff_url = %q", first.DiffURL)
	}
	// repo1 is a bare key, qualified with project.org for the fork issue link.
	if first.IssueNumber != 77 || first.IssueURL != "https://github.com/myorg/repo1/issues/77" {
		t.Errorf("issue = %d %q, want 77 under myorg/repo1", first.IssueNumber, first.IssueURL)
	}
	byRef := map[string]UpstreamWatchItem{}
	for _, item := range repo.Recent {
		byRef[item.Ref] = item
	}
	if got, want := byRef["upstream#9"].State, "ported"; got != want {
		t.Errorf("upstream#9 state = %q, want %q", got, want)
	}
	if rel := byRef["release:v2.0"]; rel.State != "dismissed" || rel.UpstreamRef != "up/stream@v2.0" || rel.DiffURL != "" {
		t.Errorf("release ref = %+v, want dismissed, up/stream@v2.0 and no diff URL", rel)
	}
	if skipped := byRef["upstream#3"]; skipped.State != "skipped" || skipped.IssueURL != "" {
		t.Errorf("skipped ref = %+v, want skipped with no fork issue link", skipped)
	}
}

// A corrupt state file stops the watch; the view must say so rather than
// looking like a watch that has never run.
func TestUpstreamWatchCorruptStateReported(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.UpstreamWatch = config.UpstreamWatchConfig{
		Enabled: true,
		Repos:   map[string]config.UpstreamWatchRepo{"repo1": {}},
	}
	path := filepath.Join(t.TempDir(), "upstream-watch.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	SetUpstreamWatchStatePathForTest(t, path)
	got := decodeUpstreamWatch(t, s)
	if got.StateError == "" {
		t.Error("state_error is empty, want the parse failure surfaced")
	}
	if len(got.Repos) != 1 {
		t.Errorf("repos = %d, want the configured repo listed even with unreadable state", len(got.Repos))
	}
}
