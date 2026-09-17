package main

// Tests for the cmd/hive half of the #7360 review-thread reconciler:
// installReviewBots and writeReviewThreads (main.go). Both were merged at
// 0% coverage while the pkg/github engine they wire (SetReviewBots /
// CollectReviewThreads / WriteReviewThreadsReport) is fully tested. These
// tests cover the glue contracts only: nil-safety, hive.yaml-vs-project-file
// precedence and the unreadable-project-file warn path, the refresh
// throttle, the disabled-still-writes-empty-file contract, enrichment
// (escalated stamping) on the enabled path, and the write-failure warn.
//
// github.ReviewThreadsPath and the throttle vars are redirected/reset per
// test so the tests are hermetic on hosts with a live /var/run/hive-metrics.
// The one live path writeReviewThreads reads is the audit log (Agent
// attribution, default /data/audit.jsonl): that read is tolerant of a
// missing file and is only reached on the enabled path; no assertion here
// depends on its contents.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
)

// redirectReviewThreadsPath points github.ReviewThreadsPath into a temp dir
// and restores it on cleanup. Returns the redirected file path.
func redirectReviewThreadsPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "review-threads.json")
	old := github.ReviewThreadsPath
	github.ReviewThreadsPath = path
	t.Cleanup(func() { github.ReviewThreadsPath = old })
	return path
}

// resetReviewThreadsThrottle zeroes the package-level refresh stamp and
// restores both throttle vars on cleanup, so tests neither see nor leave
// state from other tests (or a prior run of the eval tick).
func resetReviewThreadsThrottle(t *testing.T) {
	t.Helper()
	oldStamp := reviewThreadsLastRefresh
	oldInterval := reviewThreadsRefreshInterval
	reviewThreadsLastRefresh = time.Time{}
	t.Cleanup(func() {
		reviewThreadsLastRefresh = oldStamp
		reviewThreadsRefreshInterval = oldInterval
	})
}

// captureLogger returns a logger writing to the returned buffer.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// reviewBotsEnabledOn reports whether client has review bots installed, via
// the same observable the production code uses: a disabled client returns
// Enabled=false from CollectReviewThreads (no network traffic either way —
// the PR list is empty).
func reviewBotsEnabledOn(client *github.Client) bool {
	return client.CollectReviewThreads(context.Background(), nil, time.Now()).Enabled
}

func TestInstallReviewBots_NilSafe(t *testing.T) {
	logger, _ := captureLogger()
	cfg := &config.Config{}
	// None of these may panic; nil client and nil cfg are both documented
	// production states (a hive without GitHub credentials runs with a nil
	// client for the life of the process).
	installReviewBots(nil, cfg, logger)
	installReviewBots(nil, nil, nil)
	client := github.NewClient("fake", "o", nil, logger, "")
	installReviewBots(client, nil, logger)
	if reviewBotsEnabledOn(client) {
		t.Error("nil cfg must not enable review bots")
	}
}

