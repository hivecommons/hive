package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// prsHandler serves a fixed list of wirePR for any /repos/{owner}/{repo}/pulls
// request, ignoring pagination params — sufficient for these single-page tests.
func prsHandler(t *testing.T, prs []wirePR) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(prs); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
}

// TestFetchPRs_StaleAppDraftIsSurfaced is the fix under test: the scanner kick
// prompt already instructs "Close stale drafts (>48h, ...)" but fetchPRs
// dropped every draft before the agent could ever see one to act on that
// instruction (kubestellar/hive#3963). An App-authored draft older than
// staleDraftAfter must come back via the new StaleDrafts channel, while still
// being absent from the normal actionable Items — a draft is never mergeable.
func TestFetchPRs_StaleAppDraftIsSurfaced(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/pulls", prsHandler(t, []wirePR{
		{
			Number:    42,
			Title:     "feat: half-finished thing",
			User:      wireUser{Login: "acme-bot[bot]"},
			Draft:     true,
			CreatedAt: hoursAgo(72), // > staleDraftAfter (48h)
			HTMLURL:   "https://github.com/acme/widget/pull/42",
		},
	}))
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"widget"})
	c.appBotLogin = "acme-bot[bot]"

	actionable, held, staleDrafts, total, err := c.fetchPRs(t.Context(), "widget")
	if err != nil {
		t.Fatalf("fetchPRs: %v", err)
	}
	if total != 1 {
		t.Errorf("totalPRs = %d, want 1", total)
	}
	if len(held) != 0 {
		t.Errorf("held = %v, want empty", held)
	}
	if len(actionable) != 0 {
		t.Errorf("actionable = %v, want empty — a draft must never be treated as ready work", actionable)
	}
	if len(staleDrafts) != 1 {
		t.Fatalf("staleDrafts = %d, want 1", len(staleDrafts))
	}
	if staleDrafts[0].Number != 42 || !staleDrafts[0].Draft {
		t.Errorf("staleDrafts[0] = %+v, want number=42 draft=true", staleDrafts[0])
	}
}

// TestFetchPRs_RecentAppDraftNotSurfaced: a draft newer than staleDraftAfter
// may still be an agent's active work-in-progress from the current cycle —
// only OLD drafts are worth nagging about.
func TestFetchPRs_RecentAppDraftNotSurfaced(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/pulls", prsHandler(t, []wirePR{
		{
			Number:    43,
			Title:     "wip",
			User:      wireUser{Login: "acme-bot[bot]"},
			Draft:     true,
			CreatedAt: hoursAgo(1),
			HTMLURL:   "https://github.com/acme/widget/pull/43",
		},
	}))
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"widget"})
	c.appBotLogin = "acme-bot[bot]"

	_, _, staleDrafts, _, err := c.fetchPRs(t.Context(), "widget")
	if err != nil {
		t.Fatalf("fetchPRs: %v", err)
	}
	if len(staleDrafts) != 0 {
		t.Errorf("staleDrafts = %v, want empty for a 1-hour-old draft", staleDrafts)
	}
}

// TestFetchPRs_HumanStaleDraftNotSurfaced: nagging about a HUMAN's stale
// draft is not this mechanism's job — they are actively doing that work, and
// hive has no standing to close or finish it for them. Only the App's own
// drafts age into StaleDrafts.
func TestFetchPRs_HumanStaleDraftNotSurfaced(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/pulls", prsHandler(t, []wirePR{
		{
			Number:    44,
			Title:     "a human's long-running feature branch",
			User:      wireUser{Login: "some-contributor"},
			Draft:     true,
			CreatedAt: hoursAgo(500),
			HTMLURL:   "https://github.com/acme/widget/pull/44",
		},
	}))
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"widget"})
	c.appBotLogin = "acme-bot[bot]"

	_, _, staleDrafts, _, err := c.fetchPRs(t.Context(), "widget")
	if err != nil {
		t.Fatalf("fetchPRs: %v", err)
	}
	if len(staleDrafts) != 0 {
		t.Errorf("staleDrafts = %v, want empty for a human-authored draft", staleDrafts)
	}
}
