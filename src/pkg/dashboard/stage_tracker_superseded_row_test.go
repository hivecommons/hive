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

// A PR superseded by a hive adoption stays listed under its issue as one
// muted, chevron-less sub-row while its superseder is open (owner,
// hivecommons/hive#15511: hive's #15574 and, beneath it, the original
// #15526 greyed). The server marks the original with `superseded_by`, the
// copy's lineage / adopted_from carry the original's state ("open" while
// it waits, "closed" once close-first ran), a closed original is listed
// under the issue only while the copy is, and the note reads "superseded ·
// hive adopted → #N" / "superseded · closed · hive adopted → #N".
func supersededRowFixture(t *testing.T, now time.Time) (*Server, *stagelabels.Store, *StatusPayload, string) {
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
	s, store, status, _ := supersededRowFixture(t, now)
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
	s, store, status, orig := supersededRowFixture(t, now)
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

func TestStageTrackerSupersededClosedOriginalListedWhileCopyOpen(t *testing.T) {
	now := time.Date(2026, 10, 29, 23, 49, 0, 0, time.UTC)
	s, store, status, orig := supersededRowFixture(t, now)
	store.MarkSupersededClosedFirst(orig, 15574, false, now.Add(-10*time.Minute))
	// The close landed: GitHub's open-PR list no longer carries #15526.
	status.Repos[0].OpenPrs = status.Repos[0].OpenPrs[1:]
	s.UpdateStatus(status)
	resp := s.buildStageTracker(status, store, now)
	for _, it := range resp.Items {
		if it.Kind == "pr" && it.Number == 15526 {
			t.Fatalf("closed original rendered as a row of its own: %+v", it)
		}
	}
	copyPR := trackerItem(t, resp, "pr", 15574)
	if copyPR.Lineage == nil || copyPR.Lineage.FromNumber != 15526 || copyPR.Lineage.FromState != "closed" {
		t.Fatalf("copy lineage = %+v", copyPR.Lineage)
	}
	if copyPR.Supersedes == nil || copyPR.Supersedes.Number != 15526 || copyPR.Supersedes.Open {
		t.Fatalf("copy supersedes = %+v", copyPR.Supersedes)
	}
	issue := trackerItem(t, resp, "issue", 15511)
	var closed *StageTrackerLinkedPR
	for i := range issue.LinkedPRs {
		if issue.LinkedPRs[i].Number == 15526 {
			closed = &issue.LinkedPRs[i]
		}
	}
	if closed == nil {
		t.Fatalf("the closed original must stay listed under the issue while #15574 is open: %+v", issue.LinkedPRs)
	}
	if closed.State != "closed" || closed.SupersededBy != 15574 || closed.Caption != "superseded · closed · hive adopted → #15574" || closed.Title != "fix: tracker" || closed.AuthorKind != "clanker" {
		t.Fatalf("closed original = %+v", *closed)
	}
}

func TestStageTrackerSupersededClosedOriginalLeavesWithCopy(t *testing.T) {
	now := time.Date(2026, 10, 29, 23, 49, 0, 0, time.UTC)
	s, store, status, orig := supersededRowFixture(t, now)
	store.MarkSupersededClosedFirst(orig, 15574, false, now.Add(-10*time.Minute))
	// Both gone from GitHub's open list: the copy merged.
	status.Repos[0].OpenPrs = nil
	s.UpdateStatus(status)
	resp := s.buildStageTracker(status, store, now)
	for _, it := range resp.Items {
		if it.Kind == "pr" && (it.Number == 15526 || it.Number == 15574) {
			t.Fatalf("closed pair rendered: %+v", it)
		}
		if it.Kind == "issue" && it.Number == 15511 {
			for _, lp := range it.LinkedPRs {
				if lp.Number == 15526 {
					t.Fatalf("closed original listed without its open superseder: %+v", it.LinkedPRs)
				}
			}
		}
	}
}

func TestStageTrackerSupersededNoteCSSNeverWraps(t *testing.T) {
	html := indexHTML(t)
	rule := ".ham-superseded-note {"
	i := strings.Index(html, rule)
	if i < 0 {
		t.Fatal("index.html lacks the .ham-superseded-note rule")
	}
	body := html[i : i+strings.Index(html[i:], "}")]
	for _, want := range []string{"white-space: nowrap", "flex: 0 0 auto"} {
		if !strings.Contains(body, want) {
			t.Errorf("the superseded note must not wrap nor shrink (the title ellipsizes first): %q lacks %q", body, want)
		}
	}
}

// Ratchet (SPA): a superseded original whose superseder is listed open is
// exactly one muted, chevron-less sub-row with the short note, never a
// full row of its own; a closed original leaves with its superseder.
func TestStageTrackerSupersededRowRatchetJS(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+jsFunc(t, html, "stageTrackerLinkedSupersededText")+"\n"+jsFunc(t, html, "stageTrackerLinkedSupersededTip")+"\n"+stageTrackerRowsFixtureJS+`
const since = new Date(now - 3600000).toISOString();
const pr = (n, extra) => Object.assign({ repo: 'hivecommons/hive', number: n, kind: 'pr', title: 'dashboard: split stage_tracker.go into 4 files, a very long title that must ellipsize before the note does', url: 'https://x/' + n, stage: 74, stage_since: since, caption: 'Adopted', author: 'odttlnt', author_kind: 'clanker', modifiers: [] }, extra || {});
const issue = (n, prs, extra) => Object.assign({ repo: 'hivecommons/hive', number: n, kind: 'issue', title: 'Issue ' + n, url: 'https://x/i' + n, stage: 40, stage_since: since, linked_prs: prs }, extra || {});
const copy = (n, from, extra) => pr(n, Object.assign({ author: 'hivecommons-hive[bot]', author_kind: 'hive', stage: 80, caption: 'Merge queued', adopted_from: { number: from, url: 'https://x/' + from, author: 'odttlnt', state: 'open', text: '↳ adopted from #' + from } }, extra || {}));
const noteOf = (sub) => (/<span class="ham-superseded-note"[^>]*>([^<]*)<\/span>/.exec(sub) || [])[1] || '';
const subsOf = (row) => row.split('<div class="ham-sub').slice(1);

// Nested: the issue lists both; the original is the muted line.
{
  const orig = pr(15526, { adopted_by: 15574, adopted_by_state: '', superseded_by: 15574, nested_under: 15511 });
  const cp = copy(15574, 15526, { nested_under: 15511 });
  const items = [issue(15511, [orig, cp]), orig, cp];
  const rows = stageTrackerVisible(items, { kind: 'all', repo: '' });
  assert.deepStrictEqual(rows.map(r => r.kind + '#' + r.number), ['issue#15511'], 'the nested pair draws under the issue only');
  assert.deepStrictEqual(stageTrackerLinkedSorted(rows[0]).map(lp => lp.number), [15574, 15526], 'the copy first, the muted original after it');
  const row = stageTrackerRowHtml(rows[0], stages, ctx);
  const subs = subsOf(row);
  assert.strictEqual(subs.length, 2, 'two sub-rows: ' + row);
  const dim = subs.find(s => s.includes('ham-sub-superseded')) || '';
  assert.ok(dim && dim.includes('pr#15526"'), 'the original is the muted sub-row: ' + row);
  assert.strictEqual(noteOf(dim), 'superseded · hive adopted → #15574');
  assert.ok(!dim.includes('aria-label="Pipeline progress"'), 'no chevron track');
  assert.ok(dim.includes('ham-pr-title'), 'the title is in its own ellipsizing span, the note is a non-shrinking sibling');
  assert.ok(/<span class="ham-superseded-note" title="Hive re-authored this PR as #15574 with the same commits and sign-off\. This PR closes as superseded when #15574 merges\."/.test(dim), 'hover carries the long form: ' + dim);
  assert.strictEqual((row.match(/aria-label="Pipeline progress"/g) || []).length, 2, 'the issue strip and the copy strip only');
}
// Closed original, copy open: the same muted line with "closed" in the note.
{
  const closed = pr(15526, { state: 'closed', superseded_by: 15574, caption: 'superseded · closed · hive adopted → #15574' });
  const cp = copy(15574, 15526, { adopted_from: { number: 15526, url: 'https://x/15526', author: 'odttlnt', state: 'closed', text: '↳ adopted from #15526' } });
  const row = stageTrackerRowHtml(issue(15511, [closed, cp]), stages, ctx);
  const dim = subsOf(row).find(s => s.includes('ham-sub-superseded')) || '';
  assert.strictEqual(noteOf(dim), 'superseded · closed · hive adopted → #15574', row);
  assert.ok(dim.includes('This PR is closed as superseded.'));
  // Copy gone (merged / closed): the closed original is not drawn.
  assert.deepStrictEqual(stageTrackerLinkedSorted(issue(15511, [closed])).map(lp => lp.number), []);
  assert.deepStrictEqual(stageTrackerLinkedSorted(issue(15511, [closed, copy(15574, 15526, { state: 'closed' })])).map(lp => lp.number), []);
}
// Ratchet over a mixed fixture: every drawn PR whose superseded_by names a
// drawn open PR under the same issue is the muted line, never a full row.
{
  const orig = pr(15526, { adopted_by: 15574, superseded_by: 15574, nested_under: 15511 });
  const cp = copy(15574, 15526, { nested_under: 15511 });
  const loose = pr(15561, { adopted_by: 15567, superseded_by: 15567, nested_under: 15560 });
  const looseCopy = copy(15567, 15561, { nested_under: 15560 });
  const items = [issue(15511, [orig, cp]), issue(15560, [loose, looseCopy]), orig, cp, loose, looseCopy, pr(15600, { stage: 70, caption: 'Fixing' })];
  for (const f of [{ kind: 'all', repo: '' }, { kind: 'pr', repo: '' }]) {
    const rows = stageTrackerVisible(items, f);
    for (const r of rows) {
      if (r.kind !== 'issue') continue;
      const html = stageTrackerRowHtml(r, stages, ctx);
      const listed = new Set(stageTrackerLinkedSorted(r).filter(lp => String(lp.state || '') !== 'closed').map(lp => lp.number));
      for (const sub of subsOf(html)) {
        const n = Number((/pr#(\d+)"/.exec(sub) || [])[1]);
        const lp = r.linked_prs.find(x => x.number === n);
        if (lp && Number(lp.superseded_by) > 0 && listed.has(Number(lp.superseded_by))) {
          assert.ok(sub.includes('ham-sub-superseded') && !sub.includes('aria-label="Pipeline progress"'), 'ratchet: superseded #' + n + ' must be the muted line, not a full row: ' + sub);
          assert.ok(/^superseded · (closed · )?hive adopted → #\d+$/.test(noteOf(sub)), 'ratchet: note shape: ' + noteOf(sub));
        }
      }
    }
    const top = rows.filter(r => r.kind === 'pr').map(r => r.number);
    if (f.kind === 'all') assert.ok(!top.includes(15526) && !top.includes(15561), 'nested superseded originals never float to the top level: ' + top.join(','));
  }
}
`)
}
