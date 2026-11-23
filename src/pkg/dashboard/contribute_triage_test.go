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

	ghpkg "github.com/hivecommons/hive/pkg/github"
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
		`<h3>Issue lifecycle</h3>`,
		`aria-label="Move Issue lifecycle card"`,
		`data-ops-card="triage"`,
		`id="cc-triage-ladder"`,
		`id="cc-triage-groups"`,
		// Client wiring: the poll + render + PR-badge helpers and the endpoint.
		`function ccTriagePoll(`,
		`function ccRenderTriage(`,
		`function ccPRBadgeHTML(`,
		`/api/contribute/triage`,
		// The five ladder levels are named client-side in lifecycle order. The
		// first keeps its "triaging" key but reads Held back.
		`['triaging','Held back']`,
		`['ready','Ready to implement']`,
		`['implementing','Implementing']`,
		`['reviewing','Reviewing']`,
		`['closed','Closed']`,
		// PR badge CSS classes (part c) present for open/merged states.
		`.cc-pr-badge.pr-open`,
		`.cc-pr-badge.pr-merged`,
		// Held back rows show the server's reason tag, hover text from detail.
		`.cc-tg-why{`,
		`it.withheld_tag`,
		`esc(it.withheld_detail||tag)`,
		`it.withheld_tag||'Reason not recorded'`,
		`Held back lists the issues the queue is not offering to agents right now`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing triage/PR-link marker %q", want)
		}
	}
	// The old label read as "awaiting triage" for issues held back for any
	// reason; it must not come back as a displayed name.
	for _, gone := range []string{`['triaging','Triaging']`, `remain in Triaging`, `<h3>Issue triage</h3>`} {
		if strings.Contains(body, gone) {
			t.Errorf("rendered page still shows the old Triaging label: %q", gone)
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

	mainCol := strings.Index(body, `id="ops-main-column"`)
	wideCol := strings.Index(body, `id="ops-wide-region"`)
	if mainCol < 0 || wideCol < 0 || card <= mainCol || card >= wideCol {
		t.Errorf("triage card is not in the Operations center column (main=%d card=%d wide=%d)", mainCol, card, wideCol)
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

// findTriageIssue returns the ladder row for repo#number and the rung it is on.
func findTriageIssue(snap triageSnapshot, repo string, number int) (triageIssue, bool) {
	for _, g := range snap.Groups {
		for _, it := range g.Issues {
			if it.Repo == repo && it.Number == number {
				return it, true
			}
		}
	}
	return triageIssue{}, false
}

// TestTriageHeldBackLabel pins the first rung's name: the key stays
// "triaging" so readers of the JSON are unaffected, the label reads Held back.
func TestTriageHeldBackLabel(t *testing.T) {
	if triageTriaging != "triaging" {
		t.Fatalf("the first rung's key changed to %q; it must stay \"triaging\"", triageTriaging)
	}
	if got := triageLevelLabel[triageTriaging]; got != "Held back" {
		t.Fatalf("first rung label = %q, want \"Held back\"", got)
	}
	snap := emptyTriageSnapshot()
	if snap.Groups[0].Level != "triaging" || snap.Groups[0].Label != "Held back" {
		t.Fatalf("first group = %q/%q, want triaging/Held back", snap.Groups[0].Level, snap.Groups[0].Label)
	}
}

// TestTriageHeldBackRowsCarryTheWithheldReason runs every exclusion cause of
// the shared withheld ladder through the lifecycle card: the Held back row
// carries the same reason code the queue's admission pass recorded, a tag the
// vocabulary knows, and the row's detail sentence. The control (#700) is Ready
// and carries no reason.
func TestTriageHeldBackRowsCarryTheWithheldReason(t *testing.T) {
	for _, tc := range withheldCases() {
		t.Run(tc.name, func(t *testing.T) {
			hub, s := withheldHub(t, tc.decorate)
			if tc.arrange != nil {
				tc.arrange(t, hub, s)
			}
			s.contributeHub = hub
			snap := s.buildTriageSnapshot(context.Background())

			it, ok := findTriageIssue(snap, "projectbluefin/dakota", 601)
			if !ok {
				t.Fatalf("#601 is missing from the ladder: %+v", snap.Groups)
			}
			if it.Level != triageTriaging {
				// An in-flight issue the fleet reports is Implementing, which
				// explains itself; it must not carry a Held back reason.
				if tc.reason != withheldReasonInFlight {
					t.Fatalf("#601 level = %q, want %q", it.Level, triageTriaging)
				}
				if it.WithheldReason != "" || it.WithheldTag != "" {
					t.Fatalf("a %s row must not carry a Held back reason: %+v", it.Level, it)
				}
				return
			}
			if it.WithheldReason != tc.reason {
				t.Fatalf("withheld_reason = %q, want %q", it.WithheldReason, tc.reason)
			}
			if it.WithheldTag == "" || strings.HasPrefix(it.WithheldTag, withheldOtherTagPrefix) || it.WithheldTag == withheldUnrecordedTag {
				t.Fatalf("withheld_tag = %q, want a known tag for %q", it.WithheldTag, tc.reason)
			}
			if it.WithheldDetail == "" {
				t.Fatal("a Held back row needs its detail sentence for the hover text")
			}

			ctl, ok := findTriageIssue(snap, "projectbluefin/dakota", 700)
			if !ok || ctl.Level != triageReady {
				t.Fatalf("control #700 = %+v (found %v), want Ready", ctl, ok)
			}
			if ctl.WithheldReason != "" || ctl.WithheldTag != "" || ctl.WithheldDetail != "" {
				t.Fatalf("a Ready row must carry no withheld fields: %+v", ctl)
			}
		})
	}
}

// TestTriageHeldBackNamesTheSkipLabel: a skip-label refusal is tagged with the
// label in plain words, so needs-triage reads as the one case that really is
// waiting for triage.
func TestTriageHeldBackNamesTheSkipLabel(t *testing.T) {
	hub, s := withheldHub(t, func(issue map[string]any) { issue["labels"] = []any{"needs-triage"} })
	s.contributeHub = hub
	it, ok := findTriageIssue(s.buildTriageSnapshot(context.Background()), "projectbluefin/dakota", 601)
	if !ok || it.Level != triageTriaging {
		t.Fatalf("#601 = %+v (found %v), want Held back", it, ok)
	}
	if it.WithheldReason != contributorAdmissionReasonLabelSkipped || it.WithheldTag != "Needs triage" {
		t.Fatalf("reason/tag = %q/%q, want %q/\"Needs triage\"", it.WithheldReason, it.WithheldTag, contributorAdmissionReasonLabelSkipped)
	}
}

// TestTriageHeldBackOperatorHold: an operator-held queue row is Held back with
// its own tag (the queue never reports an operator's hold as withheld), and
// the operator's note rides in the hover text.
func TestTriageHeldBackOperatorHold(t *testing.T) {
	hub, s := withheldHub(t, nil)
	s.deps.Config.Hub.ContributeQueueHold = []string{withheldKey601}
	s.deps.Config.Hub.ContributeQueueHoldReasons = map[string]string{withheldKey601: "waiting on design review"}
	s.contributeHub = hub
	it, ok := findTriageIssue(s.buildTriageSnapshot(context.Background()), "projectbluefin/dakota", 601)
	if !ok || it.Level != triageTriaging {
		t.Fatalf("#601 = %+v (found %v), want Held back", it, ok)
	}
	if it.WithheldReason != triageHeldOperatorHold || it.WithheldTag != "On hold" {
		t.Fatalf("reason/tag = %q/%q, want %q/\"On hold\"", it.WithheldReason, it.WithheldTag, triageHeldOperatorHold)
	}
	if !strings.Contains(it.WithheldDetail, "waiting on design review") {
		t.Fatalf("detail %q does not carry the operator's hold note", it.WithheldDetail)
	}
}

// TestTriageHeldBackPastQueueLimitIsReady: admitted issues past the queue's
// row limit are not refused, and selectTask offers from the whole admitted
// set, so they sit on Ready rather than Held back.
func TestTriageHeldBackPastQueueLimitIsReady(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	extra := 17
	s.statusMu.Lock()
	s.status = seedActionable("projectbluefin/common", readyQueueDefaultLimit+extra)
	s.statusMu.Unlock()

	snap := s.buildTriageSnapshot(context.Background())
	if got := triageCount(snap, triageTriaging); got != 0 {
		t.Fatalf("held back count = %d, want 0: every issue was admitted", got)
	}
	if got := triageCount(snap, triageReady); got != readyQueueDefaultLimit+extra {
		t.Fatalf("ready count = %d, want %d (the listed queue plus the %d past its limit)", got, readyQueueDefaultLimit+extra, extra)
	}
	for _, g := range snap.Groups {
		for _, it := range g.Issues {
			if it.WithheldReason != "" || it.WithheldTag != "" {
				t.Errorf("#%d on %s carries a held back reason %q", it.Number, g.Level, it.WithheldReason)
			}
		}
	}
}

// TestTriageHeldBackIssueClosed: a scan row that says the issue is closed is
// skipped by the admission pass before any gate, so it has no withheld record;
// the card names that instead of leaving the row unexplained.
func TestTriageHeldBackIssueClosed(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{{Name: "r", Full: "acme/r", ActionableIssues: []any{
		map[string]any{"number": float64(5), "title": "already closed", "state": "closed"},
		map[string]any{"number": float64(6), "title": "open work"},
	}}}}
	s.statusMu.Unlock()

	snap := s.buildTriageSnapshot(context.Background())
	it, ok := findTriageIssue(snap, "acme/r", 5)
	if !ok || it.Level != triageTriaging {
		t.Fatalf("#5 = %+v (found %v), want Held back", it, ok)
	}
	if it.WithheldReason != triageHeldIssueClosed || it.WithheldTag != "Issue closed" || it.WithheldDetail == "" {
		t.Fatalf("#5 reason/tag/detail = %q/%q/%q, want %q/\"Issue closed\"/<sentence>", it.WithheldReason, it.WithheldTag, it.WithheldDetail, triageHeldIssueClosed)
	}
	if open, ok := findTriageIssue(snap, "acme/r", 6); !ok || open.Level != triageReady {
		t.Fatalf("control #6 = %+v (found %v), want Ready", open, ok)
	}
}

// TestTriageHeldBackUnrecorded: the withheld list stops at the queue's row
// limit, so a refusal past it has no record. Its row still reads something
// (never blank), and nothing claims to know the reason.
func TestTriageHeldBackUnrecorded(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	trackers := func(n int) []any {
		out := make([]any, 0, n)
		for i := 1; i <= n; i++ {
			out = append(out, map[string]any{"number": float64(i), "title": "tracker", "is_tracker": true})
		}
		return out
	}
	// zzz/full is scanned first and fills the withheld list; aaa/late is
	// scanned after, so its refusal is not recorded, and it sorts first on the
	// rung, so it is inside the listed rows.
	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{
		{Name: "full", Full: "zzz/full", ActionableIssues: trackers(readyQueueDefaultLimit)},
		{Name: "late", Full: "aaa/late", ActionableIssues: trackers(1)},
	}}
	s.statusMu.Unlock()

	snap := s.buildTriageSnapshot(context.Background())
	it, ok := findTriageIssue(snap, "aaa/late", 1)
	if !ok || it.Level != triageTriaging {
		t.Fatalf("aaa/late#1 = %+v (found %v), want Held back", it, ok)
	}
	if it.WithheldReason != triageHeldUnrecorded || it.WithheldTag != withheldUnrecordedTag || it.WithheldDetail == "" {
		t.Fatalf("reason/tag/detail = %q/%q/%q, want %q/%q/<sentence>", it.WithheldReason, it.WithheldTag, it.WithheldDetail, triageHeldUnrecorded, withheldUnrecordedTag)
	}
	recorded, ok := findTriageIssue(snap, "zzz/full", 1)
	if ok && recorded.WithheldReason != withheldReasonTracker {
		t.Fatalf("a recorded refusal lost its reason: %+v", recorded)
	}
}

// TestTriageHeldBackNeedsTriageNote: the needs-triage hover says the label
// holds back only outside authors' issues, since the same label also sits on
// offered issues from trusted authors.
func TestTriageHeldBackNeedsTriageNote(t *testing.T) {
	hub, s := withheldHub(t, func(issue map[string]any) { issue["labels"] = []any{"needs-triage"} })
	s.contributeHub = hub
	it, ok := findTriageIssue(s.buildTriageSnapshot(context.Background()), "projectbluefin/dakota", 601)
	if !ok || it.Level != triageTriaging {
		t.Fatalf("#601 = %+v (found %v), want Held back", it, ok)
	}
	if !strings.Contains(it.WithheldDetail, "needs-triage") || !strings.Contains(it.WithheldDetail, "outside authors") {
		t.Fatalf("detail %q should keep the label and explain it applies only to outside authors", it.WithheldDetail)
	}
}

// TestTriageHeldBackDisabledRepo: the #6449 shape (a disabled repository)
// keeps every candidate on the first rung, now tagged with why.
func TestTriageHeldBackDisabledRepo(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	deps := testDeps(t)
	deps.Config.Hub.DisabledRepos = []string{"projectbluefin/common"}
	s.RegisterAPI(deps)
	s.statusMu.Lock()
	s.status = seedActionable("projectbluefin/common", 3)
	s.statusMu.Unlock()

	snap := s.buildTriageSnapshot(context.Background())
	for n := 1; n <= 3; n++ {
		it, ok := findTriageIssue(snap, "projectbluefin/common", n)
		if !ok || it.Level != triageTriaging {
			t.Fatalf("#%d = %+v (found %v), want Held back", n, it, ok)
		}
		if it.WithheldReason != withheldReasonDisabledRepo || it.WithheldTag != "Repo switched off" {
			t.Fatalf("#%d reason/tag = %q/%q, want %q", n, it.WithheldReason, it.WithheldTag, withheldReasonDisabledRepo)
		}
	}
}

// TestTriageIssueWithheldFieldsAreAdditive: a row with no reason marshals with
// exactly the keys it had before, so existing readers of the endpoint see no
// change on any rung but Held back.
func TestTriageIssueWithheldFieldsAreAdditive(t *testing.T) {
	b, err := json.Marshal(triageIssue{Repo: "a/b", Number: 1, Title: "t", Level: triageReady})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "withheld") {
		t.Fatalf("a row with no reason grew withheld keys: %s", b)
	}
	b, err = json.Marshal(triageIssue{Repo: "a/b", Number: 1, Level: triageTriaging, WithheldReason: "tracker", WithheldTag: "Epic or tracker", WithheldDetail: "Tracker / umbrella issue"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"withheld_reason":"tracker"`, `"withheld_tag":"Epic or tracker"`, `"withheld_detail":"Tracker / umbrella issue"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("held back row JSON missing %s: %s", key, b)
		}
	}
}
