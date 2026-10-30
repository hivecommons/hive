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

// A PR superseded by a hive adoption is closed and not listed (owner,
// hivecommons/hive#15526 under #15511 drew beside hive's #15574 as a
// greyed "Adopted → #15574 · superseded" sub-row). The server marks the
// original with `superseded_by` so the SPA folds it into the copy's
// "adopted from" chip; the copy's lineage and adopted_from carry the
// original's state ("open" while the write-back gate kept it open,
// "closed" once close-first ran).
func supersededFoldFixture(t *testing.T, now time.Time) (*Server, *stagelabels.Store, *StatusPayload, string) {
	t.Helper()
	s := newTestServer()
	store := stagelabels.NewStore("", func() time.Time { return now })
	s.stageStore = store
	s.deps = &Dependencies{Config: &config.Config{}}
	orig := stagelabels.Key("hivecommons/hive", 15526)
	store.Observe(orig, stagelabels.AdoptedByHive, "contributor-lane:adopted")
	store.MarkAdopted(orig, "ab12cd3", 15574, now.Add(-15*time.Minute))
	store.Observe(stagelabels.Key("hivecommons/hive", 15574), stagelabels.AwaitingMergeAdmission, "merge-lane:queued")

	status := minimalPayload()
	status.HiveID = "hive"
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		ActionableIssues: []any{github.Issue{Number: 15511, Title: "Tracker lists a superseded PR", Author: "clubanderson", Labels: []string{"stage/040-covered-by-pr"},
			LinkedPRs: []github.IssueLinkedPR{
				{Number: 15526, State: "open", URL: "https://github.com/hivecommons/hive/pull/15526", Closing: true},
				{Number: 15574, State: "open", URL: "https://github.com/hivecommons/hive/pull/15574", Closing: true},
			}}},
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 15526, Title: "fix: tracker", Author: "odttlnt", HeadSHA: "ab12cd3", Labels: []string{"author/clanker", "role/contributor", "stage/074-adopted-by-hive"},
				LinkedIssues: []github.PRLinkedIssue{{Number: 15511, State: "open"}}}},
			FrontendPR{PullRequest: github.PullRequest{Number: 15574, Title: "fix: tracker", Author: "hivecommons-hive[bot]", AppAuthored: true, HeadSHA: "ef45ab6", Labels: []string{"stage/080-merge-gate"},
				LinkedIssues: []github.PRLinkedIssue{{Number: 15511, State: "open"}}}},
		},
	}}
	s.UpdateStatus(status)
	return s, store, status, orig
}

