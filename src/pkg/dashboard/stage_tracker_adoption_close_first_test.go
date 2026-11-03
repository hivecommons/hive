package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Close-first supersession (hivecommons/hive#15135): the closed original
// never reaches the tracker; a reopened one reads "copy failed", parked for
// a person, and is not "recovering".

func TestStageTrackerCloseFirstOriginalNotRendered(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	s, store, status, key := orphanTrackerFixture(t, now, stagelabels.AdoptedByHive, "contributor-lane:adopted", []string{"stage/074-adopted-by-hive"})
	store.MarkAdopted(key, "bc3a6ea", 15123, now)
	store.MarkSupersededClosedFirst(key, 15123, false, now)
	// Even if the status still lists the original (the close has not
	// propagated), the superseded record drops the row.
	for _, it := range s.buildStageTracker(status, store, now).Items {
		if it.Kind == "pr" && it.Number == 14947 {
			t.Fatalf("superseded original rendered: %+v", it)
		}
	}
}

func TestStageTrackerCloseFirstCopyFailedSaysWhy(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	s, store, status, key := orphanTrackerFixture(t, now, stagelabels.NeedsDecision, stagelabels.LaneReasonAdoptionCopyFailed, []string{"stage/085-needs-decision"})
	store.MarkAdopted(key, "bc3a6ea", 15123, now.Add(-time.Hour))
	store.MarkSupersededClosedFirst(key, 15123, false, now.Add(-time.Hour))
	store.MarkAdoptionReopened(key, 15123, now)
	store.MarkAdoptionEscalated(key, "bc3a6ea", stagelabels.LaneReasonAdoptionCopyFailed)

	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14947)
	if it.AdoptedBy != 15123 || it.AdoptedByState != "copy-failed" {
		t.Fatalf("adopted_by=%d state=%q", it.AdoptedBy, it.AdoptedByState)
	}
	if !strings.Contains(it.Caption, "copy #15123 failed") || !strings.Contains(it.Caption, "reopened") || !strings.Contains(it.Caption, "needs a person") || strings.Contains(it.Caption, "fresh copy") {
		t.Fatalf("caption = %q", it.Caption)
	}
	if it.DecisionNeeded == nil || !strings.Contains(it.DecisionNeeded.Question, "#15123") || !strings.Contains(it.DecisionNeeded.Question, "reopened") {
		t.Fatalf("decision_needed = %+v", it.DecisionNeeded)
	}
	if got := humanizeStageReason(stagelabels.LaneReasonAdoptionCopyFailed); !strings.Contains(got, "copy of this PR failed") {
		t.Fatalf("humanizeStageReason = %q", got)
	}
	raw, _ := json.Marshal(it)
	if !strings.Contains(string(raw), `"adopted_by_state":"copy-failed"`) {
		t.Fatalf("payload lacks adopted_by_state: %s", raw)
	}
	// An operator re-adopts: the fresh copy clears the state.
	store.MarkAdopted(key, "bc3a6ea", 15200, now.Add(time.Minute))
	store.Observe(key, stagelabels.AdoptedByHive, "contributor-lane:adopted")
	it = trackerItem(t, s.buildStageTracker(status, store, now.Add(time.Minute)), "pr", 14947)
	if it.AdoptedBy != 15200 || it.AdoptedByState != "" {
		t.Fatalf("after re-adopt adopted_by=%d state=%q", it.AdoptedBy, it.AdoptedByState)
	}
}

