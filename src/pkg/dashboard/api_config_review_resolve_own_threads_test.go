package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

// review.resolve_own_threads is on when unset, editable through the owner's
// PUT /api/config/review (the endpoint the admin MCP review.settings op
// writes through), stored as the explicit value sent, and left alone by a
// PUT that does not name it.
func TestReviewConfigPut_ResolveOwnThreads(t *testing.T) {
	s := covApiServer(t)
	if s.deps.Config.Review.ResolveOwnThreads != nil || !s.deps.Config.Review.ResolveOwnThreadsEnabled() {
		t.Fatalf("fresh config must leave resolve_own_threads unset (on): %+v", s.deps.Config.Review.ResolveOwnThreads)
	}
	if rec := doPutNoRole(s, "/api/config/review", `{"resolve_own_threads":false}`); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated PUT resolve_own_threads: expected 403, got %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/review", map[string]any{"resolve_own_threads": false}); rec.Code != http.StatusOK {
		t.Fatalf("PUT resolve_own_threads=false: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Review.ResolveOwnThreadsEnabled() {
		t.Fatal("resolve_own_threads=false not applied")
	}
	if rec := doPut(s, "/api/config/review", map[string]any{"post_comments": true}); rec.Code != http.StatusOK {
		t.Fatalf("PUT without the key: expected 200, got %d", rec.Code)
	}
	if s.deps.Config.Review.ResolveOwnThreadsEnabled() {
		t.Fatal("absent resolve_own_threads key changed the stored value")
	}
	rec := doGetNoRole(s, "/api/config/review")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"resolve_own_threads":false`) {
		t.Fatalf("GET review must expose the explicit value: %d %s", rec.Code, rec.Body.String())
	}
}

// The Review Gate card carries the switch, on unless explicitly false, with
// its help mark pointing at the docs section.
func TestReviewGateResolveOwnThreadsToggle(t *testing.T) {
	features := extractJSFunction(t, indexHTML(t), "renderGovFeatures")
	for _, want := range []string{
		`data-key="resolve_own_threads"`,
		`rv.resolve_own_threads !== false ? 'on' : ''`,
		"yamlHint('review.resolve_own_threads')",
		"settingHelpMark('review-bot-threads', 'the-hives-own-review-threads'",
	} {
		if !strings.Contains(features, want) {
			t.Fatalf("Settings → Features → Review Gate has no %q", want)
		}
	}
}
