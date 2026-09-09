package hub

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// The 2026-09-09 shape: v4's tip is an image-less release commit on top of a
// commit whose hub image is published; the hub is one commit further back.
// Full SHAs so the branch/commits payloads pass the StandardSHALen check.
const (
	walkbackTip     = "c7a88b8000000000000000000000000000000000" // release: v4.20.1 — no image
	walkbackFix     = "d566f97000000000000000000000000000000000" // #6294 — hub image published
	walkbackRunning = "bd68d95000000000000000000000000000000000" // what the hub runs
)

// walkbackServer serves a v4 branch whose tip is walkbackTip, a commits list
// [tip, fix, running], and a GHCR where only the tags in published exist. It
// counts manifest probes so tests can pin how much the walk costs.
func walkbackServer(t *testing.T, published map[string]bool, probes *atomic.Int32) {
	t.Helper()
	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			probes.Add(1)
			tag := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if published[tag] {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, "/token"):
			_, _ = w.Write([]byte(`{"token":"anon"}`))
		case strings.Contains(r.URL.Path, "/branches/"):
			fmt.Fprintf(w, `{"commit":{"sha":%q,"commit":{"message":"release: v4.20.1"}}}`, walkbackTip)
		case strings.HasSuffix(r.URL.Path, "/commits"):
			if got := r.URL.Query().Get("sha"); got != "v4" {
				t.Errorf("commits listed for %q, want the polled branch v4", got)
			}
			fmt.Fprintf(w, `[
				{"sha":%q,"commit":{"message":"release: v4.20.1"}},
				{"sha":%q,"commit":{"message":"fix(dashboard): measure behind against the reachable target\n\nbody"}},
				{"sha":%q,"commit":{"message":"docs: backend-setup"}}
			]`, walkbackTip, walkbackFix, walkbackRunning)
		case strings.Contains(r.URL.Path, "/actions/workflows/"):
			_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
}

func TestFetchBranchSHAHubTargetWalksBackPastImagelessTip(t *testing.T) {
	resetSHACaches(t)
	var probes atomic.Int32
	walkbackServer(t, map[string]bool{shortSHA(walkbackFix): true}, &probes)
	latestSHAMu.Lock()
	latestHubSHAByBranch["v4"] = branchSHAInfo{SHA: shortSHA(walkbackRunning)}
	latestSHAMu.Unlock()

	fetchBranchSHA(slog.Default(), "v4")

	if got, want := getLatestHubSHAForBranch("v4"), shortSHA(walkbackFix); got != want {
		t.Fatalf("hub target = %q, want the newest published ancestor %q", got, want)
	}
	latestSHAMu.RLock()
	msg := commitMsgBySHA[shortSHA(walkbackFix)]
	latestSHAMu.RUnlock()
	if !strings.HasPrefix(msg, "fix(dashboard)") || strings.Contains(msg, "body") {
		t.Errorf("cached message %q, want the first line of the walked-back commit", msg)
	}
}

func TestFetchBranchSHAHubTargetWalkbackStopsAtCurrentTarget(t *testing.T) {
	resetSHACaches(t)
	var probes atomic.Int32
	// Only the running commit has an image; the poller must not probe it, since
	// it is already the target and nothing newer is published.
	walkbackServer(t, map[string]bool{shortSHA(walkbackRunning): true}, &probes)
	latestSHAMu.Lock()
	latestHubSHAByBranch["v4"] = branchSHAInfo{SHA: shortSHA(walkbackRunning)}
	latestSHAMu.Unlock()

	fetchBranchSHA(slog.Default(), "v4")

	if got, want := getLatestHubSHAForBranch("v4"), shortSHA(walkbackRunning); got != want {
		t.Fatalf("hub target = %q, want unchanged %q", got, want)
	}
	// Probes: hub tip, walkback candidate (fix), then spoke tip — never the
	// current target itself.
	if got := probes.Load(); got > 3 {
		t.Errorf("%d manifest probes, want the walk to stop at the current target", got)
	}
}

func TestFetchBranchSHAHubTargetWalkbackNothingPublished(t *testing.T) {
	resetSHACaches(t)
	var probes atomic.Int32
	walkbackServer(t, map[string]bool{}, &probes)

	fetchBranchSHA(slog.Default(), "v4")

	if got := getLatestHubSHAForBranch("v4"); got != "" {
		t.Fatalf("hub target = %q, want none when no commit in the window has an image", got)
	}
}

func TestFetchBranchSHAHubTargetPrefersPublishedTip(t *testing.T) {
	resetSHACaches(t)
	var probes atomic.Int32
	walkbackServer(t, map[string]bool{shortSHA(walkbackTip): true, shortSHA(walkbackFix): true}, &probes)

	fetchBranchSHA(slog.Default(), "v4")

	if got, want := getLatestHubSHAForBranch("v4"), shortSHA(walkbackTip); got != want {
		t.Fatalf("hub target = %q, want the tip %q when its image exists", got, want)
	}
}

func TestListRecentBranchCommitsFailureIsNil(t *testing.T) {
	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // rate-limited
	})
	if got := listRecentBranchCommits(http.DefaultClient, "v4", 5, slog.Default()); got != nil {
		t.Errorf("commit list on HTTP 403 = %v, want nil so the target is left alone", got)
	}
}