func TestStageTrackerSupersededOriginalCarriesSupersededBy(t *testing.T) {
	now := time.Date(2026, 10, 29, 23, 49, 0, 0, time.UTC)
	s, store, status, _ := supersededFoldFixture(t, now)
	resp := s.buildStageTracker(status, store, now)

	orig := trackerItem(t, resp, "pr", 15526)
	if orig.SupersededBy != 15574 || orig.AdoptedBy != 15574 || orig.AdoptedByState != "" {
		t.Fatalf("original superseded_by=%d adopted_by=%d state=%q", orig.SupersededBy, orig.AdoptedBy, orig.AdoptedByState)
	}
	if orig.NestedUnder != 15511 {
		t.Fatalf("original nested_under=%d", orig.NestedUnder)
	}
	copyPR := trackerItem(t, resp, "pr", 15574)
	if copyPR.SupersededBy != 0 {
		t.Fatalf("copy must never carry superseded_by: %d", copyPR.SupersededBy)
	}
	if copyPR.AdoptedFrom == nil || copyPR.AdoptedFrom.Number != 15526 || copyPR.AdoptedFrom.State != "open" {
		t.Fatalf("copy adopted_from = %+v", copyPR.AdoptedFrom)
	}
	if copyPR.Lineage == nil || copyPR.Lineage.FromNumber != 15526 || copyPR.Lineage.FromState != "open" {
		t.Fatalf("copy lineage = %+v", copyPR.Lineage)
	}
	issue := trackerItem(t, resp, "issue", 15511)
	var mirrored bool
	for _, lp := range issue.LinkedPRs {
		if lp.Number == 15526 {
			mirrored = lp.SupersededBy == 15574
		}
	}
	if !mirrored {
		t.Fatalf("issue linked_prs do not mirror superseded_by: %+v", issue.LinkedPRs)
	}
	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"superseded_by":15574`) {
		t.Fatalf("payload lacks superseded_by: %s", raw)
	}
}

func TestStageTrackerSupersededByClearsWhenCopyDies(t *testing.T) {
	now := time.Date(2026, 10, 29, 23, 49, 0, 0, time.UTC)
	s, store, status, orig := supersededFoldFixture(t, now)
	store.NoteAdoptionOrphaned(orig, 15574, now)
	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 15526)
	if it.SupersededBy != 0 || it.AdoptedByState != "closed" {
		t.Fatalf("orphaned original must not fold: superseded_by=%d state=%q", it.SupersededBy, it.AdoptedByState)
	}
}

func TestStageTrackerSupersededByOmittedForInPlaceAdoption(t *testing.T) {
	now := time.Date(2026, 10, 29, 23, 49, 0, 0, time.UTC)
	s, store, status, key := orphanTrackerFixture(t, now, stagelabels.AdoptedByHive, "contributor-lane:adopted", []string{"stage/074-adopted-by-hive"})
	store.MarkAdoptedInPlace(key, "bc3a6ea", "contributor-lane:adopted-in-place", now)
	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14947)
	if it.SupersededBy != 0 {
		t.Fatalf("in-place adoption has no superseder: %d", it.SupersededBy)
	}
}

func TestStageTrackerSupersededFromStateClosedAfterCloseFirst(t *testing.T) {
	now := time.Date(2026, 10, 29, 23, 49, 0, 0, time.UTC)
	s, store, status, orig := supersededFoldFixture(t, now)
	store.MarkSupersededClosedFirst(orig, 15574, false, now.Add(-10*time.Minute))
	resp := s.buildStageTracker(status, store, now)
	for _, it := range resp.Items {
		if it.Kind == "pr" && it.Number == 15526 {
			t.Fatalf("closed original rendered as a row: %+v", it)
		}
	}
	copyPR := trackerItem(t, resp, "pr", 15574)
	if copyPR.Lineage == nil || copyPR.Lineage.FromNumber != 15526 || copyPR.Lineage.FromState != "closed" {
		t.Fatalf("copy lineage = %+v", copyPR.Lineage)
	}
	if copyPR.AdoptedFrom != nil && copyPR.AdoptedFrom.State != "closed" {
		t.Fatalf("copy adopted_from = %+v", copyPR.AdoptedFrom)
	}
	if copyPR.Supersedes == nil || copyPR.Supersedes.Number != 15526 || copyPR.Supersedes.Open {
		t.Fatalf("copy supersedes = %+v", copyPR.Supersedes)
	}
}

// Ratchet (SPA): no item whose superseded_by names a rendered open PR
// appears as a row — nested under an issue or at top level.
func TestStageTrackerSupersededFoldRatchetJS(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+stageTrackerRowsFixtureJS+`
const since = new Date(now - 3600000).toISOString();
const pr = (n, extra) => Object.assign({ repo: 'hivecommons/hive', number: n, kind: 'pr', title: 'fix: tracker', url: 'https://x/' + n, stage: 74, stage_since: since, caption: 'Adopted', author: 'odttlnt', author_kind: 'clanker', modifiers: [] }, extra || {});
const issue = (n, prs, extra) => Object.assign({ repo: 'hivecommons/hive', number: n, kind: 'issue', title: 'Issue ' + n, url: 'https://x/i' + n, stage: 40, stage_since: since, linked_prs: prs }, extra || {});
const copy = (n, from, extra) => pr(n, Object.assign({ author: 'hivecommons-hive[bot]', author_kind: 'hive', stage: 80, caption: 'Merge queued', adopted_from: { number: from, url: 'https://x/' + from, author: 'odttlnt', state: 'open', text: '↳ adopted from #' + from } }, extra || {}));

// Nested: the issue lists both; only the copy is drawn.
{
  const orig = pr(15526, { adopted_by: 15574, adopted_by_state: '', superseded_by: 15574, nested_under: 15511 });
  const cp = copy(15574, 15526, { nested_under: 15511 });
  const items = [issue(15511, [orig, cp]), orig, cp];
  const rows = stageTrackerVisible(items, { kind: 'all', repo: '' });
  assert.deepStrictEqual(rows.map(r => r.kind + '#' + r.number), ['issue#15511'], 'the nested pair draws under the issue only');
  const subs = stageTrackerLinkedSorted(rows[0]).map(lp => lp.number);
  assert.deepStrictEqual(subs, [15574], 'the superseded original is not a sub-row');
  const prs = stageTrackerVisible(items, { kind: 'pr', repo: '' }).map(r => r.number);
  assert.deepStrictEqual(prs, [15574], 'the Pull requests filter never lists the superseded original');
}
// Top level: the pair without an issue; the original folds.
{
  const orig = pr(15526, { adopted_by: 15574, superseded_by: 15574 });
  const cp = copy(15574, 15526);
  assert.deepStrictEqual(stageTrackerVisible([orig, cp], { kind: 'all', repo: '' }).map(r => r.number), [15574]);
  assert.deepStrictEqual(stageTrackerFoldSuperseded([orig, cp]).map(r => r.number), [15574]);
}
// The superseder not drawn (closed / merged / absent): the original stays.
{
  const orig = pr(15526, { adopted_by: 15574, superseded_by: 15574 });
  assert.deepStrictEqual(stageTrackerFoldSuperseded([orig]).map(r => r.number), [15526], 'no superseder listed: keep');
  assert.deepStrictEqual(stageTrackerFoldSuperseded([orig, copy(15574, 15526, { state: 'closed' })]).map(r => r.number), [15526, 15574], 'closed superseder: keep');
  assert.deepStrictEqual(stageTrackerFoldSuperseded([orig, copy(15574, 15526, { modifiers: ['merged'] })]).length, 1, 'a merged copy still drops the original: the fold only asks that the copy be drawn');
  const other = pr(15526, { repo: 'hivecommons/docs', adopted_by: 15574, superseded_by: 15574 });
  assert.deepStrictEqual(stageTrackerFoldSuperseded([other, copy(15574, 15526)]).map(r => r.repo + '#' + r.number), ['hivecommons/docs#15526', 'hivecommons/hive#15574'], 'repo-scoped: a same-numbered PR elsewhere is not the superseder');
}
// Ratchet over a mixed fixture: no drawn row's superseded_by names a drawn open PR.
{
  const orig = pr(15526, { adopted_by: 15574, superseded_by: 15574, nested_under: 15511 });
  const cp = copy(15574, 15526, { nested_under: 15511 });
  const loose = pr(15561, { adopted_by: 15567, superseded_by: 15567 });
  const looseCopy = copy(15567, 15561);
  const items = [issue(15511, [orig, cp]), orig, cp, loose, looseCopy, pr(15600, { stage: 70, caption: 'Fixing' })];
  for (const f of [{ kind: 'all', repo: '' }, { kind: 'pr', repo: '' }]) {
    const rows = stageTrackerVisible(items, f);
    const drawn = new Set();
    for (const r of rows) {
      if (r.kind === 'pr') drawn.add(r.repo + '#' + r.number);
      if (r.kind === 'issue') for (const lp of stageTrackerLinkedSorted(r)) drawn.add(r.repo + '#' + lp.number);
    }
    for (const r of rows) {
      if (r.kind === 'pr') assert.ok(!(Number(r.superseded_by) > 0 && drawn.has(r.repo + '#' + r.superseded_by)), 'ratchet: superseded row drawn beside its open superseder: ' + r.number);
      if (r.kind === 'issue') for (const lp of stageTrackerLinkedSorted(r)) assert.ok(!(Number(lp.superseded_by) > 0 && drawn.has(r.repo + '#' + lp.superseded_by)), 'ratchet: superseded sub-row drawn beside its superseder: ' + lp.number);
    }
    assert.ok(drawn.has('hivecommons/hive#15574') && drawn.has('hivecommons/hive#15567') && drawn.has('hivecommons/hive#15600'), 'the copies and the unrelated PR draw: ' + [...drawn].join(','));
  }
}
`)
}
