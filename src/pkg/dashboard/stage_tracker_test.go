package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestStageTrackerNumbersPinned keeps the layout constants in step with
// stagelabels.Table so a renumbered stage cannot silently move a segment.
func TestStageTrackerNumbersPinned(t *testing.T) {
	pins := map[stagelabels.Stage]int{
		stagelabels.Actionable: trackerNumberActionable, stagelabels.Claimed: trackerNumberClaimed,
		stagelabels.PRInProgress: trackerNumberPRInProgress, stagelabels.CoveredByPR: trackerNumberCoveredByPR,
		stagelabels.ReviewPending: trackerNumberReviewPending, stagelabels.Reviewed: trackerNumberReviewed,
		stagelabels.Fixing: trackerNumberFixing, stagelabels.NeedsAuthor: trackerNumberNeedsAuthor, stagelabels.ReturnedToContributor: trackerNumberReturned,
		stagelabels.AdoptedByHive: trackerNumberAdopted, stagelabels.AwaitingMergeAdmission: trackerNumberAwaitingMerge,
		stagelabels.NeedsDecision: trackerNumberNeedsDecision, stagelabels.ReadyForHumanMerge: trackerNumberReadyForHuman,
	}
	for s, n := range pins {
		if s.Number() != n {
			t.Errorf("%s: table number %d, tracker pin %d", s, s.Number(), n)
		}
	}
	stages := stageTrackerStages(nil)
	if len(stages) != len(stagelabels.Table) {
		t.Fatalf("stages = %d, table = %d", len(stages), len(stagelabels.Table))
	}
	mains, detours := 0, 0
	for _, st := range stages {
		if st.Main {
			mains++
			continue
		}
		detours++
		if st.BranchFrom != trackerNumberReviewPending && st.BranchFrom != trackerNumberAwaitingMerge {
			t.Errorf("detour %s branches from %d", st.Slug, st.BranchFrom)
		}
	}
	if mains != 8 || detours != 6 {
		t.Fatalf("mains=%d detours=%d, want 8 main segments and 6 detours", mains, detours)
	}
}

// Stage 07x by author kind: 071 needs-author (human) and 072
// returned-to-contributor (clanker) are both review-side detours with their
// own chevron word and an author-must-act caption.
func TestStageTrackerNeedsAuthorAndReturnedWords(t *testing.T) {
	for _, tc := range []struct {
		stage             stagelabels.Stage
		word, captionHead string
	}{
		{stagelabels.NeedsAuthor, "Needs author", "Needs @sam"},
		{stagelabels.ReturnedToContributor, "Returned to contributor", "Returned to @sam"},
		{stagelabels.Fixing, "Fixing", "Red or changes requested; a hive agent is fixing it"},
	} {
		if got := stageTrackerShort(tc.stage); got != tc.word {
			t.Errorf("%v: short = %q, want %q", tc.stage, got, tc.word)
		}
		if got := stageTrackerBranchFrom(tc.stage.Number()); got != trackerNumberReviewPending {
			t.Errorf("%v: branches from %d, want %d", tc.stage, got, trackerNumberReviewPending)
		}
		caption, _ := stageTrackerCaption(tc.stage, "check-failing:unit", "sam", "pr", false, stagelabels.Record{}, false, 0)
		if !strings.HasPrefix(caption, tc.captionHead) {
			t.Errorf("%v: caption = %q", tc.stage, caption)
		}
	}
	caption, actor := stageTrackerCaption(stagelabels.NeedsAuthor, "check-failing:unit", "sam", "pr", false, stagelabels.Record{}, false, 0)
	if caption != "Needs @sam — CI is red (check-failing:unit) — @sam must act" || actor != "sam" {
		t.Errorf("071 caption = %q actor = %q", caption, actor)
	}
}

