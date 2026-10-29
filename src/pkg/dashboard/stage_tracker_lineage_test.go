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

func TestStageTrackerLiveAdoptionLineage(t *testing.T) {
	now := time.Date(2026, 10, 29, 10, 56, 0, 0, time.UTC)
	for _, closeFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "original-open", true: "close-first"}[closeFirst], func(t *testing.T) {
			s := newTestServer()
			store := stagelabels.NewStore("", func() time.Time { return now })
			s.stageStore = store
			s.deps = &Dependencies{Config: &config.Config{}}
			key := stagelabels.Key("hivecommons/hive", 15226)
			store.Observe(key, stagelabels.AdoptedByHive, "contributor-lane:adopted")
			store.NoteAuthor(key, "thatguywob")
			store.MarkAdopted(key, "oldhead", 15308, now.Add(-6*time.Hour))
			store.NoteAdoptReason(key, "contributor-offline")
			if closeFirst {
				store.MarkSupersededClosedFirst(key, 15308, false, now.Add(-6*time.Hour))
			}
			status := minimalPayload()
			status.Repos = []FrontendRepo{{Name: "hive", Full: "hivecommons/hive",
				ActionableIssues: []any{github.Issue{Number: 15198, Title: "Fix this", Labels: []string{"stage/040-covered-by-pr"}, LinkedPRs: []github.IssueLinkedPR{{Number: 15308, State: "open", Closing: true}}}},
				OpenPrs:          []any{FrontendPR{PullRequest: github.PullRequest{Number: 15308, Author: "hivecommons-hive[bot]", AppAuthored: true, Title: "Adopted fix", Labels: []string{"stage/070-fixing"}, LinkedIssues: []github.PRLinkedIssue{{Number: 15198, State: "open"}}}}},
			}}
			if !closeFirst {
				status.Repos[0].OpenPrs = append(status.Repos[0].OpenPrs, FrontendPR{PullRequest: github.PullRequest{Number: 15226, Author: "thatguywob", Labels: []string{"author/clanker", "stage/074-adopted-by-hive"}}})
			}
			s.UpdateStatus(status)
			resp := s.buildStageTracker(status, store, now)
			pr := trackerItem(t, resp, "pr", 15308)
			issue := trackerItem(t, resp, "issue", 15198)
			l := pr.Lineage
			if l == nil || l.Kind != "adopted" || l.FromNumber != 15226 || l.FromAuthor != "thatguywob" || l.FromURL != "https://github.com/hivecommons/hive/pull/15226" || l.ToNumber != 15308 || l.ToURL != "https://github.com/hivecommons/hive/pull/15308" || l.At == nil || !l.At.Equal(now.Add(-6*time.Hour)) || l.Reason != "contributor-offline" {
				t.Fatalf("lineage = %+v", l)
			}
			if !closeFirst && l.FromAuthorKind != "clanker" {
				t.Fatalf("cached original kind = %q", l.FromAuthorKind)
			}
			if len(issue.LinkedPRs) != 1 || issue.LinkedPRs[0].Lineage != l {
				t.Fatalf("linked lineage = %+v", issue.LinkedPRs)
			}
			if !strings.Contains(pr.Caption, "(adopted from #15226)") || !strings.Contains(issue.Caption, "(adopted from #15226)") {
				t.Fatalf("captions: %q / %q", pr.Caption, issue.Caption)
			}
			raw, err := json.Marshal(resp)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(raw), `"lineage":{"kind":"adopted"`) != 2 {
				t.Fatalf("missing top-level or linked lineage: %s", raw)
			}
		})
	}
}

func TestStageTrackerLineageRecordWinsAndIsRepoScoped(t *testing.T) {
	now := time.Date(2026, 10, 29, 4, 56, 0, 0, time.UTC)
	store := stagelabels.NewStore("", func() time.Time { return now })
	key := stagelabels.Key("acme/repo", 7)
	store.Observe(key, stagelabels.AdoptedByHive, "adopted")
	store.MarkAdopted(key, "head", 9, now)
	index := stageTrackerLineageIndex(store, "")
	pr := github.PullRequest{Number: 9, AppAuthored: true, HeadRef: "hive/adopt-8"}
	if l := stageTrackerPRLineage(index, "acme/repo", pr, ""); l == nil || l.FromNumber != 7 || l.At == nil {
		t.Fatalf("record did not win: %+v", l)
	}
	pr.HeadRef = "ordinary"
	if l := stageTrackerPRLineage(index, "another/repo", pr, ""); l != nil {
		t.Fatalf("cross-repo lineage: %+v", l)
	}
}

