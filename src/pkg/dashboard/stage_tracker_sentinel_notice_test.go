package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestStageTrackerSentinelNoticePreservesActualReviewerDecision(t *testing.T) {
	const question = "@hanthor: may the protected build-and-test context become a three-lane aggregate gated by required-context-scope success?"
	it := StageTrackerItem{
		Kind: "pr", Repo: "hivecommons/hive", Number: 16026,
		Head:  "32fe2056576fce6ddf93a51f1dd17431575679d7",
		Stage: stagelabels.NeedsDecision.Number(), Reason: stageengine.StageReasonVerdictRequiresHuman,
		Author: "clubanderson", AuthorKind: authorKindHuman,
		SentinelAlert: true, SentinelNotice: true, SentinelReason: "workflow changes by a trusted author",
	}
	verdict := &review.Aggregate{HeadSHA: it.Head, DecisionQuestion: question}
	d := stageTrackerDecisionNeeded(it, verdict, "sentinel-alert")
	if d == nil || d.Question != question || d.AskedBy != decisionAskedByReviewer ||
		strings.Contains(d.Blocker, "security check") || strings.Contains(d.Blocker, "sentinel-alert") {
		t.Fatalf("informational notice displaced the actual reviewer question: %+v", d)
	}
	it.DecisionNeeded = d
	actor := stageTrackerStageActor(it, nil, time.Now())
	if actor.Kind != StageActorHuman || actor.Login != "" || !strings.Contains(actor.Why, "@hanthor") ||
		!strings.Contains(actor.Why, "reviewer question:") || strings.Contains(actor.Why, "clubanderson") || strings.Contains(actor.HumanReason, "sentinel") {
		t.Fatalf("author or notice became the decision owner/reason: %+v", actor)
	}
	it.StageActor = actor
	payload, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+stageTrackerRowsFixtureJS+
		"\nconst notice = "+string(payload)+";\n"+`
assert.strictEqual(stageTrackerSentinelInLane(notice), false);
assert.strictEqual(stageTrackerSentinelAsSuffix({...notice, stage:71}), false);
assert.ok(stageTrackerSentinelTip(notice).includes('Informational security notice (not a merge gate)'));
assert.ok(!stageTrackerSentinelTip({...notice, sentinel_reason:''}).includes('must review before merge'));
const own = stageTrackerRowHtml(notice, stages, ctx);
assert.ok(!text(own).includes('Sentinel alert'), text(own));
assert.ok(!text(own).includes('Needs @clubanderson'), text(own));
assert.ok(own.includes('@hanthor'), own);
const linked = stageTrackerLinkedPRItem({repo:notice.repo, number:500}, notice);
assert.strictEqual(linked.sentinel_notice, true, 'canonical linked adapter preserves notice semantics');
const inline = stageTrackerRowHtml({repo:notice.repo, kind:'issue', number:500, stage:40, linked_prs:[notice]}, stages, ctx);
assert.ok(!text(inline).includes('Sentinel alert'), text(inline));
assert.ok(!text(inline).includes('Needs @clubanderson'), text(inline));
const nested = stageTrackerRowHtml({repo:notice.repo, kind:'issue', number:500, stage:40,
  linked_prs:[notice, {...notice, number:16099, sentinel_alert:false, sentinel_notice:false, stage:50}]}, stages, ctx);
assert.ok(!text(nested).includes('Sentinel alert'), text(nested));
assert.ok(!text(nested).includes('Needs @clubanderson'), text(nested));
const blocking = {...notice, sentinel_notice:false};
assert.strictEqual(stageTrackerSentinelInLane(blocking), true, 'actual blocking alert remains a sentinel gate');
assert.strictEqual(stageTrackerSentinelAsSuffix({...blocking, stage:71}), true);
assert.ok(!stageTrackerSentinelTip(blocking).includes('Informational'));
`)
	it.SentinelNotice = false
	blocking := stageTrackerDecisionNeeded(it, verdict, "sentinel-alert")
	if blocking == nil || !strings.Contains(blocking.Blocker, "sentinel-alert") || blocking.Question != question {
		t.Fatalf("genuine alert or coexisting reviewer question was suppressed: %+v", blocking)
	}
}
