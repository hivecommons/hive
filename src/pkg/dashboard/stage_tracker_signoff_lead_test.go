package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// Owner on hive#16572 / docs#615 (085, a reviewer asking "@clubanderson
// should decide whether to accept this .github/workflows/fossa.yml change
// from a fork"): "why does review say awaiting humans if 5/5 reviews are
// good and nothing is left to do?" The reviews are done; what is left is
// the sign-off. Approve leads the row (Let hive decide beside it), the
// review word says whose sign-off, and a delegate verdict that recorded
// no recommendation never reads "Recommendation recorded".

func signOffItem(ci, review string, opts ...StageTrackerDecisionOption) StageTrackerItem {
	if opts == nil {
		opts = []StageTrackerDecisionOption{
			decisionOption(decisionActionDelegate, "Let hive decide", true, ""),
			decisionOption(decisionActionApprove, "Approve", true, ""),
			decisionOption(decisionActionDirection, "Give direction", true, ""),
		}
	}
	it := StageTrackerItem{Repo: "hivecommons/hive", Kind: "pr", Number: 16572, Stage: 85, Head: "h1",
		DecisionNeeded: &StageTrackerDecision{AskedBy: decisionAskedByReviewer, Question: "@clubanderson should decide whether to accept this .github/workflows/fossa.yml change from a fork.", Options: opts}}
	if ci != "" {
		it.CIActivity = &StageTrackerActivity{Head: "h1", State: ci}
		if ci == "failing" {
			it.CIActivity.ChecksFailed = 1
		}
	}
	if review != "" {
		it.ReviewActivity = &StageTrackerActivity{Head: "h1", State: review}
	}
	return it
}

func leadActions(d *StageTrackerDecision) []string {
	var out []string
	for _, o := range d.Options {
		if o.Lead {
			out = append(out, o.Action)
		}
	}
	return out
}

func TestStageTrackerLeadApproveForSignOff(t *testing.T) {
	cases := []struct {
		name string
		it   StageTrackerItem
		want []string
	}{
		{"reviewer ask, CI green, reviews done → Approve leads", signOffItem("passing", "human_decision"), []string{decisionActionApprove}},
		{"approved reviews also count", signOffItem("passing", "approved"), []string{decisionActionApprove}},
		{"no CI evidence yet is not failing", signOffItem("", "human_decision"), []string{decisionActionApprove}},
		{"CI failing: never lead with approve", signOffItem("failing", "human_decision"), nil},
		{"reviews still running: no lead", signOffItem("passing", "reviewing"), nil},
		{"no review evidence: no lead", signOffItem("passing", ""), nil},
		{"approve disabled (changes requested): no lead", signOffItem("passing", "human_decision",
			decisionOption(decisionActionDelegate, "Let hive decide", true, ""),
			decisionOption(decisionActionApprove, "Approve", false, "A reviewer requested changes")), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it := tc.it
			stageTrackerLeadApproveForSignOff(&it)
			got := leadActions(it.DecisionNeeded)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("leads = %v, want %v", got, tc.want)
			}
			if len(tc.want) > 0 {
				for _, o := range it.DecisionNeeded.Options {
					if o.Lead && o.Consequence == "" {
						t.Fatalf("lead %s carries no consequence for its hover", o.Action)
					}
				}
			}
		})
	}
	t.Run("sentinel ask leads too", func(t *testing.T) {
		it := signOffItem("passing", "approved")
		it.DecisionNeeded.AskedBy = decisionAskedBySentinel
		stageTrackerLeadApproveForSignOff(&it)
		if got := leadActions(it.DecisionNeeded); len(got) != 1 || got[0] != decisionActionApprove {
			t.Fatalf("leads = %v", got)
		}
	})
	t.Run("the hive's own park is not a sign-off", func(t *testing.T) {
		it := signOffItem("passing", "human_decision")
		it.DecisionNeeded.AskedBy = decisionAskedByLane
		stageTrackerLeadApproveForSignOff(&it)
		if got := leadActions(it.DecisionNeeded); got != nil {
			t.Fatalf("leads = %v", got)
		}
	})
	t.Run("an existing lead (Re-author) stands", func(t *testing.T) {
		it := signOffItem("passing", "human_decision")
		it.DecisionNeeded.Options = decisionLeadWithReauthor(it.DecisionNeeded.Options)
		stageTrackerLeadApproveForSignOff(&it)
		if got := leadActions(it.DecisionNeeded); len(got) != 1 || got[0] != decisionActionReauthor {
			t.Fatalf("leads = %v", got)
		}
	})
	t.Run("issues and rows without an ask are untouched", func(t *testing.T) {
		it := signOffItem("passing", "human_decision")
		it.Kind = "issue"
		stageTrackerLeadApproveForSignOff(&it)
		if leadActions(it.DecisionNeeded) != nil {
			t.Fatal("issue row gained a lead")
		}
		none := StageTrackerItem{Kind: "pr"}
		stageTrackerLeadApproveForSignOff(&none)
	})
}