func TestInstallReviewBots_HiveYAMLBlockWins(t *testing.T) {
	// A project file naming a different bot exists, but hive.yaml's block
	// names a login, so it must win.
	project := filepath.Join(t.TempDir(), "hive-project.yaml")
	if err := os.WriteFile(project, []byte("classification:\n  review_bots:\n    logins: [\"other-bot\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_PROJECT_YAML", project)

	logger, buf := captureLogger()
	client := github.NewClient("fake", "o", nil, logger, "")
	cfg := &config.Config{Classification: config.ClassificationConfig{
		ReviewBots: config.ReviewBotsConfig{Logins: []string{"Copilot"}},
	}}
	installReviewBots(client, cfg, logger)
	if !reviewBotsEnabledOn(client) {
		t.Fatal("hive.yaml review_bots block must enable the reconciler")
	}
	if !bytes.Contains(buf.Bytes(), []byte("review-thread reconciler enabled")) ||
		!bytes.Contains(buf.Bytes(), []byte("Copilot")) {
		t.Errorf("expected enabled log naming Copilot, got:\n%s", buf.String())
	}
	if bytes.Contains(buf.Bytes(), []byte("other-bot")) {
		t.Errorf("project file must lose to hive.yaml, got:\n%s", buf.String())
	}
}

func TestInstallReviewBots_ProjectFileFallback(t *testing.T) {
	project := filepath.Join(t.TempDir(), "hive-project.yaml")
	if err := os.WriteFile(project, []byte("classification:\n  review_bots:\n    logins: [\"chatgpt-codex-connector[bot]\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_PROJECT_YAML", project)

	logger, buf := captureLogger()
	client := github.NewClient("fake", "o", nil, logger, "")
	// hive.yaml block absent → the same key is read from HIVE_PROJECT_YAML.
	installReviewBots(client, &config.Config{}, logger)
	if !reviewBotsEnabledOn(client) {
		t.Fatal("project-file review_bots block must enable the reconciler")
	}
	if !bytes.Contains(buf.Bytes(), []byte("review-thread reconciler enabled")) {
		t.Errorf("expected enabled log, got:\n%s", buf.String())
	}
}

func TestInstallReviewBots_UnparseableProjectFileWarnsAndStaysOff(t *testing.T) {
	project := filepath.Join(t.TempDir(), "hive-project.yaml")
	if err := os.WriteFile(project, []byte(":\tnot yaml ["), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_PROJECT_YAML", project)

	logger, buf := captureLogger()
	client := github.NewClient("fake", "o", nil, logger, "")
	installReviewBots(client, &config.Config{}, logger)
	if reviewBotsEnabledOn(client) {
		t.Error("a broken project file must leave the feature off, not half-configured")
	}
	if !bytes.Contains(buf.Bytes(), []byte("project file unreadable")) {
		t.Errorf("expected unreadable-project-file warning, got:\n%s", buf.String())
	}
}

func TestWriteReviewThreads_NilArgsWriteNothing(t *testing.T) {
	path := redirectReviewThreadsPath(t)
	resetReviewThreadsThrottle(t)
	logger, _ := captureLogger()
	client := github.NewClient("fake", "o", nil, logger, "")

	writeReviewThreads(context.Background(), nil, &github.ActionableResult{}, "o", nil, logger)
	writeReviewThreads(context.Background(), client, nil, "o", nil, logger)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("nil client/actionable must not write the report (stat err: %v)", err)
	}
	if !reviewThreadsLastRefresh.IsZero() {
		t.Error("nil args must not consume the refresh budget")
	}
}

func TestWriteReviewThreads_ThrottleSkipsPass(t *testing.T) {
	path := redirectReviewThreadsPath(t)
	resetReviewThreadsThrottle(t)
	stamp := time.Now().Add(-time.Minute) // within the 5-minute interval
	reviewThreadsLastRefresh = stamp

	logger, _ := captureLogger()
	client := github.NewClient("fake", "o", nil, logger, "")
	writeReviewThreads(context.Background(), client, &github.ActionableResult{}, "o", nil, logger)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a pass inside the refresh interval must write nothing (stat err: %v)", err)
	}
	if !reviewThreadsLastRefresh.Equal(stamp) {
		t.Error("a throttled pass must not move the refresh stamp")
	}
}

func TestWriteReviewThreads_DisabledStillWritesEmptyFile(t *testing.T) {
	path := redirectReviewThreadsPath(t)
	resetReviewThreadsThrottle(t)
	logger, buf := captureLogger()
	client := github.NewClient("fake", "o", nil, logger, "") // no review bots installed

	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "o/r", Number: 1, Author: "hive[bot]"},
	}}}
	writeReviewThreads(context.Background(), client, actionable, "o", nil, logger)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("feature-off must still write the (empty) report so a reader can tell \"off\" from \"never ran\": %v", err)
	}
	var report github.ReviewThreadsReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("report not valid JSON: %v\n%s", err, data)
	}
	if report.Enabled || report.TotalThreads != 0 || report.PRs == nil || len(report.PRs) != 0 {
		t.Errorf("disabled report wrong: %+v", report)
	}
	if bytes.Contains(buf.Bytes(), []byte("review-threads.json refreshed")) {
		t.Errorf("feature-off pass must not log a refresh, got:\n%s", buf.String())
	}
	if reviewThreadsLastRefresh.IsZero() {
		t.Error("a completed pass must stamp the refresh throttle")
	}

	// The stamp it just set must throttle an immediately following pass.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeReviewThreads(context.Background(), client, actionable, "o", nil, logger)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("second pass within the interval must be throttled (stat err: %v)", err)
	}
}