// A closed original (#15184, closed first when its copy #15247 opened)
// must not ride under its issue as a bare sub-row with an empty ladder:
// the claims index still names it "open" for a while, and the status
// enumeration's bucket reason still names it. It rides as the muted
// "superseded · closed · hive adopted → #15247" line while the copy is
// open (hivecommons/hive#15511), and the live copy keeps the trace as
// `supersedes` metadata (hivecommons/hive#15135 follow-up).
func TestStageTrackerClosedSupersededPRNotASubRow(t *testing.T) {
	now := time.Date(2026, 10, 29, 2, 30, 0, 0, time.UTC)
	s := newTestServer()
	store := stagelabels.NewStore("", func() time.Time { return now })
	s.stageStore = store
	s.deps = &Dependencies{Config: &config.Config{}}
	orig := stagelabels.Key("hivecommons/hive", 15184)
	store.Observe(orig, stagelabels.AdoptedByHive, "contributor-lane:adopted")
	store.MarkAdopted(orig, "bc3a6ea", 15247, now.Add(-30*time.Minute))
	store.MarkSupersededClosedFirst(orig, 15247, false, now.Add(-16*time.Minute))
	store.Observe(stagelabels.Key("hivecommons/hive", 15247), stagelabels.Fixing, "reviewer-verdict:changes_requested")

	status := minimalPayload()
	status.HiveID = "hive"
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		ActionableIssues: []any{github.Issue{Number: 15183, Title: "api-reference.md decision row is stale", Author: "pat", Labels: []string{"stage/040-covered-by-pr"},
			// The claims index lags the close: it still reports #15184 open.
			LinkedPRs: []github.IssueLinkedPR{{Number: 15184, State: "open", URL: "https://github.com/hivecommons/hive/pull/15184", Closing: true}}}},
		// Only the copy is an open PR of the snapshot; the closed original is not.
		OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Number: 15247, Title: "📖 docs: refresh operator decision API reference", Author: "hivecommons-hive[bot]", AppAuthored: true, HeadSHA: "8464bac", Labels: []string{"stage/070-fixing"},
			LinkedIssues: []github.PRLinkedIssue{{Number: 15183, State: "open"}}}}},
	}}
	s.UpdateStatus(status)

	resp := s.buildStageTracker(status, store, now)
	for _, it := range resp.Items {
		if it.Kind == "pr" && it.Number == 15184 {
			t.Fatalf("closed original rendered as a row: %+v", it)
		}
	}
	issue := trackerItem(t, resp, "issue", 15183)
	if len(issue.LinkedPRs) != 2 || issue.LinkedPRs[0].Number != 15247 || issue.LinkedPRs[1].Number != 15184 {
		t.Fatalf("issue 15183 linked_prs = %+v (want the live copy #15247 and the muted closed original)", issue.LinkedPRs)
	}
	if c := issue.LinkedPRs[1]; c.State != "closed" || c.SupersededBy != 15247 || c.Caption != "superseded · closed · hive adopted → #15247" || c.Title == "" || c.Stage != 0 {
		t.Fatalf("closed original must be the muted line, never a bare numbered row: %+v", c)
	}
	lp := issue.LinkedPRs[0]
	if lp.Stage != 70 || lp.Title == "" {
		t.Fatalf("live sub-row not resolved from its PR row: %+v", lp)
	}
	if lp.Supersedes == nil || lp.Supersedes.Number != 15184 || lp.Supersedes.URL != "https://github.com/hivecommons/hive/pull/15184" || lp.Supersedes.Open || lp.Supersedes.ClosedAt == nil || !lp.Supersedes.ClosedAt.Equal(now.Add(-16*time.Minute)) {
		t.Fatalf("sub-row supersedes = %+v", lp.Supersedes)
	}
	pr := trackerItem(t, resp, "pr", 15247)
	if pr.Supersedes == nil || pr.Supersedes.Number != 15184 || pr.NestedUnder != 15183 {
		t.Fatalf("pr 15247 supersedes=%+v nested_under=%d", pr.Supersedes, pr.NestedUnder)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"supersedes":{"number":15184,"url":"https://github.com/hivecommons/hive/pull/15184","closed_at":`) {
		t.Fatalf("payload lacks the supersedes chip metadata: %s", raw)
	}
	if strings.Contains(string(raw), `{"number":15184,"url":"https://github.com/hivecommons/hive/pull/15184"}`) {
		t.Fatalf("payload still carries the closed original as a bare linked PR: %s", raw)
	}
}

// The resolver drops a linked PR that has no PR row in the response (not
// open in the snapshot) unless the caller vouches for it as open elsewhere
// (a cross-repository PR listed by another repo's open set).
func TestStageTrackerResolveLinkedPRsDropsUnlistedPRs(t *testing.T) {
	items := func() []StageTrackerItem {
		return []StageTrackerItem{
			{Repo: "acme/console", Number: 9, Kind: "issue", LinkedPR: 77, LinkedPRs: []StageTrackerLinkedPR{{Number: 10}, {Number: 11, URL: "u11"}}},
			{Repo: "acme/console", Number: 10, Kind: "pr", Title: "Live PR", Stage: 70, Supersedes: &StageTrackerSupersedes{Number: 11}},
		}
	}
	got := items()
	stageTrackerResolveLinkedPRs(got, nil)
	if len(got[0].LinkedPRs) != 1 || got[0].LinkedPRs[0].Number != 10 || got[0].LinkedPRs[0].Supersedes == nil || got[0].LinkedPRs[0].Supersedes.Number != 11 {
		t.Fatalf("unlisted links kept: %+v", got[0].LinkedPRs)
	}
	got = items()
	stageTrackerResolveLinkedPRs(got, func(repo string, number int) bool { return number == 11 })
	if len(got[0].LinkedPRs) != 2 || got[0].LinkedPRs[1].Number != 11 || got[0].LinkedPRs[1].URL != "u11" || got[0].LinkedPRs[1].Title != "" {
		t.Fatalf("vouched-for bare link not kept as is: %+v", got[0].LinkedPRs)
	}
}

func TestStageTrackerSupersedesIndex(t *testing.T) {
	now := time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)
	store := stagelabels.NewStore("", func() time.Time { return now })
	for _, n := range []int{15184, 15170} {
		key := stagelabels.Key("hivecommons/hive", n)
		store.Observe(key, stagelabels.AdoptedByHive, "contributor-lane:adopted")
		store.MarkAdopted(key, "bc3a6ea", 15247, now)
		store.MarkSupersededClosedFirst(key, 15247, n == 15170, now)
	}
	// A plain adoption whose copy has not merged is not a supersession yet.
	plain := stagelabels.Key("hivecommons/hive", 15300)
	store.Observe(plain, stagelabels.AdoptedByHive, "contributor-lane:adopted")
	store.MarkAdopted(plain, "bc3a6ea", 15301, now)

	idx := stageTrackerSupersedesIndex(store, "")
	if len(idx) != 1 {
		t.Fatalf("index = %+v", idx)
	}
	sup, ok := idx[stagelabels.Key("hivecommons/hive", 15247)]
	if !ok || sup.Number != 15170 || !sup.Open || sup.URL != "https://github.com/hivecommons/hive/pull/15170" || sup.ClosedAt == nil || !sup.ClosedAt.Equal(now) {
		t.Fatalf("supersedes = %+v", sup)
	}
	if got := stageTrackerSupersedesIndex(nil, ""); len(got) != 0 {
		t.Fatalf("nil store = %+v", got)
	}
	if stageTrackerLinkedPRFinished("") || stageTrackerLinkedPRFinished(blockedOnPRStateOpen) || !stageTrackerLinkedPRFinished(blockedOnPRStateClosed) || !stageTrackerLinkedPRFinished(blockedOnPRStateMerged) {
		t.Fatal("stageTrackerLinkedPRFinished")
	}
}

// The SPA draws the trace as one muted chip on the live PR's row (and its
// sub-row), linking to the closed original; nothing without `supersedes`.
func TestStageTrackerSupersedesChipJS(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".ham-badge-supersedes {",
		"supersedes: lp.supersedes || null",
		"stageTrackerSupersedesChipHtml(item, Date.now())",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	script := strings.Join([]string{
		"const assert = require('assert');",
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "stageTrackerAgoText"),
		jsFunc(t, html, "stageTrackerSupersedesTip"),
		jsFunc(t, html, "stageTrackerSupersedesChipHtml"),
		jsFunc(t, html, "stageTrackerLinkedPRItem"),
		jsFunc(t, html, "stageTrackerLinkedPROpen"),
		"const STAGE_TRACKER_TERMINAL = 100;",
		`
const nowMs = Date.parse('2026-10-29T02:30:00Z');
const live = { kind: 'pr', repo: 'hivecommons/hive', number: 15247, stage: 70, supersedes: { number: 15184, url: 'https://github.com/hivecommons/hive/pull/15184', closed_at: '2026-10-29T02:14:00Z' } };
const chip = stageTrackerSupersedesChipHtml(live, nowMs);
assert.ok(chip.includes('>supersedes #15184<'), chip);
assert.ok(chip.includes('href="https://github.com/hivecommons/hive/pull/15184"'), chip);
assert.ok(chip.includes('closed 16m ago'), chip);
assert.ok(chip.includes('ham-badge-supersedes'), chip);
assert.strictEqual(stageTrackerSupersedesChipHtml({ kind: 'pr', number: 1 }, nowMs), '');
assert.ok(stageTrackerSupersedesTip({ supersedes: { number: 7, open: true } }, nowMs).includes('left open'));
const issue = { kind: 'issue', repo: 'hivecommons/hive', number: 15183, linked_prs: [Object.assign({ title: 'docs' }, live)] };
const sub = stageTrackerLinkedPRItem(issue, issue.linked_prs[0]);
assert.strictEqual(sub.supersedes.number, 15184);
assert.strictEqual(stageTrackerLinkedPRItem(issue, { number: 2 }).supersedes, null);
`,
	}, "\n")
	runStageTrackerRowActionsScript(t, script)
}

// hivecommons/hive#15307: a copy the fixer is rebasing reads "Adopting ·
// fixer resolving conflicts" (one label) at 074 and, once the episode is
// spent, 085 offers Adopt into fresh branch / Close as superseded.
func TestStageTrackerAdoptionCopyResolvingConflicts(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	s, store, status, key := orphanTrackerFixture(t, now, stagelabels.AdoptedByHive, "conflicting", []string{"stage/074-adopted-by-hive", "needs-rebase"})
	store.MarkAdopted(key, "bc3a6ea", 15123, now.Add(-time.Hour))
	store.NoteAdoptCopyConflicting(key, true)

	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14947)
	if it.AdoptedBy != 15123 || it.AdoptedByState != "resolving-conflicts" {
		t.Fatalf("adopted_by=%d state=%q", it.AdoptedBy, it.AdoptedByState)
	}
	if !strings.Contains(it.Caption, "Adopting") || !strings.Contains(it.Caption, "#15123") || !strings.Contains(it.Caption, "fixer is resolving the conflicts") {
		t.Fatalf("caption = %q", it.Caption)
	}

	reason := stagelabels.LaneReasonAdoptionStalledPrefix + stagelabels.LaneReasonRebaseFailedTwice
	store.MarkAdoptionEscalated(key, "bc3a6ea", reason)
	store.Observe(key, stagelabels.NeedsDecision, reason)
	status.Repos[0].OpenPrs = []any{FrontendPR{PullRequest: github.PullRequest{Number: 14947, Title: "Fix", Author: "thatguywob", HeadSHA: "bc3a6ea", Labels: []string{"author/clanker", "role/contributor", "stage/085-needs-decision", "needs-rebase"}}}}
	s.UpdateStatus(status)
	it = trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14947)
	if !strings.Contains(it.Caption, "Rebase failed twice") || !strings.Contains(it.Caption, "#15123") {
		t.Fatalf("085 caption = %q", it.Caption)
	}
	d := it.DecisionNeeded
	if d == nil || !strings.Contains(d.Question, "two attempts") || len(d.Options) < 2 || d.Options[0].Action != decisionActionAdopt || d.Options[0].Label != "Adopt into fresh branch" {
		t.Fatalf("decision_needed = %+v", d)
	}
	closeSup := false
	for _, o := range d.Options {
		closeSup = closeSup || (o.Action == decisionActionCloseSuperseded && o.Label == "Close as superseded")
	}
	if !closeSup {
		t.Fatalf("Close as superseded missing: %+v", d.Options)
	}

	// The hive's own copy at 085 rebase-failed-twice gets the same options.
	copyItem := StageTrackerItem{Repo: "hivecommons/hive", Number: 15123, Kind: "pr", Stage: 85, Reason: stagelabels.LaneReasonRebaseFailedTwice}
	cd := decisionForPR(copyItem, nil, "", false)
	if cd.Options[0].Action != decisionActionAdopt || !strings.Contains(cd.Question, "fresh branch") {
		t.Fatalf("copy decision = %+v", cd)
	}

	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, strings.Join([]string{
		"const assert = require('assert');",
		jsConstLine(t, html, "STAGE_TRACKER_ADOPTED_BY_HIVE"),
		jsFunc(t, html, "stageTrackerAdoptedWord"),
		`assert.strictEqual(stageTrackerAdoptedWord({ adopted_by: 15123, adopted_by_state: 'resolving-conflicts' }), 'Adopting · fixer resolving conflicts');`,
	}, "\n"))
}