func TestStageTrackerLineageBranchFallback(t *testing.T) {
	for _, tc := range []struct {
		branch string
		app    bool
		want   bool
	}{
		{"hive/adopt-15226", true, true}, {"hive/adopt-15226", false, false},
		{"hive/adopt-0", true, false}, {"hive/adopt-abc", true, false},
		{"hive/adopt-15226-extra", true, false}, {"hive/adopt-15308", true, false},
		{"ordinary", true, false},
	} {
		pr := github.PullRequest{Number: 15308, AppAuthored: tc.app, HeadRef: tc.branch}
		l := stageTrackerPRLineage(nil, "acme/repo", pr, "https://git.example")
		if (l != nil) != tc.want {
			t.Fatalf("%+v: %+v", tc, l)
		}
		if l != nil && (l.FromURL != "https://git.example/acme/repo/pull/15226" || l.FromAuthor != "" || l.At != nil || l.Reason != "") {
			t.Fatalf("fallback fabricated facts: %+v", l)
		}
	}
	if len(stageTrackerLineageIndex(nil, "")) != 0 {
		t.Fatal("nil store")
	}
}

func TestStageTrackerLineageJS(t *testing.T) {
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		jsFunc(t, html, "esc"), jsFunc(t, html, "stageTrackerAgoText"),
		jsFunc(t, html, "stageTrackerLineageClause"), jsFunc(t, html, "stageTrackerLineageChipHtml"),
		jsFunc(t, html, "stageTrackerSupersedesTip"), jsFunc(t, html, "stageTrackerSupersedesChipHtml"),
		jsFunc(t, html, "stageTrackerLinkedPRItem"), jsFunc(t, html, "stageTrackerLinkedPROpen"),
		jsFunc(t, html, "stageTrackerFetchDelegatePlan"),
		"function stageTrackerActionRequest() { return {url:'/plan', body:{}}; }",
		"function stageTrackerRefOf(it) { return '#' + it.number; }",
		"const STAGE_TRACKER_TERMINAL = 100;",
		`
const now = Date.parse('2026-10-29T10:56:00Z');
const l = {kind:'adopted', from_number:15226, from_url:'https://github.com/hivecommons/hive/pull/15226', from_author:'thatguywob', at:'2026-10-29T04:56:00Z', reason:'contributor-offline'};
const pr = {kind:'pr', number:15308, stage:70, lineage:l};
assert.strictEqual(stageTrackerLineageClause(pr), '(adopted from #15226)');
assert.strictEqual(stageTrackerLineageClause({}), '');
assert.strictEqual(stageTrackerLineageChipHtml({}, now), '');
const chip = stageTrackerLineageChipHtml(pr, now);
assert.ok(chip.includes("adopted from @thatguywob&#39;s #15226 · 6h ago"), chip);
assert.ok(chip.includes('href="' + l.from_url + '"'), chip);
assert.ok(chip.includes('Original commits kept.'), chip);
assert.ok(chip.includes('after CI went red'), chip);
assert.strictEqual(stageTrackerSupersedesChipHtml(Object.assign({supersedes:{number:15226}}, pr), now), '');
const sub = stageTrackerLinkedPRItem({repo:'hivecommons/hive', number:15198, linked_prs:[pr]}, pr);
assert.strictEqual(sub.lineage, l);
assert.strictEqual(stageTrackerLinkedPRItem({number:1}, {number:2}).lineage, null);
const unknown = stageTrackerLineageChipHtml({lineage:{kind:'adopted', from_number:7}}, now);
assert.ok(!unknown.includes('undefined') && !unknown.includes('after CI went red'), unknown);
const hostile = stageTrackerLineageChipHtml({lineage:Object.assign({}, l, {from_author:'<img>', from_url:'https://example.org/"x'})}, now);
assert.ok(!hostile.includes('<img>') && hostile.includes('&lt;img&gt;'), hostile);
global.fetch = async () => ({ok:true, json:async () => ({ref:'server-ref', number:15308})});
(async () => {
  const plan = await stageTrackerFetchDelegatePlan(pr);
  assert.strictEqual(plan.ref, 'server-ref (adopted from #15226)');
})().catch(e => { console.error(e); process.exit(1); });
`,
	}, "\n")
	runStageTrackerRowActionsScript(t, script)
	for _, want := range []string{"stageTrackerLineageChipHtml(pr, nowMs)", "stageTrackerLineageChipHtml(item, nowMs)", "stageTrackerLineageChipHtml(lp, nowMs)", "(lineage ? ' ' + lineage : '')"} {
		if !strings.Contains(html, want) {
			t.Errorf("lineage not wired: %s", want)
		}
	}
}