func TestHumanizeStageReason(t *testing.T) {
	cases := map[string]string{
		"actionable":                                    "",
		"live agent claim":                              "",
		"mirrors PR #12332":                             "mirrors PR #12332",
		"label:needs-rebase":                            "needs a rebase",
		"held":                                          "held by a hold label",
		"contributor-lane:no-verdict":                   "no review verdict yet",
		"required-check-pending:build-and-test":         "check build-and-test still running",
		"required-check-failing:DCO":                    "check DCO failing",
		"reviewer-verdict:changes_requested":            "review swarm requested changes",
		"pending: head pushed 59s ago (< min_head_age)": "head pushed 59s ago; waiting for the minimum head age",
		"contributor-lane:soaking:12m":                  "soaking",
		"excluded-label:do-not-merge":                   "label do-not-merge excludes it",
		"some-new-token":                                "some new token",
	}
	for in, want := range cases {
		if got := humanizeStageReason(in); got != want {
			t.Errorf("humanizeStageReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func stageTrackerFixture(t *testing.T, now time.Time) (*Server, *stagelabels.Store) {
	t.Helper()
	s := newTestServer()
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store

	// PR 101: contributor PR returned twice, nudged once, stalled past 24h.
	key101 := stagelabels.Key("acme/console", 101)
	clock = now.Add(-30 * time.Hour)
	store.Observe(key101, stagelabels.ReviewPending, "contributor-lane:no-verdict")
	clock = now.Add(-29 * time.Hour)
	store.Observe(key101, stagelabels.Reviewed, "required-check-pending:build")
	clock = now.Add(-25 * time.Hour)
	store.Observe(key101, stagelabels.ReturnedToContributor, "reviewer-verdict:changes_requested")
	store.MarkNudged(key101, "abc123", 1, true, clock)
	// PR 102: hive PR in the merge queue, fresh.
	clock = now.Add(-40 * time.Minute)
	store.Observe(stagelabels.Key("acme/console", 102), stagelabels.MergeQueue, "contributor-lane:soaking:12m")
	// PR 103: regressed from the merge queue to review-pending.
	key103 := stagelabels.Key("acme/console", 103)
	clock = now.Add(-3 * time.Hour)
	store.Observe(key103, stagelabels.MergeQueue, "contributor-lane:soaking")
	clock = now.Add(-10 * time.Minute)
	store.Observe(key103, stagelabels.ReviewPending, "contributor-lane:no-verdict")
	// Issue 7: mirrors PR 102.
	clock = now.Add(-40 * time.Minute)
	store.Observe(stagelabels.Key("acme/console", 7), stagelabels.MergeQueue, "mirrors PR #102")
	clock = now

	status := minimalPayload()
	status.HiveID = "hv-test"
	status.Repos = []FrontendRepo{{
		Name: "console", Full: "acme/console",
		ActionableIssues: []any{
			github.Issue{Number: 7, Title: "Login redirect loops", Author: "karibhou", AuthorIsHuman: true, Labels: []string{"stage/100-merge-queue"}},
			github.Issue{Number: 8, Title: "Flaky test", Author: "hive-app[bot]", Labels: []string{"stage/010-actionable", "agent/scanner"}},
			// GitHub's issues feed returns pull requests too. These are copies
			// of PRs 101 (also in OpenPrs) and 106 (URL says /pull/, not in the
			// PR set); neither may become an issue row.
			github.Issue{Number: 101, Title: "Fix redirect", Author: "karibhou", AuthorIsHuman: true, Labels: []string{"stage/072-returned-to-contributor"}},
			github.Issue{Number: 106, Title: "Closed-feed PR", Author: "pat", URL: "https://github.com/acme/console/pull/106", Labels: []string{"stage/050-review-pending"}},
		},
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 101, Title: "Fix redirect", Author: "karibhou", Labels: []string{"stage/072-returned-to-contributor", "author/contributor", "needs-rebase"}}},
			FrontendPR{PullRequest: github.PullRequest{Number: 102, Title: "Stabilise test", Author: "hive-app[bot]", AppAuthored: true, Labels: []string{"stage/100-merge-queue", "author/trusted", "author/hive"}}, MergeVerdict: &github.MergeVerdict{Tier: "trusted"}},
			FrontendPR{PullRequest: github.PullRequest{Number: 103, Title: "Relay PR", Author: "zoe", HiveAttributed: true, Labels: []string{"stage/050-review-pending"}}},
			// Label only: the store never saw it.
			FrontendPR{PullRequest: github.PullRequest{Number: 104, Title: "Hand-made", Author: "pat", Labels: []string{"stage/085-needs-decision", "needs-decision"}}},
		},
		HeldPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 105, Title: "Parked", Author: "pat", Labels: []string{"hold"}, Draft: true}},
		},
	}}
	s.UpdateStatus(status)
	return s, store
}

