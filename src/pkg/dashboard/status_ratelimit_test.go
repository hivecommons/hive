package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Regression coverage for kubestellar/hive#5733 at the payload boundary — the
// dashboard card reads exactly these fields.
//
// The card reported full headroom while ~89% of the App installation's budget
// was spent, because a just-minted installation token transiently reports a
// fresh, EMPTY bucket for a budget that is genuinely shared. buildGHRateLimits
// latched that reading. The clamp lives in pkg/github so every consumer
// inherits it; these tests pin that the corrected value and its observation
// time actually reach /api/status.

func rateLimitTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// rateLimitServer serves a partly-spent bucket first, then the fresh-bucket
// artifact — full, and carrying its own later reset, exactly as measured.
func rateLimitServer(t *testing.T) *httptest.Server {
	t.Helper()
	reset := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	artifactReset := reset.Add(155 * time.Second)

	var mu sync.Mutex
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/rate_limit" {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "workflow_runs": []any{}, "workflows": []any{}})
			return
		}
		mu.Lock()
		call++
		n := call
		mu.Unlock()
		remaining, rs := 6815, reset
		if n > 1 {
			remaining, rs = 7100, artifactReset
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resources": map[string]any{
				"core": map[string]any{"limit": 7100, "remaining": remaining, "reset": rs.Unix()},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestBuildGHRateLimits_DoesNotReportFalseHeadroom is the payload-level form of
// the bug: two consecutive builds, and the second must not claim the budget was
// restored. On the reporter's hive the card sat at the full limit for six
// minutes while 455 real requests were consumed.
func TestBuildGHRateLimits_DoesNotReportFalseHeadroom(t *testing.T) {
	deps := testDeps(t)
	srv := rateLimitServer(t)
	ghc := ghpkg.NewClientForTest(srv.URL, "myorg", []string{"repo1"}, rateLimitTestLogger())
	ctx := context.Background()

	first := buildGHRateLimits(ghc, ctx, deps.Config)
	firstCore, ok := first["core"].(map[string]any)
	if !ok {
		t.Fatalf("core is %T, want a map", first["core"])
	}
	if firstCore["remaining"] != 6815 {
		t.Fatalf("first build remaining = %v, want the observed 6815", firstCore["remaining"])
	}

	second := buildGHRateLimits(ghc, ctx, deps.Config)
	secondCore, ok := second["core"].(map[string]any)
	if !ok {
		t.Fatalf("core is %T, want a map", second["core"])
	}
	if secondCore["remaining"] != 6815 {
		t.Fatalf("second build remaining = %v, want 6815 — a value that ROSE within one "+
			"window is the false-full card of #5733, and an operator scales the wrong "+
			"thing on it", secondCore["remaining"])
	}
}

// TestBuildGHRateLimits_CarriesObservedAt pins the staleness signal the card
// needs. reset cannot answer "how old is this number": it moves independently
// of the sample, and on the reporter's hive it was 8.5 minutes adrift while the
// card sat pinned at the limit.
func TestBuildGHRateLimits_CarriesObservedAt(t *testing.T) {
	deps := testDeps(t)
	srv := rateLimitServer(t)
	ghc := ghpkg.NewClientForTest(srv.URL, "myorg", []string{"repo1"}, rateLimitTestLogger())

	core, ok := buildGHRateLimits(ghc, context.Background(), deps.Config)["core"].(map[string]any)
	if !ok {
		t.Fatalf("core is not a map")
	}
	raw, ok := core["observed_at"].(string)
	if !ok {
		t.Fatalf("core has no observed_at; the card cannot distinguish a held value from a "+
			"fresh one without it (got %v)", core)
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("observed_at %q is not RFC3339: %v", raw, err)
	}
	if time.Since(ts) > time.Minute {
		t.Errorf("observed_at = %v, want roughly now", ts)
	}
}

// TestBuildGHRateLimits_OmitsObservedAtWhenUnknown: a zero timestamp rendered
// as 1970 would read as "catastrophically stale" on a card whose whole job is
// to be trusted. The field is emitted only when a reading was actually taken.
func TestBuildGHRateLimits_OmitsObservedAtWhenUnknown(t *testing.T) {
	deps := testDeps(t)

	// No client: the core map stays empty and carries no timestamp.
	core, ok := buildGHRateLimits(nil, nil, deps.Config)["core"].(map[string]any)
	if !ok {
		t.Fatalf("core is not a map")
	}
	if _, present := core["observed_at"]; present {
		t.Errorf("observed_at present with no client; got %v", core)
	}
}