func TestStageTrackerRowActionResultCarriesVerdict(t *testing.T) {
	got := delegatedRowActionResult(delegatedecision.Input{Repo: "o/r", Kind: delegatedecision.KindPR, Number: 7},
		delegatedecision.Decision{Verdict: delegatedecision.VerdictOwnerOnly, Text: "owner only"}, nil)
	if got.Verdict != string(delegatedecision.VerdictOwnerOnly) || got.Action != rowActionRecommended {
		t.Fatalf("result = %+v", got)
	}
}

// The row as the SPA draws it: Approve ▸ first, Let hive decide ▸ second;
// the review chevron says whose sign-off; a recommend-less verdict's
// caption hands the decision back.
func TestStageTrackerSignOffRowInline(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`' · needs your sign-off'`,
		`' · needs owner sign-off'`,
		`const STAGE_TRACKER_PARK_KEPT_ACTING = {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+`
const ctx = { user: true, canHold: true, canHuman: true, login: 'clubanderson', nowMs: Date.parse('2026-11-04T12:00:00Z'), governor: null };
const row = { repo: 'hivecommons/hive', kind: 'pr', number: 16572, title: 'fossa workflow', url: 'https://github.com/hivecommons/hive/pull/16572', stage: 85, head: 'h1', actor: 'maintainer', modifiers: ['needs-decision'],
  review_activity: { head: 'h1', state: 'human_decision', perspectives_done: 5, perspectives_total: 5 },
  decision_needed: { question: '@clubanderson should decide whether to accept this .github/workflows/fossa.yml change from a fork.', asked_by: 'a reviewer',
    options: [{ action: 'decide:delegate', label: 'Let hive decide', enabled: true }, { action: 'decide:approve', label: 'Approve', enabled: true, lead: true, consequence: 'Posts /lgtm + /approve as you; the merge queue takes it from there. Nothing merges here.' }, { action: 'decide:direction', label: 'Give direction', enabled: true }] } };
const h = stageTrackerLeadActionHtml(row, ctx);
const acts = [...h.matchAll(/data-ham-act="([^"]+)"/g)].map(m => m[1]);
assert.deepStrictEqual(acts, ['decide:approve', 'decide:delegate'], h);
assert.ok(h.includes('class="ham-lead-act ham-act-primary" data-ham-act="decide:approve" title="Posts /lgtm + /approve as you; the merge queue takes it from there. Nothing merges here."'), h);
assert.ok(h.includes('>Approve</button>') && h.includes('>Let hive decide</button>'), h);
`)
	runStageTrackerRowActionsScript(t, `
const assert = require('assert');
const STAGE_TRACKER_DECIDED_ACTING = { adopted: 'Adopting · opening hive copy…', approved: 'Approving…', directed: 'Sending back…', closed: 'Closing…', merged: 'Merging…', 'queued-for-merge': 'Queuing for merge…', recommended: 'Recommendation recorded' };
`+jsConstLine(t, html, "STAGE_TRACKER_PARK_KEPT_ACTING")+`
`+jsFunc(t, html, "stageTrackerDecidedActing")+`
assert.strictEqual(stageTrackerDecidedActing({ action: 'recommended', verdict: 'owner-only' }, {}), 'Your decision needed · owner sign-off');
assert.strictEqual(stageTrackerDecidedActing({ action: 'recommended', verdict: 'operator-action' }, {}), 'Your action needed · operator step');
assert.strictEqual(stageTrackerDecidedActing({ action: 'recommended', verdict: 'recommend-only' }, {}), 'Recommendation recorded');
assert.strictEqual(stageTrackerDecidedActing({ action: 'recommended' }, {}), 'Your decision needed', 'no verdict, no recommendation claimed');
assert.strictEqual(stageTrackerDecidedActing({ action: 'adopted' }, {}), 'Adopting · opening hive copy…');
assert.strictEqual(stageTrackerDecidedActing({ action: 'recommended', verdict: 'owner-only' }, { decided: { acting: 'from server' } }), 'from server');
`)
}