func TestWriteReviewThreads_EnabledCollectsStampsAndWrites(t *testing.T) {
	path := redirectReviewThreadsPath(t)
	resetReviewThreadsThrottle(t)

	// One-PR GraphQL fixture: a single unresolved Copilot thread on o/r#1.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"data":{"repository":{"pullRequest":{
			"headRefName":"hive/fix-1",
			"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
				{"id":"PRRT_1","isResolved":false,"isOutdated":false,"path":"a.go","line":3,
				 "comments":{"nodes":[{"author":{"login":"Copilot"},"body":"nil deref","databaseId":10}]}}
			]}}}}}`)); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()

	logger, buf := captureLogger()
	client := github.NewClientForTest(srv.URL, "o", nil, logger)
	client.SetAppBotLogin("hive[bot]")
	client.SetReviewBots(config.ReviewBotsConfig{Logins: []string{"Copilot"}})

	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "o/r", Number: 1, Title: "fix one", Author: "hive[bot]"},
	}}}
	escalated := map[string]bool{escalation.Key("o/r", 1): true}
	writeReviewThreads(context.Background(), client, actionable, "o", escalated, logger)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("enabled pass must write the report: %v", err)
	}
	var report github.ReviewThreadsReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("report not valid JSON: %v\n%s", err, data)
	}
	if !report.Enabled || report.TotalThreads != 1 || len(report.PRs) != 1 {
		t.Fatalf("expected 1 PR / 1 thread, got: %+v", report)
	}
	pr := report.PRs[0]
	if pr.Repo != "o/r" || pr.Number != 1 || pr.HeadRef != "hive/fix-1" || len(pr.Threads) != 1 {
		t.Errorf("PR wrong: %+v", pr)
	}
	if !pr.Escalated {
		t.Error("escalation verdict must be stamped onto the written PR entry")
	}
	if pr.Threads[0].ThreadID != "PRRT_1" || pr.Threads[0].Author != "Copilot" {
		t.Errorf("thread wrong: %+v", pr.Threads[0])
	}
	if !bytes.Contains(buf.Bytes(), []byte("review-threads.json refreshed")) {
		t.Errorf("expected refresh log, got:\n%s", buf.String())
	}
	if reviewThreadsLastRefresh.IsZero() {
		t.Error("a completed pass must stamp the refresh throttle")
	}
}

func TestWriteReviewThreads_WriteFailureWarns(t *testing.T) {
	resetReviewThreadsThrottle(t)
	// Point the report path UNDER an existing regular file so MkdirAll fails.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := github.ReviewThreadsPath
	github.ReviewThreadsPath = filepath.Join(blocker, "review-threads.json")
	t.Cleanup(func() { github.ReviewThreadsPath = old })

	logger, buf := captureLogger()
	client := github.NewClient("fake", "o", nil, logger, "")
	writeReviewThreads(context.Background(), client, &github.ActionableResult{}, "o", nil, logger)
	if !bytes.Contains(buf.Bytes(), []byte("failed to write review-threads.json")) {
		t.Errorf("expected write-failure warning, got:\n%s", buf.String())
	}
	if bytes.Contains(buf.Bytes(), []byte("review-threads.json refreshed")) {
		t.Errorf("a failed write must not log a refresh, got:\n%s", buf.String())
	}
}
