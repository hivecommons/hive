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
		stagelabels.Actionable: trackerNumberActionable, stagelabels.TriageNeeded: trackerNumberTriageNeeded, stagelabels.Claimed: trackerNumberClaimed,
		stagelabels.PRInProgress: trackerNumberPRInProgress, stagelabels.CoveredByPR: trackerNumberCoveredByPR,
		stagelabels.IssueNeedsDecision: trackerNumberIssueDecision,
		stagelabels.ReviewPending:      trackerNumberReviewPending, stagelabels.Reviewed: trackerNumberReviewed,
		stagelabels.Fixing: trackerNumberFixing, stagelabels.NeedsAuthor: trackerNumberNeedsAuthor, stagelabels.ReturnedToContributor: trackerNumberReturned,
		stagelabels.AdoptedByHive: trackerNumberAdopted, stagelabels.AwaitingMergeAdmission: trackerNumberAwaitingMerge,
		stagelabels.NeedsDecision: trackerNumberNeedsDecision, stagelabels.ReadyForHumanMerge: trackerNumberReadyForHuman,
		stagelabels.PreMergeSoak: trackerNumberPreMergeSoak,
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
	if mains != 12 || detours != 6 {
		t.Fatalf("mains=%d detours=%d, want 12 main segments (015 and 017 share the Actionable column; 045 is the issue parking bay; 095 shares the merge gate column) and 6 detours", mains, detours)
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
		caption, _ := stageTrackerCaption(tc.stage, "check-failing:unit", "sam", "pr", false, stagelabels.Record{}, false, 0, "")
		if !strings.HasPrefix(caption, tc.captionHead) {
			t.Errorf("%v: caption = %q", tc.stage, caption)
		}
	}
	caption, actor := stageTrackerCaption(stagelabels.NeedsAuthor, "check-failing:unit", "sam", "pr", false, stagelabels.Record{}, false, 0, "")
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
		"base-branch-red:docker (hive),docker (hub)":    "waiting for the base branch's CI to recover (docker (hive), docker (hub)) — not this PR's fault",
		"shared-ci-incident:13208":                      "deferred to shared-CI incident #13208 — not this PR's fault",
		"fork-guard:Canary":                             "check Canary refuses fork PRs — a maintainer must push this branch to the repository",
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
	// PR 102: hive PR admitted and soaking, fresh (12m left as of a sweep 5m ago).
	clock = now.Add(-40 * time.Minute)
	store.Observe(stagelabels.Key("acme/console", 102), stagelabels.PreMergeSoak, "contributor-lane:soaking(remaining=17m0s)")
	clock = now.Add(-5 * time.Minute)
	store.Observe(stagelabels.Key("acme/console", 102), stagelabels.PreMergeSoak, "contributor-lane:soaking(remaining=12m0s)")
	// PR 103: regressed from the merge queue to review-pending.
	key103 := stagelabels.Key("acme/console", 103)
	clock = now.Add(-3 * time.Hour)
	store.Observe(key103, stagelabels.MergeQueue, "contributor-lane:soaking")
	clock = now.Add(-10 * time.Minute)
	store.Observe(key103, stagelabels.ReviewPending, "contributor-lane:no-verdict")
	// Issue 7: mirrors PR 102.
	clock = now.Add(-40 * time.Minute)
	store.Observe(stagelabels.Key("acme/console", 7), stagelabels.PreMergeSoak, "mirrors PR #102")
	clock = now

	status := minimalPayload()
	status.HiveID = "hv-test"
	status.Repos = []FrontendRepo{{
		Name: "console", Full: "acme/console",
		ActionableIssues: []any{
			github.Issue{Number: 7, Title: "Login redirect loops", Author: "karibhou", AuthorIsHuman: true, Labels: []string{"stage/095-pre-merge-soak"}},
			github.Issue{Number: 8, Title: "Flaky test", Author: "hive-app[bot]", Labels: []string{"stage/010-actionable", "agent/scanner"}},
			// Parked on a person: the issue parking bay 045 (owner decision:
			// issues never wear 085), with needs-human beside it.
			github.Issue{Number: 13, Title: "Agents stand down", Author: "pat", AuthorIsHuman: true, Labels: []string{"stage/045-needs-decision", "needs-human"}},
			// GitHub's issues feed returns pull requests too. These are copies
			// of PRs 101 (also in OpenPrs) and 106 (URL says /pull/, not in the
			// PR set); neither may become an issue row.
			github.Issue{Number: 101, Title: "Fix redirect", Author: "karibhou", AuthorIsHuman: true, Labels: []string{"stage/072-returned-to-contributor"}},
			github.Issue{Number: 106, Title: "Closed-feed PR", Author: "pat", URL: "https://github.com/acme/console/pull/106", Labels: []string{"stage/050-review-pending"}},
		},
		OpenPrs: []any{
			// 101's body says `Fixes #90`, an issue already closed: no open
			// issue to pair with, so it stays a PR row naming #90 as closed.
			FrontendPR{PullRequest: github.PullRequest{Number: 101, Title: "Fix redirect", Author: "karibhou", Labels: []string{"stage/072-returned-to-contributor", "author/contributor", "needs-rebase"},
				LinkedIssues: []github.PRLinkedIssue{{Number: 90, State: "closed"}}}},
			FrontendPR{PullRequest: github.PullRequest{Number: 102, Title: "Stabilise test", Author: "hive-app[bot]", AppAuthored: true, Labels: []string{"stage/095-pre-merge-soak", "author/trusted", "author/hive"}}, MergeVerdict: &github.MergeVerdict{Tier: "trusted"}},
			// 103's body says `Fixes #8` (GitHub's closingIssuesReferences);
			// issue 8's own claim evidence has not caught up, so the PR side
			// alone pairs them. A self-reference and a foreign repo are dropped.
			FrontendPR{PullRequest: github.PullRequest{Number: 103, Title: "Relay PR", Author: "zoe", HiveAttributed: true, Labels: []string{"stage/050-review-pending"},
				LinkedIssues: []github.PRLinkedIssue{{Number: 8}, {Number: 103}, {Number: 8}, {Number: 5, Repo: "acme/other"}}}},
			// Label only: the store never saw it.
			FrontendPR{PullRequest: github.PullRequest{Number: 104, Title: "Hand-made", Author: "pat", Labels: []string{"stage/085-needs-decision", "needs-decision"}}},
		},
		HeldPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 105, Title: "Parked", Author: "pat", Labels: []string{"do-not-merge/blocked-paths"}, Draft: true}},
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
	if len(live.Items) != 8 {
		t.Fatalf("handler items = %d, want 8", len(live.Items))
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
	if len(resp.Items) != 8 {
		t.Fatalf("items = %d, want 8 (3 issues + 4 open PRs + 1 held PR; issue-feed PR copies dropped)", len(resp.Items))
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
	if pr102.Stage != 95 || pr102.Lane != laneHive || pr102.AuthorKind != authorKindHive || pr102.AuthorKindSource != authorKindSrcLabel || pr102.Tier != "trusted" {
		t.Fatalf("pr102 = %+v", pr102)
	}
	if pr102.Caption != "Admitted by the automerge lane; the pre-merge soak window is running — soaking, 12m0s left at the last sweep" || pr102.Actor != "automerge lane" || len(pr102.Modifiers) != 0 {
		t.Fatalf("pr102 caption=%q actor=%q mods=%v", pr102.Caption, pr102.Actor, pr102.Modifiers)
	}
	// 12m left when the sweep wrote it 5m ago → 7m now; the 40m-old entry
	// time is the stage_since, not the soak clock.
	if pr102.SoakRemainingSeconds != 7*60 || pr102.StageSince == nil || !pr102.StageSince.Equal(now.Add(-40*time.Minute)) {
		t.Fatalf("pr102 soak_remaining_seconds=%d since=%v", pr102.SoakRemainingSeconds, pr102.StageSince)
	}

	pr103 := trackerItem(t, resp, "pr", 103)
	if pr103.Stage != 50 || pr103.RegressedFrom != 100 || !hasMod(pr103, modRegressed) || pr103.AuthorKind != authorKindClanker || pr103.Lane != laneContributor {
		t.Fatalf("pr103 = %+v", pr103)
	}
	if pr103.RegressedTo != 50 || pr103.RegressedReason != "contributor-lane:no-verdict" || pr103.RegressedAt == nil || !pr103.RegressedAt.Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("pr103 regression detail to=%d reason=%q at=%v, want the 100 → 050 hop", pr103.RegressedTo, pr103.RegressedReason, pr103.RegressedAt)
	}
	if h := pr103.History; len(h) != 2 || h[1].From != 100 || !h[1].Regressed || h[0].Regressed {
		t.Fatalf("pr103 history = %+v, want the second hop marked as the 100 → 050 fall", h)
	}

	pr104 := trackerItem(t, resp, "pr", 104)
	if pr104.Stage != 85 || pr104.StageSince != nil || len(pr104.History) != 0 || hasMod(pr104, modNeedsDecision) || pr104.Actor != "maintainer" {
		t.Fatalf("label-only pr104 = %+v", pr104)
	}

	pr105 := trackerItem(t, resp, "pr", 105)
	if pr105.Stage != 0 || !hasMod(pr105, modHold) || !hasMod(pr105, modDraft) || pr105.Caption != "No stage recorded yet" || !slices.Contains(pr105.Labels, "do-not-merge/blocked-paths") {
		t.Fatalf("held pr105 = %+v", pr105)
	}

	issue13 := trackerItem(t, resp, "issue", 13)
	if issue13.Stage != trackerNumberIssueDecision || issue13.Mirrored || issue13.Actor != "maintainer" || hasMod(issue13, modNeedsDecision) || !hasMod(issue13, modNeedsHuman) {
		t.Fatalf("parked issue13 = %+v", issue13)
	}

	issue7 := trackerItem(t, resp, "issue", 7)
	if issue7.Stage != 95 || !issue7.Mirrored || issue7.LinkedPR != 102 || issue7.Lane != laneIssue || issue7.AuthorKind != authorKindHuman {
		t.Fatalf("issue7 = %+v", issue7)
	}
	if issue7.Caption != "Covered by PR #102 — soaking on the PR side" {
		t.Fatalf("issue7 caption = %q", issue7.Caption)
	}
	// The covering PR is resolved onto the issue with what its sub-row
	// needs, and the PR itself is marked as nested under the issue.
	if len(issue7.LinkedPRs) != 1 || issue7.LinkedPRs[0].Number != 102 || issue7.LinkedPRs[0].Title != "Stabilise test" || issue7.LinkedPRs[0].Author != "hive-app[bot]" ||
		issue7.LinkedPRs[0].AuthorKind != authorKindHive || issue7.LinkedPRs[0].Stage != 95 || issue7.LinkedPRs[0].StageSince == nil || !issue7.LinkedPRs[0].StageSince.Equal(*pr102.StageSince) ||
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
	// 095: issue7 and pr102 share since; repo tie-break then number → 7, 102.
	want := []int{7, 102, 104, 101, 103, 13, 8, 105}
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

// TestStageTrackerSupersededPRDropped pins hivecommons/hive#14471: a PR whose
// adoption merged and was told so is not a row, even while GitHub still
// lists it open (the close lagged, or the App could not close a fork PR).
func TestStageTrackerSupersededPRDropped(t *testing.T) {
	now := time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	trackerItem(t, s.buildStageTracker(status, store, now), "pr", 101)
	key := stagelabels.Key("acme/console", 101)
	store.MarkAdopted(key, "abc123", 150, now)
	store.MarkSuperseded(key, 150, true, now)
	for _, it := range s.buildStageTracker(status, store, now).Items {
		if it.Kind == "pr" && it.Number == 101 {
			t.Fatalf("superseded PR #101 still a row: %+v", it)
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

// hivecommons/hive#13012: a held issue is "Open · on hold" in the tracker
// whatever stage label or store record the hold caught it with — #9846
// wore stage/045-needs-decision beside `hold` + `needs-human` and rendered
// as "Needs a human decision". needs-human without a hold still parks at
// 045, and a held PR keeps its own stage.
func TestStageTrackerHeldIssueIsOpenOnHold(t *testing.T) {
	now := time.Date(2026, 10, 19, 17, 0, 0, 0, time.UTC)
	s := newTestServer()
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store
	clock = now.Add(-72 * time.Hour)
	store.Observe(stagelabels.Key("acme/console", 9846), stagelabels.IssueNeedsDecision, "held for a person: needs-human")
	clock = now

	status := minimalPayload()
	status.HiveID = "hv-test"
	status.Repos = []FrontendRepo{{
		Name: "console", Full: "acme/console",
		ActionableIssues: []any{
			github.Issue{Number: 12964, Title: "Plain issue", Author: "pat", AuthorIsHuman: true, Labels: []string{"help wanted", "stage/010-actionable"}},
			github.Issue{Number: 13, Title: "Parked", Author: "pat", AuthorIsHuman: true, Labels: []string{"stage/045-needs-decision", "needs-human"}},
		},
		HeldIssues: []any{
			github.Issue{Number: 9846, Title: "Held with a stale park", Author: "pat", AuthorIsHuman: true, Labels: []string{"hold", "needs-human", "stage/045-needs-decision"}},
			github.Issue{Number: 9735, Title: "Held plainly", Author: "pat", AuthorIsHuman: true, Labels: []string{"hold", "needs-human"}},
			github.Issue{Number: 9700, Title: "Paused by this hive", Author: "pat", AuthorIsHuman: true, Labels: []string{"hive-pause/hv-test"}},
			github.Issue{Number: 9699, Title: "Prow hold", Author: "pat", AuthorIsHuman: true, Labels: []string{"do-not-merge/hold"}},
		},
		HeldPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 105, Title: "Parked PR", Author: "pat", Labels: []string{"hold", "stage/085-needs-decision"}}},
		},
	}}
	s.UpdateStatus(status)
	resp := s.buildStageTracker(s.status, store, now)

	for _, n := range []int{9846, 9735, 9700, 9699} {
		it := trackerItem(t, resp, "issue", n)
		if it.Stage != 0 || it.StageSlug != "" || it.StageSince != nil {
			t.Errorf("held issue #%d = stage %d (%q, since %v), want Open with no record", n, it.Stage, it.StageSlug, it.StageSince)
		}
		if !hasMod(it, modHold) {
			t.Errorf("held issue #%d modifiers %v, want hold", n, it.Modifiers)
		}
	}
	if it := trackerItem(t, resp, "issue", 13); it.Stage != 45 || hasMod(it, modHold) {
		t.Errorf("needs-human without a hold = stage %d mods %v, want 045 unheld", it.Stage, it.Modifiers)
	}
	if it := trackerItem(t, resp, "issue", 12964); it.Stage != 10 || len(it.Modifiers) != 0 {
		t.Errorf("#12964 = stage %d mods %v, want 010 unchanged", it.Stage, it.Modifiers)
	}
	if it := trackerItem(t, resp, "pr", 105); it.Stage != 85 || !hasMod(it, modHold) {
		t.Errorf("held PR = stage %d mods %v, want 085 + hold (PR path untouched)", it.Stage, it.Modifiers)
	}
}

// A PR the hive adopted in place keeps the 074 label whatever the head
// needs, so the caption is where the work shows: rebasing, fixing CI, or
// addressing review findings.
func TestStageTrackerAdoptedCaptionNamesTheWork(t *testing.T) {
	cases := map[string]string{
		"conflicting":                        "Hive adopted this PR; hive is rebasing it onto the base branch — conflicting",
		"label:needs-rebase":                 "Hive adopted this PR; hive is rebasing it onto the base branch — needs a rebase",
		"check-failing:coverage":             "Hive adopted this PR; hive is fixing CI — CI is red (check-failing:coverage)",
		"required-check-failing:test":        "Hive adopted this PR; hive is fixing CI — check test failing",
		"reviewer-verdict:changes_requested": "Hive adopted this PR; hive is addressing the review findings — review swarm requested changes",
		"":                                   "Hive adopted this PR",
	}
	for reason, want := range cases {
		caption, actor := stageTrackerCaption(stagelabels.AdoptedByHive, reason, "arteeh", "pr", false, stagelabels.Record{}, false, 0, "")
		if caption != want || actor != "hive" {
			t.Errorf("074 reason %q: caption = %q actor = %q, want %q / hive", reason, caption, actor, want)
		}
	}
}

func TestStageTrackerCaptionNamesHeadAwaitingVerdict(t *testing.T) {
	caption, actor := stageTrackerCaption(stagelabels.ReviewPending, "contributor-lane:no-verdict", "mendezr", "pr", false, stagelabels.Record{}, false, 0, "98489e7ba141fc60c30c9aaf47774dc42f9b1c76")
	if !strings.HasSuffix(caption, "awaiting review of head 98489e7") || actor != "review swarm" {
		t.Fatalf("caption = %q actor = %q", caption, actor)
	}
	caption, _ = stageTrackerCaption(stagelabels.ReviewPending, "contributor-lane:no-verdict", "mendezr", "pr", false, stagelabels.Record{}, false, 0, "")
	if !strings.HasSuffix(caption, "no review verdict yet") {
		t.Fatalf("caption without head = %q", caption)
	}
}

// Owner invariant (hivecommons/hive activity monitor, 2026-10-21): a PR
// row always has its issue on the left and the PR on the right. The payload
// pairs from the PR side too — GitHub's closingIssuesReferences, i.e. the
// `Fixes/Closes/Resolves #N` keywords in the PR body — so a PR whose open
// issue is listed nests under it even before the issue's claim evidence
// names the PR, and a PR with no open issue still carries what its issue
// slot shows (the closed issue it names, or nothing).
func TestStageTrackerPRSidePairing(t *testing.T) {
	now := time.Date(2026, 10, 21, 18, 49, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	resp := s.buildStageTracker(status, store, now)

	pr103 := trackerItem(t, resp, "pr", 103)
	if pr103.NestedUnder != 8 {
		t.Fatalf("PR #103 (Fixes #8) nested_under = %d, want 8", pr103.NestedUnder)
	}
	if len(pr103.LinkedIssues) != 1 || pr103.LinkedIssues[0] != (StageTrackerLinkedIssue{Number: 8, Title: "Flaky test", URL: "https://github.com/acme/console/issues/8", State: "open", Listed: true}) {
		t.Fatalf("PR #103 linked_issues = %+v", pr103.LinkedIssues)
	}
	issue8 := trackerItem(t, resp, "issue", 8)
	if len(issue8.LinkedPRs) != 1 || issue8.LinkedPRs[0].Number != 103 || issue8.LinkedPRs[0].Title != "Relay PR" || issue8.LinkedPRs[0].Stage != 50 || issue8.LinkedPRs[0].URL != pr103.URL {
		t.Fatalf("issue #8 linked_prs = %+v", issue8.LinkedPRs)
	}

	pr101 := trackerItem(t, resp, "pr", 101)
	if pr101.NestedUnder != 0 || len(pr101.LinkedIssues) != 1 || pr101.LinkedIssues[0] != (StageTrackerLinkedIssue{Number: 90, URL: "https://github.com/acme/console/issues/90", State: "closed"}) {
		t.Fatalf("PR #101 (Fixes #90, closed) = nested %d, linked_issues %+v", pr101.NestedUnder, pr101.LinkedIssues)
	}
	// The issue→PR pass also names the issue on the PR's side: issue 7's
	// stage reason covers PR 102, which carries no closing reference.
	pr102 := trackerItem(t, resp, "pr", 102)
	if pr102.NestedUnder != 7 || len(pr102.LinkedIssues) != 1 || !pr102.LinkedIssues[0].Listed || pr102.LinkedIssues[0].Number != 7 || pr102.LinkedIssues[0].Title == "" {
		t.Fatalf("PR #102 linked_issues = %+v (nested %d)", pr102.LinkedIssues, pr102.NestedUnder)
	}
	if pr104 := trackerItem(t, resp, "pr", 104); len(pr104.LinkedIssues) != 0 || pr104.NestedUnder != 0 {
		t.Fatalf("PR #104 with no closing reference = %+v", pr104.LinkedIssues)
	}
}

func TestStageTrackerPRLinkedIssues(t *testing.T) {
	pr := github.PullRequest{Repo: "acme/console", Number: 42}
	tests := []struct {
		name   string
		linked []github.PRLinkedIssue
		want   []StageTrackerLinkedIssue
	}{
		{"none", nil, nil},
		{"same repo bare", []github.PRLinkedIssue{{Number: 7}}, []StageTrackerLinkedIssue{{Number: 7, URL: "https://github.com/acme/console/issues/7"}}},
		{"explicit same repo, any case, state lower-cased", []github.PRLinkedIssue{{Number: 7, Repo: "Acme/Console", State: "CLOSED", URL: "https://gh/7"}}, []StageTrackerLinkedIssue{{Number: 7, URL: "https://gh/7", State: "closed"}}},
		{"foreign repo dropped", []github.PRLinkedIssue{{Number: 7, Repo: "acme/other"}}, nil},
		{"self and non-positive dropped", []github.PRLinkedIssue{{Number: 42}, {Number: 0}, {Number: -1}}, nil},
		{"multiple, deduplicated, order kept", []github.PRLinkedIssue{{Number: 9}, {Number: 7}, {Number: 9}}, []StageTrackerLinkedIssue{{Number: 9, URL: "https://github.com/acme/console/issues/9"}, {Number: 7, URL: "https://github.com/acme/console/issues/7"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr.LinkedIssues = tt.linked
			got := stageTrackerPRLinkedIssues(pr, "")
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("[%d] got %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// A closed issue is never a tracker row, whatever stage label it closed
// wearing (hivecommons/homebrew-hive#127); and a 045 store record whose
// issue has lost every park label — a `/hive decision` the engine has not
// yet re-derived — no longer reads "Needs human decision"
// (hivecommons/docs#419). A record the labels still back is unchanged.
func TestStageTrackerDecisionStateFollowsCurrentLabels(t *testing.T) {
	now := time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)
	s := newTestServer()
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store
	clock = now.Add(-72 * time.Hour)
	store.Observe(stagelabels.Key("acme/console", 127), stagelabels.IssueNeedsDecision, "parked: needs a maintainer")
	store.Observe(stagelabels.Key("acme/console", 419), stagelabels.IssueNeedsDecision, "parked: needs a maintainer")
	store.Observe(stagelabels.Key("acme/console", 459), stagelabels.IssueNeedsDecision, "parked: needs a maintainer")
	store.Observe(stagelabels.Key("acme/console", 460), stagelabels.IssueNeedsDecision, "parked: needs a maintainer")
	clock = now

	status := minimalPayload()
	status.HiveID = "hv-test"
	status.Repos = []FrontendRepo{{
		Name: "console", Full: "acme/console",
		ActionableIssues: []any{
			github.Issue{Number: 127, Title: "Closed with a stale park", Author: "pat", AuthorIsHuman: true, State: "closed", Labels: []string{"stage/045-needs-decision", "needs-human"}},
			github.Issue{Number: 419, Title: "Decided, record not yet re-derived", Author: "pat", AuthorIsHuman: true, State: "open", Labels: []string{"approved-direction"}},
			github.Issue{Number: 459, Title: "Still parked", Author: "pat", AuthorIsHuman: true, Labels: []string{"stage/045-needs-decision"}},
			github.Issue{Number: 460, Title: "needs-human without the stage label", Author: "pat", AuthorIsHuman: true, Labels: []string{"needs-human"}},
		},
	}}
	s.UpdateStatus(status)
	resp := s.buildStageTracker(s.status, store, now)

	for _, it := range resp.Items {
		if it.Kind == "issue" && it.Number == 127 {
			t.Errorf("closed issue #127 rendered at stage %d; closed issues are never tracker rows", it.Stage)
		}
	}
	if it := trackerItem(t, resp, "issue", 419); it.Stage == 45 || it.StageSince != nil || hasMod(it, modNeedsDecision) {
		t.Errorf("decided issue #419 = stage %d since %v mods %v, want no needs-decision state from a stale record", it.Stage, it.StageSince, it.Modifiers)
	}
	if it := trackerItem(t, resp, "issue", 459); it.Stage != 45 || it.StageSince == nil {
		t.Errorf("parked issue #459 = stage %d since %v, want 045 from the record", it.Stage, it.StageSince)
	}
	if it := trackerItem(t, resp, "issue", 460); it.Stage != 45 || !hasMod(it, modNeedsHuman) {
		t.Errorf("needs-human issue #460 = stage %d mods %v, want 045 kept (label still backs the record)", it.Stage, it.Modifiers)
	}
}

func TestStageTrackerStaleDecisionRecord(t *testing.T) {
	cases := []struct {
		name   string
		rec    stagelabels.Record
		labels []string
		want   bool
	}{
		{"045 record, no park labels", stagelabels.Record{Stage: stagelabels.IssueNeedsDecision}, []string{"approved-direction"}, true},
		{"085 record, no park labels", stagelabels.Record{Stage: stagelabels.NeedsDecision}, nil, true},
		{"045 record backed by stage label", stagelabels.Record{Stage: stagelabels.IssueNeedsDecision}, []string{"stage/045-needs-decision"}, false},
		{"045 record backed by needs-human", stagelabels.Record{Stage: stagelabels.IssueNeedsDecision}, []string{"Needs-Human"}, false},
		{"045 record backed by retired spelling", stagelabels.Record{Stage: stagelabels.IssueNeedsDecision}, []string{"needs-decision"}, false},
		{"085 record backed by 085 label", stagelabels.Record{Stage: stagelabels.NeedsDecision}, []string{"stage/085-needs-decision"}, false},
		{"other stage, no labels", stagelabels.Record{Stage: stagelabels.Claimed}, nil, false},
	}
	for _, tc := range cases {
		if got := stageTrackerStaleDecisionRecord(tc.rec, tc.labels); got != tc.want {
			t.Errorf("%s: stale = %t, want %t", tc.name, got, tc.want)
		}
	}
}
