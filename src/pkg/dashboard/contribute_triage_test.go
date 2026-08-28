package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ghpkg "github.com/kubestellar/hive/pkg/github"
)

// TestDeriveTriageLevel table-tests the ONE documented mapping from live signals
// onto the Warp-style ladder, covering every level and the precedence boundaries
// (merged-PR/closed win over everything; open PR splits Reviewing vs Implementing;
// fleet vs queue vs neither).
func TestDeriveTriageLevel(t *testing.T) {
	openPR := &prLinkStatus{Number: 10, State: prLinkStateOpen}
	mergedPR := &prLinkStatus{Number: 11, State: prLinkStateMerged}

	cases := []struct {
		name string
		sig  triageSignals
		want string
	}{
		{"open unclaimed unqueued no-pr -> triaging", triageSignals{}, triageTriaging},
		{"admitted to ready queue -> ready", triageSignals{inReadyQueue: true}, triageReady},
		{"in fleet -> implementing", triageSignals{inFleet: true}, triageImplementing},
		{"in fleet even if also queued -> implementing", triageSignals{inFleet: true, inReadyQueue: true}, triageImplementing},
		{"open linked PR, not in fleet -> reviewing", triageSignals{prLink: openPR}, triageReviewing},
		{"open linked PR while queued -> reviewing", triageSignals{prLink: openPR, inReadyQueue: true}, triageReviewing},
		{"open linked PR AND in fleet -> implementing", triageSignals{prLink: openPR, inFleet: true}, triageImplementing},
		{"merged linked PR -> closed", triageSignals{prLink: mergedPR}, triageClosed},
		{"merged PR wins over fleet+queue -> closed", triageSignals{prLink: mergedPR, inFleet: true, inReadyQueue: true}, triageClosed},
		{"issue closed -> closed", triageSignals{closed: true}, triageClosed},
		{"issue closed wins over open PR -> closed", triageSignals{closed: true, prLink: openPR}, triageClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveTriageLevel(tc.sig); got != tc.want {
				t.Errorf("deriveTriageLevel(%+v) = %q, want %q", tc.sig, got, tc.want)
			}
		})
	}
}

// newPRLinkTestResolver builds a resolver whose lookups hit a go-github client
// pointed at the given httptest server, with a fixed clock so TTL behaviour is
// deterministic.
func newPRLinkTestResolver(serverURL string) *prLinkResolver {
	gc := ghpkg.NewClientForTest(serverURL, "testorg", []string{"testorg/repo"}, slog.Default()).GoGitHub()
	r := newPRLinkResolver(gc)
	fixed := time.Unix(1700000000, 0)
	r.now = func() time.Time { return fixed }
	return r
}

// searchIssuesResponse builds a /search/issues body with the given PR items.
func searchIssuesResponse(items ...map[string]any) map[string]any {
	return map[string]any{"total_count": len(items), "items": items}
}

// TestPRLinkResolver_Open resolves an OPEN linked PR referencing the issue.
func TestPRLinkResolver_Open(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(searchIssuesResponse(map[string]any{
			"number":       77,
			"state":        "open",
			"title":        "Fix the thing",
			"body":         "Fixes #123",
			"html_url":     "https://github.com/testorg/repo/pull/77",
			"pull_request": map[string]any{"url": "https://api.github.com/repos/testorg/repo/pulls/77"},
		}))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	r := newPRLinkTestResolver(ts.URL)
	got := r.resolve(context.Background(), "testorg/repo", 123)
	if got == nil {
		t.Fatal("expected a linked PR, got nil")
	}
	if got.Number != 77 || got.State != prLinkStateOpen {
		t.Errorf("got %+v, want #77 open", got)
	}
}

// TestPRLinkResolver_Merged resolves a MERGED linked PR (merged_at set), which
// must win even if an open PR is also present.
func TestPRLinkResolver_Merged(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(searchIssuesResponse(
			map[string]any{
				"number":       80,
				"state":        "open",
				"body":         "Also touches #123",
				"html_url":     "https://github.com/testorg/repo/pull/80",
				"pull_request": map[string]any{"url": "x"},
			},
			map[string]any{
				"number":       81,
				"state":        "closed",
				"body":         "Closes #123",
				"html_url":     "https://github.com/testorg/repo/pull/81",
				"pull_request": map[string]any{"url": "y", "merged_at": "2024-01-02T03:04:05Z"},
			},
		))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	r := newPRLinkTestResolver(ts.URL)
	got := r.resolve(context.Background(), "testorg/repo", 123)
	if got == nil {
		t.Fatal("expected a linked PR, got nil")
	}
	if got.Number != 81 || got.State != prLinkStateMerged {
		t.Errorf("got %+v, want #81 merged", got)
	}
}

// TestPRLinkResolver_None returns nil when no PR references the issue (the search
// hit is a plain issue, not a PR, and/or references a different number).
func TestPRLinkResolver_None(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		// An item with no pull_request object is an issue, not a PR — must be ignored.
		_ = json.NewEncoder(w).Encode(searchIssuesResponse(map[string]any{
			"number": 5, "state": "open", "body": "mentions #123 but is an issue",
		}))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	r := newPRLinkTestResolver(ts.URL)
	if got := r.resolve(context.Background(), "testorg/repo", 123); got != nil {
		t.Errorf("expected no linked PR, got %+v", got)
	}
}

