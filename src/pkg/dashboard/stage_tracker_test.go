package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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

// TestStageTrackerStagesFallsTo: each stage row carries the lower same-kind
// stages a move to which is a regression, so the SPA's event line says
// "fell back to" only for those — not for triage accepted (015 → 010), a
// decision made (085 → 080) or the author resuming (072 → 070)
// (hivecommons/hive#15198).
func TestStageTrackerStagesFallsTo(t *testing.T) {
	byNum := map[int]StageTrackerStage{}
	for _, st := range stageTrackerStages(nil) {
		byNum[st.Number] = st
	}
	for _, tc := range []struct {
		from, to int
		fall     bool
	}{
		{trackerNumberTriageNeeded, trackerNumberActionable, false},
		{trackerNumberPossiblyExists, trackerNumberActionable, false},
		{trackerNumberIssueDecision, trackerNumberActionable, false},
		{trackerNumberNeedsDecision, trackerNumberAwaitingMerge, false},
		{trackerNumberReturned, trackerNumberFixing, false},
		{trackerNumberFixing, trackerNumberReviewPending, false},
		{trackerNumberCoveredByPR, trackerNumberActionable, true},
		{trackerNumberCoveredByPR, trackerNumberTriageNeeded, true},
		{trackerNumberReviewed, trackerNumberReviewPending, true},
		{trackerNumberAwaitingMerge, trackerNumberFixing, true},
		{trackerNumberReviewPending, trackerNumberCoveredByPR, false}, // PR stages never fall to issue stages
	} {
		if got := slices.Contains(byNum[tc.from].FallsTo, tc.to); got != tc.fall {
			t.Errorf("%03d → %03d: falls_to lists it = %v, want %v (%v)", tc.from, tc.to, got, tc.fall, byNum[tc.from].FallsTo)
		}
	}
	if len(byNum[trackerNumberActionable].FallsTo) != 0 {
		t.Errorf("010 has nothing lower to fall to: %v", byNum[trackerNumberActionable].FallsTo)
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
	if caption != "Needs @sam — CI failed: unit — @sam must act" || actor != "sam" {
		t.Errorf("071 caption = %q actor = %q", caption, actor)
	}
}

func TestHumanizeStageReason(t *testing.T) {
	cases := map[string]string{
		"actionable":                            "",
		"live agent claim":                      "",
		"mirrors PR #12332":                     "mirrors PR #12332",
		"label:needs-rebase":                    "needs a rebase",
		"held":                                  "held by a hold label",
		"contributor-lane:no-verdict":           "no review verdict yet",
		"required-check-pending:build-and-test": "check build-and-test still running",
		// Pending is not red (hivecommons/docs#512 read "CI is red (check-pending)").
		"check-pending":                      "CI running",
		"status-pending":                     "CI running",
		"check-success":                      "CI green",
		"status-success":                     "CI green",
		"check-failure":                      "CI failed",
		"status-error":                       "CI failed",
		"check-timed_out":                    "CI failed (timed out)",
		"check-failing:lint":                 "CI failed: lint",
		"status-failing:tests":               "CI failed: tests",
		"ci-red":                             "CI failed",
		"check-runs":                         "CI checks could not be read",
		"required-check-failing:DCO":         "check DCO failing",
		"reviewer-verdict:changes_requested": "review swarm requested changes",
		"pending: head pushed 59s ago (< min_head_age)":              "head pushed 59s ago; waiting for the minimum head age",
		"contributor-lane:soaking:12m":                               "soaking",
		"excluded-label:do-not-merge":                                "label do-not-merge excludes it",
		"some-new-token":                                             "some new token",
		"base-branch-red:docker (hive),docker (hub)":                 "waiting for the base branch's CI to recover (docker (hive), docker (hub)) — not this PR's fault",
		"shared-ci-incident:13208":                                   "deferred to shared-CI incident #13208 — not this PR's fault",
		"fork-guard:Canary":                                          "check Canary refuses fork PRs — a maintainer must push this branch to the repository",
		"contributor-lane:workflow-path":                             "workflow path",
		"contributor-lane:tests-removed":                             "tests removed",
		"human-merge-path":                                           "human-merge path",
		"human-merge-required":                                       "human merge",
		"contributor-lane:workflow-path:human-approved@clubanderson": "workflow path — approved by @clubanderson",
		"reviewer-verdict:requires_human:human-approved@octocat":     "review verdict requires human — approved by @octocat",
	}
	for in, want := range cases {
		if got := humanizeStageReason(in); got != want {
			t.Errorf("humanizeStageReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestStageTrackerManualMergeApproved: a 090 record under a manual-merge
// lane rule the trusted merger approved (reason suffixed
// `:human-approved@login`) renders "Awaiting human merge (workflow path) —
// approved by @login" with the one step left, and the payload carries the
// approver and manual_merge so the lit chevron can say the same; a
// trust-blocked 090 and an unapproved rule are unchanged.
func TestStageTrackerManualMergeApproved(t *testing.T) {
	tests := []struct {
		name          string
		stage         stagelabels.Stage
		reason        string
		wantCaption   string
		wantApprover  string
		wantManual    bool
		wantActor     string
		wantNoCaption string
	}{
		{
			name: "workflow path approved", stage: stagelabels.ReadyForHumanMerge, reason: "contributor-lane:workflow-path:human-approved@clubanderson",
			wantCaption:  "Ready to merge (workflow path) — approved by @clubanderson — a person merges it",
			wantApprover: "clubanderson", wantManual: true, wantActor: "maintainer",
		},
		{
			name: "tests removed, approver unknown", stage: stagelabels.ReadyForHumanMerge, reason: "contributor-lane:tests-removed",
			wantCaption: "Ready to merge (tests removed) — a person merges it",
			wantManual:  true, wantActor: "maintainer",
		},
		{
			name: "breaking change parks at 090 for the owner", stage: stagelabels.ReadyForHumanMerge, reason: "human-merge-required",
			wantCaption: "Ready for human merge (breaking change — owner merges)", wantManual: true, wantActor: "maintainer", wantNoCaption: "Ready to merge",
		},
		{
			name: "breaking change approved still waits for the owner", stage: stagelabels.ReadyForHumanMerge, reason: "human-merge-required:human-approved@clubanderson",
			wantCaption:  "Ready for human merge (breaking change — owner merges) — approved by @clubanderson",
			wantApprover: "clubanderson", wantManual: true, wantActor: "maintainer",
		},
		{
			name: "trust-blocked 090 unchanged", stage: stagelabels.ReadyForHumanMerge, reason: "untrusted-author-role",
			wantCaption: "Ready to merge — a maintainer merges it — author's role is not trusted for unattended merge", wantActor: "maintainer",
		},
		{
			name: "requires_human approved at 090", stage: stagelabels.ReadyForHumanMerge, reason: "reviewer-verdict:requires_human:human-approved@octocat",
			wantCaption:  "Ready to merge — a maintainer merges it — review verdict requires human — approved by @octocat",
			wantApprover: "octocat", wantActor: "maintainer",
		},
		{
			name: "workflow path still at 085", stage: stagelabels.NeedsDecision, reason: "contributor-lane:workflow-path",
			wantCaption: "A human decision is owed — workflow path", wantManual: true, wantActor: "maintainer", wantNoCaption: "approved",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caption, actor := stageTrackerCaption(tt.stage, tt.reason, "pat", "pr", false, stagelabels.Record{}, false, 0, "")
			if caption != tt.wantCaption || actor != tt.wantActor {
				t.Fatalf("caption = %q actor %q, want %q %q", caption, actor, tt.wantCaption, tt.wantActor)
			}
			if tt.wantNoCaption != "" && strings.Contains(caption, tt.wantNoCaption) {
				t.Fatalf("caption %q must not say %q", caption, tt.wantNoCaption)
			}
			base, approver := stagelabels.HumanApprovedReason(tt.reason)
			if approver != tt.wantApprover || stageTrackerManualMergeReason(base) != tt.wantManual {
				t.Fatalf("approver %q manual %v, want %q %v", approver, stageTrackerManualMergeReason(base), tt.wantApprover, tt.wantManual)
			}
		})
	}

	// End to end through the payload: the store record's reason fills
	// human_approver / manual_merge on the row.
	now := time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	store.Observe(stagelabels.Key("acme/console", 120), stagelabels.ReadyForHumanMerge, "contributor-lane:workflow-path:human-approved@clubanderson")
	s.statusMu.Lock()
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		FrontendPR{PullRequest: github.PullRequest{Number: 120, Title: "Shard nightly", Author: "pat", Labels: []string{"stage/090-ready-for-human-merge", "lgtm"}}})
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)
	it := trackerItem(t, resp, "pr", 120)
	if it.Stage != 90 || it.HumanApprover != "clubanderson" || !it.ManualMerge || !strings.HasPrefix(it.Caption, "Ready to merge (workflow path) — approved by @clubanderson") {
		t.Fatalf("row = stage %d approver %q manual %v caption %q", it.Stage, it.HumanApprover, it.ManualMerge, it.Caption)
	}
	raw, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"human_approver":"clubanderson","manual_merge":true,"human_merge_reason":"workflow-files","human_merge_reason_text":"It changes GitHub workflow files`) {
		t.Fatalf("payload lacks the manual-merge / human-merge-reason fields: %s", raw)
	}
	if clean := trackerItem(t, resp, "pr", 101); clean.HumanApprover != "" || clean.ManualMerge || clean.HumanMergeReason != "" || clean.HumanMergeReasonText != "" {
		t.Fatalf("PR 101 must not carry manual-merge fields: %+v", clean)
	}
}

// TestStageTrackerHumanMergeReason: every 090 row names WHY a person and
// not the hive merges it (hivecommons/hive#14658) — a compact code from the
// lane reason already on the stage record plus a one-sentence tooltip; an
// unrecognised lane reason falls back to "policy" quoting it; a PR nested
// under its issue mirrors the fields onto the linked_prs entry; rows off
// 090 carry nothing.
func TestStageTrackerHumanMergeReason(t *testing.T) {
	tests := []struct {
		reason, wantCode, wantWord, wantTipPrefix string
	}{
		{"contributor-lane:workflow-path:human-approved@clubanderson", "workflow-files", "workflow files", "It changes GitHub workflow files"},
		{"contributor-lane:tests-removed", "tests-removed", "tests removed", "It removes tests"},
		{"human-merge-path:human-approved@octocat", "manual-merge-policy", "manual-merge policy", "It touches files this repo reserves"},
		{"untrusted-author-role", "untrusted-author", "untrusted author", "Hive won't merge it alone"},
		{"contributor-lane:claim-tier-untrusted", "untrusted-author", "untrusted author", "Hive won't merge it alone"},
		{"repo-auto-merge-disabled", "auto-merge-off", "auto-merge off", "Auto-merge is off for this repo"},
		{"reviewer-verdict:requires_human:human-approved@octocat", "reviewer-deferred", "reviewer deferred", "A reviewer asked for a person to merge"},
		{"human-merge-required", "breaking-change", "human merge", "Breaking change — owner merges"},
		{"something-brand-new", "policy", "policy", "Hive's merge rules leave this one to a person (`something-brand-new`)"},
		{"", "", "", ""},
	}
	for _, tt := range tests {
		code, word, tip := stageTrackerHumanMergeReason(tt.reason)
		if code != tt.wantCode || word != tt.wantWord || !strings.HasPrefix(tip, tt.wantTipPrefix) || (tt.wantTipPrefix == "" && tip != "") {
			t.Fatalf("%q → code %q word %q tip %q; want %q %q prefix %q", tt.reason, code, word, tip, tt.wantCode, tt.wantWord, tt.wantTipPrefix)
		}
	}

	// Through the payload: a 090 PR whose body says `Fixes #400` nests under
	// the open issue and the sub-row carries the same reason, approver and
	// manual_merge; the issue line mirrors the PR's stage.
	now := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	store.Observe(stagelabels.Key("acme/console", 401), stagelabels.ReadyForHumanMerge, "contributor-lane:workflow-path:human-approved@clubanderson")
	store.Observe(stagelabels.Key("acme/console", 402), stagelabels.ReadyForHumanMerge, "no-such-lane-reason")
	s.statusMu.Lock()
	s.status.Repos[0].ActionableIssues = append(s.status.Repos[0].ActionableIssues,
		github.Issue{Number: 400, Title: "Shard the nightly mutation run", Author: "pat", AuthorIsHuman: true, Labels: []string{"stage/090-ready-for-human-merge"}})
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		FrontendPR{PullRequest: github.PullRequest{Number: 401, Title: "ci(mutation): shard nightly Stryker", Author: "pat", Labels: []string{"stage/090-ready-for-human-merge", "lgtm"},
			LinkedIssues: []github.PRLinkedIssue{{Number: 400}}}},
		FrontendPR{PullRequest: github.PullRequest{Number: 402, Title: "Odd lane", Author: "pat", Labels: []string{"stage/090-ready-for-human-merge"}}})
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)

	pr401 := trackerItem(t, resp, "pr", 401)
	if pr401.HumanMergeReason != "workflow-files" || !strings.HasPrefix(pr401.HumanMergeReasonText, "It changes GitHub workflow files") || pr401.NestedUnder != 400 {
		t.Fatalf("PR 401 = reason %q text %q nested %d", pr401.HumanMergeReason, pr401.HumanMergeReasonText, pr401.NestedUnder)
	}
	issue400 := trackerItem(t, resp, "issue", 400)
	if len(issue400.LinkedPRs) != 1 {
		t.Fatalf("issue 400 linked_prs = %+v", issue400.LinkedPRs)
	}
	lp := issue400.LinkedPRs[0]
	if lp.Number != 401 || lp.Stage != 90 || !lp.ManualMerge || lp.HumanApprover != "clubanderson" || lp.HumanMergeReason != "workflow-files" || lp.HumanMergeReasonText != pr401.HumanMergeReasonText {
		t.Fatalf("issue 400 nested PR lacks the human-merge fields: %+v", lp)
	}
	raw, err := json.Marshal(lp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"human_approver":"clubanderson","manual_merge":true,"human_merge_reason":"workflow-files"`) {
		t.Fatalf("linked_prs payload lacks the fields: %s", raw)
	}

	pr402 := trackerItem(t, resp, "pr", 402)
	if pr402.HumanMergeReason != "policy" || !strings.Contains(pr402.HumanMergeReasonText, "`no-such-lane-reason`") {
		t.Fatalf("PR 402 fallback = reason %q text %q", pr402.HumanMergeReason, pr402.HumanMergeReasonText)
	}
	if soak := trackerItem(t, resp, "pr", 102); soak.HumanMergeReason != "" || soak.HumanMergeReasonText != "" {
		t.Fatalf("PR 102 (095) must not carry a human-merge reason: %+v", soak)
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
	if pr102.Caption != "Admitted by the automerge lane; soaking — 7m left" || pr102.Actor != "automerge lane" || len(pr102.Modifiers) != 0 {
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
	if issue7.Caption != "Covered by PR #102 — "+pr102.Caption {
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
	if issue8.Stage != 10 || issue8.Mirrored || issue8.Actor != "its PR" || issue8.AuthorKind != authorKindHive ||
		issue8.Caption != "Covered by PR #103 — "+pr103.Caption {
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
	stageTrackerResolveLinkedPRs(items, nil)
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
		"check-failing:coverage":             "Hive adopted this PR; hive is fixing CI — CI failed: coverage",
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
	tests := []struct {
		name     string
		pr       github.PullRequest
		repoName string
		linked   []github.PRLinkedIssue
		refs     []github.PRLinkedIssue
		want     []StageTrackerLinkedIssue
	}{
		{name: "none", pr: github.PullRequest{Repo: "acme/console", Number: 42}},
		{name: "same repo bare", pr: github.PullRequest{Repo: "acme/console", Number: 42}, linked: []github.PRLinkedIssue{{Number: 7}}, want: []StageTrackerLinkedIssue{{Number: 7, URL: "https://github.com/acme/console/issues/7"}}},
		{name: "explicit same repo, any case, state lower-cased", pr: github.PullRequest{Repo: "acme/console", Number: 42}, linked: []github.PRLinkedIssue{{Number: 7, Repo: "Acme/Console", State: "CLOSED", URL: "https://gh/7"}}, want: []StageTrackerLinkedIssue{{Number: 7, URL: "https://gh/7", State: "closed"}}},
		{name: "foreign repo dropped", pr: github.PullRequest{Repo: "acme/console", Number: 42}, linked: []github.PRLinkedIssue{{Number: 7, Repo: "acme/other"}}},
		{name: "same name, other owner dropped", pr: github.PullRequest{Repo: "hive", Number: 42}, repoName: "hivecommons/hive", linked: []github.PRLinkedIssue{{Number: 7, Repo: "fork/hive", Title: "Theirs"}}},
		{name: "self and non-positive dropped", pr: github.PullRequest{Repo: "acme/console", Number: 42}, linked: []github.PRLinkedIssue{{Number: 42}, {Number: 0}, {Number: -1}}},
		{name: "multiple, deduplicated, order kept", pr: github.PullRequest{Repo: "acme/console", Number: 42}, linked: []github.PRLinkedIssue{{Number: 9}, {Number: 7}, {Number: 9}}, want: []StageTrackerLinkedIssue{{Number: 9, URL: "https://github.com/acme/console/issues/9"}, {Number: 7, URL: "https://github.com/acme/console/issues/7"}}},
		{
			// hivecommons/hive#14568: the config names the repo "hive", the
			// GraphQL closing reference says "hivecommons/hive". The titled
			// closing link must win over the untitled `Fixes #14567` text
			// reference, and the fallback URL must carry the owner.
			name: "short config name vs full closing-ref name keeps the title",
			pr:   github.PullRequest{Repo: "hive", Number: 14568}, repoName: "hivecommons/hive",
			linked: []github.PRLinkedIssue{{Number: 14567, Repo: "hivecommons/hive", Title: "Runner stalls", State: "CLOSED", URL: "https://github.com/hivecommons/hive/issues/14567"}},
			refs:   []github.PRLinkedIssue{{Number: 14567, Repo: "hive", Via: github.PRLinkViaFixes}, {Number: 14560, Repo: "hive", Via: github.PRLinkViaRefs}},
			want: []StageTrackerLinkedIssue{
				{Number: 14567, Title: "Runner stalls", State: "closed", URL: "https://github.com/hivecommons/hive/issues/14567"},
				{Number: 14560, URL: "https://github.com/hivecommons/hive/issues/14560", Via: github.PRLinkViaRefs},
			},
		},
		{name: "short name only builds the URL from the full repo", pr: github.PullRequest{Repo: "hive", Number: 1}, repoName: "hivecommons/hive", linked: []github.PRLinkedIssue{{Number: 7}}, want: []StageTrackerLinkedIssue{{Number: 7, URL: "https://github.com/hivecommons/hive/issues/7"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := tt.pr
			pr.LinkedIssues = tt.linked
			pr.ReferencedIssues = tt.refs
			got := stageTrackerPRLinkedIssues(pr, tt.repoName, "")
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

func TestStageTrackerSameRepo(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"hivecommons/hive", "hivecommons/hive", true},
		{"HiveCommons/Hive", "hivecommons/hive", true},
		{"hive", "hivecommons/hive", true},
		{"hivecommons/hive", "hive", true},
		{"hive", "hive", true},
		{"fork/hive", "hivecommons/hive", false},
		{"hivecommons/docs", "hive", false},
		{"", "hive", false},
		{"hive", "", false},
	}
	for _, tt := range tests {
		if got := stageTrackerSameRepo(tt.a, tt.b); got != tt.want {
			t.Errorf("stageTrackerSameRepo(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
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
		{"085 record whose labels the write-back gate held off GitHub", stagelabels.Record{Stage: stagelabels.NeedsDecision, LabelsSuppressed: true}, []string{"dco-signoff: yes", "size/M"}, false},
		{"other stage, no labels", stagelabels.Record{Stage: stagelabels.Claimed}, nil, false},
	}
	for _, tc := range cases {
		if got := stageTrackerStaleDecisionRecord(tc.rec, tc.labels); got != tc.want {
			t.Errorf("%s: stale = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// TestStageTrackerRowAnchorsOnOpenPRWhenIssueClosed pins
// hivecommons/hive#14655: an issue auto-closed while its `Fixes #N` PR is
// still open at 090 stays in the tracker as a row keyed on the PR — the
// PR's stage, no regression modifier, the closed issue demoted to the
// closed_issue chip (with its not-planned state_reason when GitHub says
// so). With the PR gone too, nothing remains.
func TestStageTrackerRowAnchorsOnOpenPRWhenIssueClosed(t *testing.T) {
	now := time.Date(2026, 10, 27, 18, 15, 0, 0, time.UTC)
	const issueNum, prNum = 14567, 14568
	tests := []struct {
		name        string
		stateReason string
		prOpen      bool
		wantRow     bool
	}{
		{name: "issue completed, PR open", stateReason: "completed", prOpen: true, wantRow: true},
		{name: "issue not planned, PR open", stateReason: "not_planned", prOpen: true, wantRow: true},
		{name: "issue closed, PR merged", stateReason: "completed", prOpen: false, wantRow: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer()
			clock := now
			store := stagelabels.NewStore("", func() time.Time { return clock })
			s.stageStore = store
			key := stagelabels.Key("hivecommons/hive", prNum)
			clock = now.Add(-3 * time.Hour)
			store.Observe(key, stagelabels.ReviewPending, "contributor-lane:no-verdict")
			clock = now.Add(-2 * time.Hour)
			store.Observe(key, stagelabels.ReadyForHumanMerge, "contributor-lane:workflow-path:human-approved@clubanderson")
			clock = now

			status := minimalPayload()
			status.HiveID = "hv-test"
			repo := FrontendRepo{Name: "hive", Full: "hivecommons/hive",
				ActionableIssues: []any{github.Issue{Number: issueNum, Title: "[CI runner canary] self-hosted runner pool unhealthy", Author: "github-actions[bot]", State: "closed", Labels: []string{"stage/040-covered-by-pr"}}}}
			if tt.prOpen {
				repo.OpenPrs = []any{FrontendPR{PullRequest: github.PullRequest{Number: prNum, Title: "fix(ci): measure runner stalls by queued job age", Author: "hive-app[bot]", AppAuthored: true,
					Labels: []string{"stage/090-ready-for-human-merge", "lgtm"}, CreatedAt: now.Add(-4 * time.Hour), UpdatedAt: now.Add(-time.Hour),
					LinkedIssues: []github.PRLinkedIssue{{Number: issueNum, Repo: "hivecommons/hive", Title: "[CI runner canary] self-hosted runner pool unhealthy", State: "CLOSED", StateReason: strings.ToUpper(tt.stateReason)}}}}}
			}
			status.Repos = []FrontendRepo{repo}
			s.UpdateStatus(status)
			resp := s.buildStageTracker(status, store, now)

			for _, it := range resp.Items {
				if it.Kind == "issue" && it.Number == issueNum {
					t.Fatalf("closed issue #%d is still a row: %+v", issueNum, it)
				}
			}
			if !tt.wantRow {
				if len(resp.Items) != 0 {
					t.Fatalf("items = %+v, want none", resp.Items)
				}
				return
			}
			if len(resp.Items) != 1 {
				t.Fatalf("items = %d, want the PR row only", len(resp.Items))
			}
			pr := trackerItem(t, resp, "pr", prNum)
			if pr.Stage != trackerNumberReadyForHuman || pr.NestedUnder != 0 || pr.StageSince == nil || !pr.StageSince.Equal(now.Add(-2*time.Hour)) {
				t.Fatalf("PR row stage = %d since %v nested %d, want 090 from the PR's own record", pr.Stage, pr.StageSince, pr.NestedUnder)
			}
			if slices.Contains(pr.Modifiers, modRegressed) || pr.RegressedFrom != 0 {
				t.Fatalf("issue closure read as a regression: modifiers %v regressed_from %d", pr.Modifiers, pr.RegressedFrom)
			}
			if !pr.ManualMerge || pr.HumanApprover != "clubanderson" {
				t.Fatalf("merge gate = manual %v approver %q, want the human-merge gate intact", pr.ManualMerge, pr.HumanApprover)
			}
			if pr.ClosedIssue == nil || pr.ClosedIssue.Number != issueNum || pr.ClosedIssue.State != "closed" || pr.ClosedIssue.StateReason != tt.stateReason || pr.ClosedIssue.Title == "" {
				t.Fatalf("closed_issue = %+v, want #%d closed (%s) with its title", pr.ClosedIssue, issueNum, tt.stateReason)
			}
		})
	}
}

func TestStageTrackerClosedIssueAnchor(t *testing.T) {
	closed := StageTrackerLinkedIssue{Number: 7, State: "closed"}
	tests := []struct {
		name string
		pr   StageTrackerItem
		want *StageTrackerLinkedIssue
	}{
		{name: "not a pr", pr: StageTrackerItem{Kind: "issue", LinkedIssues: []StageTrackerLinkedIssue{closed}}},
		{name: "no links", pr: StageTrackerItem{Kind: "pr"}},
		{name: "nested under a listed issue", pr: StageTrackerItem{Kind: "pr", NestedUnder: 8, LinkedIssues: []StageTrackerLinkedIssue{{Number: 8, State: "open", Listed: true}, closed}}},
		{name: "one closing link still open", pr: StageTrackerItem{Kind: "pr", LinkedIssues: []StageTrackerLinkedIssue{closed, {Number: 9, State: "open"}}}},
		{name: "unknown state keeps the issue slot", pr: StageTrackerItem{Kind: "pr", LinkedIssues: []StageTrackerLinkedIssue{{Number: 9}}}},
		{name: "soft links only", pr: StageTrackerItem{Kind: "pr", LinkedIssues: []StageTrackerLinkedIssue{{Number: 9, State: "closed", Via: github.PRLinkViaRefs}}}},
		{name: "closed closing link", pr: StageTrackerItem{Kind: "pr", LinkedIssues: []StageTrackerLinkedIssue{closed}}, want: &closed},
		{name: "closed closing link beside a soft one", pr: StageTrackerItem{Kind: "pr", LinkedIssues: []StageTrackerLinkedIssue{{Number: 9, Via: github.PRLinkViaMention}, closed}}, want: &closed},
		{name: "first closed wins", pr: StageTrackerItem{Kind: "pr", LinkedIssues: []StageTrackerLinkedIssue{closed, {Number: 11, State: "closed", StateReason: "not_planned"}}}, want: &closed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stageTrackerClosedIssueAnchor(tt.pr)
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// hivecommons/hive#14606: a soft link (refs / title / mention) carries only a
// number, so the tracker fetches its title and state once per distinct issue
// and memoises it across rebuilds, since every status publish rebuilds the
// payload. hivecommons/hive#14667: a number that is a PR, open or closed
// (`Supersedes #14592`, an adoption's "close #14592"), is dropped rather than
// drawn as an issue "(title unavailable)".
func TestStageTrackerEnrichSoftLinks(t *testing.T) {
	var gets, closerReads atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql") {
			// The closer lookup a closed issue costs: #14576 was closed by
			// the merge of #14570; #14590 (open, body says Fixes #14576)
			// is listed under the same relation and must not be named.
			closerReads.Add(1)
			fmt.Fprint(w, `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[{"number":14590,"merged":false},{"number":14570,"url":"https://github.com/hivecommons/hive/pull/14570","merged":true,"mergedAt":"2026-10-27T12:00:00Z"}]}}}}}`)
			return
		}
		gets.Add(1)
		switch r.URL.Path {
		case "/repos/hivecommons/hive/issues/14576":
			fmt.Fprint(w, `{"number":14576,"title":"Runner stalls","state":"closed","state_reason":"completed"}`)
		case "/repos/hivecommons/hive/issues/14577":
			fmt.Fprint(w, `{"number":14577,"title":"A PR","state":"open","pull_request":{}}`)
		case "/repos/hivecommons/hive/issues/14592":
			fmt.Fprint(w, `{"number":14592,"title":"Superseded PR","state":"closed","pull_request":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	s := newTestServer()
	s.deps = &Dependencies{GHClient: github.NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, s.logger)}
	url := func(n int) string { return "https://github.com/hivecommons/hive/issues/" + strconv.Itoa(n) }
	build := func() []StageTrackerItem {
		return []StageTrackerItem{
			{Repo: "hive", Number: 14597, Kind: "pr", LinkedIssues: []StageTrackerLinkedIssue{
				{Number: 14576, URL: url(14576), Via: github.PRLinkViaRefs},
				{Number: 14577, URL: url(14577), Via: github.PRLinkViaTitle},
				{Number: 14578, URL: url(14578), Via: github.PRLinkViaRefs},
				{Number: 14567, Title: "Titled", State: "closed", URL: url(14567)},
				{Number: 14592, URL: url(14592)},
			}},
			{Repo: "hive", Number: 14590, Kind: "pr", LinkedIssues: []StageTrackerLinkedIssue{{Number: 14576, URL: url(14576), Via: github.PRLinkViaRefs}}},
		}
	}
	qualify := func(string) string { return "hivecommons/hive" }
	now := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	items := build()
	s.stageTrackerEnrichSoftLinks(items, qualify, now)

	links := items[0].LinkedIssues
	if len(links) != 3 {
		t.Fatalf("links = %+v, want the PR numbers 14577 and 14592 dropped", links)
	}
	if got := links[0]; got.Number != 14576 || got.Title != "Runner stalls" || got.State != "closed" || got.StateReason != "completed" || got.ClosedBy != 14570 {
		t.Fatalf("refs link = %+v, want titled, closed (completed) by merged #14570", got)
	}
	if got := items[1].LinkedIssues[0]; got.Title != "Runner stalls" || got.State != "closed" || got.ClosedBy != 14570 {
		t.Fatalf("second PR's refs link = %+v, want closed by #14570 (never the open #14590)", got)
	}
	if n := closerReads.Load(); n != 1 {
		t.Fatalf("closer reads = %d, want 1 (closed issues only, memoised)", n)
	}
	if got := links[1]; got.Number != 14578 || got.Title != "" || got.State != "" {
		t.Fatalf("failed lookup = %+v, want untouched", got)
	}
	if got := links[2]; got.Number != 14567 || got.Title != "Titled" || got.State != "closed" {
		t.Fatalf("closing link = %+v, want unchanged", got)
	}
	if n := gets.Load(); n != 4 {
		t.Fatalf("issues GETs = %d, want 4 (one per distinct number)", n)
	}

	// A rebuild after a status publish reuses the memo, the failed lookup
	// included, until its retry window lapses.
	items = build()
	s.stageTrackerEnrichSoftLinks(items, qualify, now.Add(time.Minute))
	if n := gets.Load(); n != 4 || len(items[0].LinkedIssues) != 3 || items[0].LinkedIssues[0].Title != "Runner stalls" {
		t.Fatalf("rebuild: GETs = %d, links = %+v, want memoised", n, items[0].LinkedIssues)
	}
	s.stageTrackerEnrichSoftLinks(build(), qualify, now.Add(stageTrackerLinkRetry))
	if n := gets.Load(); n != 5 {
		t.Fatalf("issues GETs = %d, want only the failed lookup retried", n)
	}
	s.stageTrackerEnrichSoftLinks(build(), qualify, now.Add(stageTrackerLinkTTL))
	if n := gets.Load(); n != 9 {
		t.Fatalf("issues GETs = %d, want every lookup refreshed after the TTL", n)
	}

	// One rebuild makes at most stageTrackerLinkLookups GETs.
	var many []StageTrackerLinkedIssue
	for n := 20000; n < 20000+stageTrackerLinkLookups+5; n++ {
		many = append(many, StageTrackerLinkedIssue{Number: n, URL: url(n), Via: github.PRLinkViaRefs})
	}
	before := gets.Load()
	s.stageTrackerEnrichSoftLinks([]StageTrackerItem{{Repo: "hive", Number: 14598, Kind: "pr", LinkedIssues: many}}, qualify, now)
	if n := gets.Load() - before; n != stageTrackerLinkLookups {
		t.Fatalf("issues GETs in one rebuild = %d, want the cap %d", n, stageTrackerLinkLookups)
	}
}
