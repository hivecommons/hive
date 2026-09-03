package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// verifyDepsFor returns Dependencies whose GHClient answers any pulls lookup with
// a PR in owner/repo authored by author, so #2565 server-side PR verification
// passes. The PR number is echoed from the request path so distinct rounds each
// resolve to their own PR. Shared by the WS integration/promotion tests.
func verifyDepsFor(t *testing.T, owner, repo, author string) *Dependencies {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	full := owner + "/" + repo
	prefix := "/repos/" + full + "/pulls/"
	mux := http.NewServeMux()
	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		num := strings.TrimPrefix(r.URL.Path, prefix)
		n, _ := strconv.Atoi(num)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number":   n,
			"html_url": "https://github.com/" + full + "/pull/" + num,
			"user":     map[string]any{"login": author},
			"base": map[string]any{
				"repo": map[string]any{
					"name":      repo,
					"full_name": full,
					"owner":     map[string]any{"login": owner},
				},
			},
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &Dependencies{
		GHClient: ghpkg.NewClientForTest(ts.URL, owner, []string{repo}, logger),
		Logger:   logger,
		Ctx:      context.Background(),
	}
}

// prVerifyHub builds a hub whose GHClient points at a mux serving a single PR
// (base repo "myorg/repo1", author authorLogin, number prNum). status controls
// the HTTP response so callers can exercise the API-error path.
func prVerifyHub(t *testing.T, status int, authorLogin string, prNum int) *ContributeWSHub {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			http.Error(w, `{"message":"boom"}`, status)
			return
		}
		body := map[string]any{
			"number":   prNum,
			"html_url": "https://github.com/myorg/repo1/pull/" + strconv.Itoa(prNum),
			"user":     map[string]any{"login": authorLogin},
			"base": map[string]any{
				"repo": map[string]any{
					"name":      "repo1",
					"full_name": "myorg/repo1",
					"owner":     map[string]any{"login": "myorg"},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, logger)
	s.RegisterAPI(deps)
	return NewContributeWSHub(logger, s)
}

const prVerifyURL = "https://github.com/myorg/repo1/pull/7"

// TestVerifyReportedPR_HubHappyPath: all three checks pass → verified, and a
// verified completion earns the FULL (168h) cooldown, mirroring what the
// task_complete handler does with the verified URL.
func TestVerifyReportedPR_HubHappyPath(t *testing.T) {
	hub := prVerifyHub(t, http.StatusOK, "alice", 7)

	if !hub.verifyReportedPR("repo1", prVerifyURL, "alice") {
		t.Fatalf("expected verification to pass for matching repo+author")
	}

	// The handler feeds the verified URL to markTaskCompleted → full cooldown.
	hub.markTaskCompleted("myorg/repo1", 7, prVerifyURL)
	hub.completedMu.Lock()
	hub.completedTasks["myorg/repo1#7"] = time.Now().Add(-(completedNoPRCooldownHours + 1) * time.Hour)
	hub.completedMu.Unlock()
	if !hub.isTaskInCooldown("myorg/repo1", 7) {
		t.Fatalf("verified PR completion should hold the full %dh cooldown, not the short %dh one",
			completedTaskCooldownHours, completedNoPRCooldownHours)
	}
}

// TestVerifyReportedPR_HubWrongAuthor: the reported PR exists in the right repo
// but was authored by someone else → NOT verified → the handler would pass ""
// to markTaskCompleted → SHORT cooldown, no trust credit.
func TestVerifyReportedPR_HubWrongAuthor(t *testing.T) {
	hub := prVerifyHub(t, http.StatusOK, "mallory", 7)

	if hub.verifyReportedPR("repo1", prVerifyURL, "alice") {
		t.Fatalf("expected verification to FAIL for wrong author")
	}
	assertShortCooldown(t, hub, "myorg/repo1", 7)
}

// TestVerifyReportedPR_HubWrongRepo: the reported URL points at a DIFFERENT repo
// than the assignment (the relay's "first PR URL anywhere" fallback) → NOT
// verified even though the PR exists and the author matches.
func TestVerifyReportedPR_HubWrongRepo(t *testing.T) {
	hub := prVerifyHub(t, http.StatusOK, "alice", 7)

	// Assignment repo is "other", but the reported/served PR belongs to repo1.
	if hub.verifyReportedPR("other", prVerifyURL, "alice") {
		t.Fatalf("expected verification to FAIL when PR base repo != assigned repo")
	}
}

// TestVerifyReportedPR_HubGHError: a transient GitHub API failure must degrade
// safely — no crash, no trust — and the completion falls back to the short
// cooldown so the contributor is not stranded.
func TestVerifyReportedPR_HubGHError(t *testing.T) {
	hub := prVerifyHub(t, http.StatusInternalServerError, "alice", 7)

	if hub.verifyReportedPR("repo1", prVerifyURL, "alice") {
		t.Fatalf("expected verification to FAIL (degrade) on GitHub API error")
	}
	assertShortCooldown(t, hub, "myorg/repo1", 7)
}

// TestVerifyReportedPR_HubEmptyURL: no PR reported → not verified, without any
// GitHub call.
func TestVerifyReportedPR_HubEmptyURL(t *testing.T) {
	hub := prVerifyHub(t, http.StatusOK, "alice", 7)
	if hub.verifyReportedPR("repo1", "", "alice") {
		t.Fatalf("empty PR URL must never verify")
	}
}

// TestVerifyReportedPR_HubNoClient: a hub without a GitHub client cannot verify,
// so it must degrade to unverified rather than crash.
func TestVerifyReportedPR_HubNoClient(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.GHClient = nil
	s.RegisterAPI(deps)
	hub := NewContributeWSHub(logger, s)

	if hub.verifyReportedPR("repo1", prVerifyURL, "alice") {
		t.Fatalf("no-client hub must not verify")
	}
}

// assertShortCooldown confirms that a completion recorded WITHOUT a verified PR
// (empty URL passed to markTaskCompleted, exactly what the handler does on a
// failed verification) gets the short no-PR cooldown, preserving the
// anti-duplicate protection (#2492/#2557) without over-rewarding.
func assertShortCooldown(t *testing.T, hub *ContributeWSHub, repo string, num int) {
	t.Helper()
	hub.markTaskCompleted(repo, num, "") // unverified → no PR credit for cooldown
	key := repo + "#" + strconv.Itoa(num)
	if !hub.isTaskInCooldown(repo, num) {
		t.Fatalf("unverified completion should still hold a short cooldown (anti-dup), got none")
	}
	// Aged just past the short window → released, proving it is NOT the 168h lock.
	hub.completedMu.Lock()
	hub.completedTasks[key] = time.Now().Add(-(completedNoPRCooldownHours + 1) * time.Hour)
	hub.completedMu.Unlock()
	if hub.isTaskInCooldown(repo, num) {
		t.Fatalf("unverified completion should release after the short %dh window, not hold the full %dh lock",
			completedNoPRCooldownHours, completedTaskCooldownHours)
	}
}