// TestPRLinkResolver_ErrorDegrades proves a GitHub failure degrades gracefully to
// "no linked PR" (nil) instead of propagating an error or panicking.
func TestPRLinkResolver_ErrorDegrades(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	r := newPRLinkTestResolver(ts.URL)
	if got := r.resolve(context.Background(), "testorg/repo", 123); got != nil {
		t.Errorf("expected nil on GitHub error, got %+v", got)
	}
}

// TestPRLinkResolver_NilClientDegrades proves a resolver with no GitHub client
// (the missing-wiring path) returns nil rather than panicking.
func TestPRLinkResolver_NilClientDegrades(t *testing.T) {
	r := newPRLinkResolver(nil)
	if got := r.resolve(context.Background(), "testorg/repo", 123); got != nil {
		t.Errorf("expected nil with no GH client, got %+v", got)
	}
}

// TestPRLinkResolver_Caches proves a second resolve within the TTL does NOT hit
// GitHub again (the cached result is reused).
func TestPRLinkResolver_Caches(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(searchIssuesResponse(map[string]any{
			"number":       9,
			"state":        "open",
			"body":         "Fixes #123",
			"html_url":     "https://github.com/testorg/repo/pull/9",
			"pull_request": map[string]any{"url": "x"},
		}))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	r := newPRLinkTestResolver(ts.URL) // fixed clock -> always within TTL
	_ = r.resolve(context.Background(), "testorg/repo", 123)
	_ = r.resolve(context.Background(), "testorg/repo", 123)
	if calls != 1 {
		t.Errorf("expected 1 GitHub call (second served from cache), got %d", calls)
	}
}

// TestBuildTriageSnapshotEmptyShell proves that with no contribute hub the builder
// still returns the full ladder (every rung present, zero counts) so the UI has a
// stable shape.
func TestBuildTriageSnapshotEmptyShell(t *testing.T) {
	s := NewServer(0, slog.Default())
	snap := s.buildTriageSnapshot(context.Background())
	if len(snap.Groups) != len(triageLevelOrder) {
		t.Fatalf("expected %d groups, got %d", len(triageLevelOrder), len(snap.Groups))
	}
	for i, g := range snap.Groups {
		if g.Level != triageLevelOrder[i] {
			t.Errorf("group %d level = %q, want %q", i, g.Level, triageLevelOrder[i])
		}
		if g.Count != 0 || len(g.Issues) != 0 {
			t.Errorf("group %q should be empty, got count=%d issues=%d", g.Level, g.Count, len(g.Issues))
		}
	}
	if snap.Total != 0 {
		t.Errorf("empty snapshot total = %d, want 0", snap.Total)
	}
}

// TestContributeTriageRenderMarkup asserts the triage SECTION (inside Operations,
// not a new tab) and the PR-badge machinery render into the served page: the card,
// the ladder container, the per-level groups container, the JS poll + badge
// helpers, and the triage endpoint wiring.
func TestContributeTriageRenderMarkup(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		// Triage card lives INSIDE the Operations tab panel (a section, not a tab).
		`id="cc-triage-card"`,
		`<h3>Issue triage</h3>`,
		`id="cc-triage-ladder"`,
		`id="cc-triage-groups"`,
		// Client wiring: the poll + render + PR-badge helpers and the endpoint.
		`function ccTriagePoll(`,
		`function ccRenderTriage(`,
		`function ccPRBadgeHTML(`,
		`/api/contribute/triage`,
		// The five ladder levels are named client-side in lifecycle order.
		`['triaging','Triaging']`,
		`['ready','Ready to implement']`,
		`['implementing','Implementing']`,
		`['reviewing','Reviewing']`,
		`['closed','Closed']`,
		// PR badge CSS classes (part c) present for open/merged states.
		`.cc-pr-badge.pr-open`,
		`.cc-pr-badge.pr-merged`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing triage/PR-link marker %q", want)
		}
	}

	// The triage card must sit inside the Operations panel (#tab-ops), before the
	// leaderboard panel — i.e. it is a section OF Operations, not a separate tab.
	ops := strings.Index(body, `id="tab-ops"`)
	card := strings.Index(body, `id="cc-triage-card"`)
	lb := strings.Index(body, `id="tab-leaderboard"`)
	if ops < 0 || card <= ops || (lb >= 0 && card >= lb) {
		t.Errorf("triage card is not inside the Operations panel (ops=%d card=%d lb=%d)", ops, card, lb)
	}
}

// TestContributeTriageEndpoint hits the JSON endpoint and asserts it returns the
// full ladder shape (all levels present), read-only, 200.
func TestContributeTriageEndpoint(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/triage", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var snap triageSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("triage response not valid JSON: %v", err)
	}
	if len(snap.Groups) != len(triageLevelOrder) {
		t.Fatalf("expected %d ladder groups, got %d", len(triageLevelOrder), len(snap.Groups))
	}
	for i, want := range triageLevelOrder {
		if snap.Groups[i].Level != want {
			t.Errorf("group %d = %q, want %q", i, snap.Groups[i].Level, want)
		}
	}
}