func trackerItem(t *testing.T, resp StageTrackerResponse, kind string, number int) StageTrackerItem {
	t.Helper()
	for _, it := range resp.Items {
		if it.Kind == kind && it.Number == number {
			return it
		}
	}
	t.Fatalf("%s #%d missing from %d items", kind, number, len(resp.Items))
	return StageTrackerItem{}
}

func hasMod(it StageTrackerItem, m string) bool {
	for _, x := range it.Modifiers {
		if x == m {
			return true
		}
	}
	return false
}

// TestStageTrackerEndpoint exercises GET /api/stage-tracker end to end:
// every open issue/PR in the snapshot is a row, the store's stage, since,
// reason, trail and nudge data win over the label, modifiers combine label
// and store health, the author kind degrades from label to inference, and
// rows sort by stage (highest first) then newest transition.
func TestStageTrackerEndpoint(t *testing.T) {
	now := time.Date(2026, 10, 16, 22, 35, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)

	req := httptest.NewRequest(http.MethodGet, "/api/stage-tracker", nil)
	rr := httptest.NewRecorder()
	s.handleStageTracker(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var live StageTrackerResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &live); err != nil {
		t.Fatal(err)
	}
	if len(live.Items) != 7 {
		t.Fatalf("handler items = %d, want 7", len(live.Items))
	}
	// Dwell-based modifiers depend on the clock, so the item assertions use
	// the fixture's frozen now (round-tripped through JSON like the handler).
	var resp StageTrackerResponse
	raw, err := json.Marshal(s.buildStageTracker(s.status, store, now))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.HiveID != "hv-test" || len(resp.Stages) != len(stagelabels.Table) || !resp.AuthorKindLabels {
		t.Fatalf("header: hive=%q stages=%d labels=%v", resp.HiveID, len(resp.Stages), resp.AuthorKindLabels)
	}
	if len(resp.Items) != 7 {
		t.Fatalf("items = %d, want 7 (2 issues + 4 open PRs + 1 held PR; issue-feed PR copies dropped)", len(resp.Items))
	}

	// needs-rebase always lands a PR in 070/072; a sample at 050/060 with the
	// chip would draw a contradictory row.
	for _, it := range resp.Items {
		if slices.Contains(it.Modifiers, "needs-rebase") && (it.Stage == trackerNumberReviewPending || it.Stage == trackerNumberReviewed) {
			t.Fatalf("%s #%d pairs needs-rebase with stage %d", it.Kind, it.Number, it.Stage)
		}
		for _, lp := range it.LinkedPRs {
			if slices.Contains(lp.Modifiers, "needs-rebase") && (lp.Stage == trackerNumberReviewPending || lp.Stage == trackerNumberReviewed) {
				t.Fatalf("issue #%d linked PR #%d pairs needs-rebase with stage %d", it.Number, lp.Number, lp.Stage)
			}
		}
	}
	// The provenance source is surfaced for hive-filed issues from the
	// canonical agent/<lane> label; a human-filed issue carries none.
	if got := trackerItem(t, resp, "issue", 8).Origin; got != "scanner" {
		t.Fatalf("issue 8 origin = %q, want scanner", got)
	}
	if got := trackerItem(t, resp, "issue", 7).Origin; got != "" {
		t.Fatalf("issue 7 origin = %q, want none", got)
	}
	pr101 := trackerItem(t, resp, "pr", 101)
	if pr101.Stage != 72 || pr101.StageLabel != "stage/072-returned-to-contributor" || pr101.Lane != laneContributor {
		t.Fatalf("pr101 stage: %+v", pr101)
	}
	if pr101.StageSince == nil || !pr101.StageSince.Equal(now.Add(-25*time.Hour)) {
		t.Fatalf("pr101 since = %v", pr101.StageSince)
	}
	if pr101.Actor != "karibhou" || pr101.Caption != "Returned to @karibhou — review swarm requested changes — @karibhou must act (1 nudge sent)" {
		t.Fatalf("pr101 caption=%q actor=%q", pr101.Caption, pr101.Actor)
	}
	if !hasMod(pr101, modStalled) || !hasMod(pr101, modNeedsRebase) || hasMod(pr101, modRegressed) {
		t.Fatalf("pr101 modifiers = %v", pr101.Modifiers)
	}
	if pr101.AuthorKind != authorKindHuman || pr101.AuthorKindSource != authorKindSrcInfer || pr101.Tier != "contributor" || pr101.NudgeCount != 1 {
		t.Fatalf("pr101 author: kind=%q src=%q tier=%q nudges=%d", pr101.AuthorKind, pr101.AuthorKindSource, pr101.Tier, pr101.NudgeCount)
	}
	if len(pr101.History) != 3 || pr101.History[0].Number != 50 || pr101.History[2].Number != 72 || pr101.History[2].Reason != "reviewer-verdict:changes_requested" {
		t.Fatalf("pr101 history = %+v", pr101.History)
	}

	pr102 := trackerItem(t, resp, "pr", 102)
	if pr102.Stage != 100 || pr102.Lane != laneHive || pr102.AuthorKind != authorKindHive || pr102.AuthorKindSource != authorKindSrcLabel || pr102.Tier != "trusted" {
		t.Fatalf("pr102 = %+v", pr102)
	}
	if pr102.Caption != "In the merge queue; waiting for soak, tide or the merge window — soaking" || pr102.Actor != "tide" || len(pr102.Modifiers) != 0 {
		t.Fatalf("pr102 caption=%q actor=%q mods=%v", pr102.Caption, pr102.Actor, pr102.Modifiers)
	}

	pr103 := trackerItem(t, resp, "pr", 103)
	if pr103.Stage != 50 || pr103.RegressedFrom != 100 || !hasMod(pr103, modRegressed) || pr103.AuthorKind != authorKindClanker || pr103.Lane != laneContributor {
		t.Fatalf("pr103 = %+v", pr103)
	}

	pr104 := trackerItem(t, resp, "pr", 104)
	if pr104.Stage != 85 || pr104.StageSince != nil || len(pr104.History) != 0 || hasMod(pr104, modNeedsDecision) || pr104.Actor != "maintainer" {
		t.Fatalf("label-only pr104 = %+v", pr104)
	}

	pr105 := trackerItem(t, resp, "pr", 105)
	if pr105.Stage != 0 || !hasMod(pr105, modHold) || !hasMod(pr105, modDraft) || pr105.Caption != "No stage recorded yet" {
		t.Fatalf("held pr105 = %+v", pr105)
	}

	issue7 := trackerItem(t, resp, "issue", 7)
	if issue7.Stage != 100 || !issue7.Mirrored || issue7.LinkedPR != 102 || issue7.Lane != laneIssue || issue7.AuthorKind != authorKindHuman {
		t.Fatalf("issue7 = %+v", issue7)
	}
	if issue7.Caption != "Covered by PR #102 — merge queue on the PR side" {
		t.Fatalf("issue7 caption = %q", issue7.Caption)
	}
	// The covering PR is resolved onto the issue with what its sub-row
	// needs, and the PR itself is marked as nested under the issue.
	if len(issue7.LinkedPRs) != 1 || issue7.LinkedPRs[0].Number != 102 || issue7.LinkedPRs[0].Title != "Stabilise test" || issue7.LinkedPRs[0].Author != "hive-app[bot]" ||
		issue7.LinkedPRs[0].AuthorKind != authorKindHive || issue7.LinkedPRs[0].Stage != 100 || issue7.LinkedPRs[0].StageSince == nil || !issue7.LinkedPRs[0].StageSince.Equal(*pr102.StageSince) ||
		issue7.LinkedPRs[0].Caption != pr102.Caption || len(issue7.LinkedPRs[0].Modifiers) != 0 {
		t.Fatalf("issue7 linked PRs = %+v", issue7.LinkedPRs)
	}
	if pr102.NestedUnder != 7 || pr101.NestedUnder != 0 {
		t.Fatalf("nested_under: pr102=%d pr101=%d", pr102.NestedUnder, pr101.NestedUnder)
	}
	issue8 := trackerItem(t, resp, "issue", 8)
	if issue8.Stage != 10 || issue8.Mirrored || issue8.Actor != "scanner" || issue8.AuthorKind != authorKindHive {
		t.Fatalf("issue8 = %+v", issue8)
	}

	// Order: stage desc, then newest transition first.
	var order []int
	for _, it := range resp.Items {
		order = append(order, it.Number)
	}
	// 100: issue7 and pr102 share since; repo tie-break then number → 7, 102.
	want := []int{7, 102, 104, 101, 103, 8, 105}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestStageTrackerInitializing(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	s.handleStageTracker(rr, httptest.NewRequest(http.MethodGet, "/api/stage-tracker", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "{\"status\":\"initializing\"}\n" {
		t.Fatalf("got %d %q", rr.Code, rr.Body.String())
	}
}

func TestStageTrackerAuthorKindFallback(t *testing.T) {
	cases := []struct {
		labels            []string
		author            string
		app, attributed   bool
		wantKind, wantSrc string
	}{
		{[]string{"author/clanker", "author/human"}, "x", false, false, authorKindClanker, authorKindSrcLabel},
		{nil, "hive-app[bot]", true, false, authorKindHive, authorKindSrcInfer},
		{nil, "zoe", false, true, authorKindClanker, authorKindSrcInfer},
		{[]string{"agent/scanner"}, "someone", false, false, authorKindHive, authorKindSrcInfer},
		{nil, "dependabot[bot]", false, false, "", ""},
		{nil, "pat", false, false, authorKindHuman, authorKindSrcInfer},
		{nil, "", false, false, "", ""},
	}
	for _, c := range cases {
		k, src := stageTrackerAuthorKind(c.labels, c.author, c.app, c.attributed)
		if k != c.wantKind || src != c.wantSrc {
			t.Errorf("%v/%q/app=%v/attr=%v: got %q/%q want %q/%q", c.labels, c.author, c.app, c.attributed, k, src, c.wantKind, c.wantSrc)
		}
	}
}

func TestStageTrackerClaimantKind(t *testing.T) {
	s := newTestServer()
	if got := s.stageTrackerClaimantKind("acme/console", 1); got != "" {
		t.Fatalf("ledger off: got %q, want empty", got)
	}
	l, err := claims.New(filepath.Join(t.TempDir(), "claims.json"), claims.DefaultPolicy(), claims.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	s.deps = &Dependencies{IssueClaims: l}
	for issue, kind := range map[int]claims.Kind{1: claims.KindHuman, 2: claims.KindAgent, 3: claims.KindContributor, 4: claims.KindExternal} {
		if _, err := l.Claim(claims.Request{Repo: "acme/console", Issue: issue, Holder: "h" + strconv.Itoa(issue), Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	want := map[int]string{1: authorKindHuman, 2: authorKindHive, 3: authorKindClanker, 4: authorKindClanker, 5: ""}
	for issue, kind := range want {
		if got := s.stageTrackerClaimantKind("acme/console", issue); got != kind {
			t.Errorf("issue %d: got %q, want %q", issue, got, kind)
		}
	}
}

// TestStageTrackerIssueFeedPRsAreNotIssues pins the fix for "every PR renders
// twice as its own parent issue": the issues feed also lists pull requests,
// and a PR must never be an issue row, pair with itself or nest under itself.
func TestStageTrackerIssueFeedPRsAreNotIssues(t *testing.T) {
	now := time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	resp := s.buildStageTracker(status, store, now)
	for _, it := range resp.Items {
		if it.Kind == "issue" && (it.Number == 101 || it.Number == 106) {
			t.Fatalf("PR #%d rendered as an issue row: %+v", it.Number, it)
		}
		if it.Kind == "pr" && it.NestedUnder == it.Number {
			t.Fatalf("PR #%d nested under itself", it.Number)
		}
		for _, lp := range it.LinkedPRs {
			if lp.Number == it.Number {
				t.Fatalf("%s #%d links to itself", it.Kind, it.Number)
			}
		}
	}
	pr := trackerItem(t, resp, "pr", 101)
	if pr.NestedUnder != 0 || pr.AuthorKind == "" {
		t.Fatalf("PR #101 should be a standalone PR row with PR info: %+v", pr)
	}
	// Direct guard on the pairing: an issue whose reason names its own number.
	items := []StageTrackerItem{
		{Repo: "acme/console", Number: 9, Kind: "issue", LinkedPR: 9, LinkedPRs: []StageTrackerLinkedPR{{Number: 9}, {Number: 10}}},
		{Repo: "acme/console", Number: 10, Kind: "pr", Title: "Real PR"},
	}
	stageTrackerResolveLinkedPRs(items)
	if len(items[0].LinkedPRs) != 1 || items[0].LinkedPRs[0].Number != 10 || items[0].LinkedPRs[0].Title != "Real PR" {
		t.Fatalf("self-link not dropped: %+v", items[0].LinkedPRs)
	}
	if items[1].NestedUnder != 9 {
		t.Fatalf("PR #10 should nest under issue #9, got %d", items[1].NestedUnder)
	}
}
