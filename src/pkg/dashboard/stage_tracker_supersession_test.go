package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/worksource"
)

func TestTrackerSupersessionSurvivesEscalationAndStaleRepairStage(t *testing.T) {
	useRingSupersessionAudit(t)
	s := newTestServer()
	now := time.Now().UTC()
	status := minimalPayload()
	status.Repos = []FrontendRepo{{Name: "hive", Full: "hivecommons/hive", OpenPrs: []any{
		FrontendPR{PullRequest: github.PullRequest{Number: 14795, Title: "duplicate", Author: "contributor", Labels: []string{"stage/072-returned-to-contributor", "sentinel-alert"}, LinkedIssues: []github.PRLinkedIssue{{Number: 14789, State: "closed"}}}},
	}}}
	detail := "repo=hive, pr=14795, issue=hivecommons/hive#14789, closer_pr=14791, reason=extra implementation files"
	s.audit.LogRecordAt(now.Add(-time.Minute).Format(time.RFC3339), "system", supersessionDetectedAuditAction, detail, "", "hive", 14795)
	s.audit.LogRecordAt(now.Format(time.RFC3339), "system", github.SupersessionSweepAuditPrefix+github.SupersessionActionEscalated, detail, "", "hive", 14795)
	store := stagelabels.NewStore("", nil)
	store.Observe(stagelabels.Key("hivecommons/hive", 14795), stagelabels.ReturnedToContributor, "check-failing:unit")
	resp := s.buildStageTracker(status, store, now)
	if len(resp.Items) != 1 {
		t.Fatalf("items=%+v", resp.Items)
	}
	it := resp.Items[0]
	if it.Superseded == nil || it.Superseded.CloserPR != 14791 || !strings.Contains(it.Caption, "Superseded by #14791 — decide") || !strings.Contains(it.Caption, "extra implementation files") {
		t.Fatalf("supersession lost to repair stage: %+v", it)
	}
	if it.Actor != "maintainer" {
		t.Fatal(it.Actor)
	}
	s.audit.LogRecord("owner", supersessionKeepOpenAction, "", "", "hive", 14795)
	if it := s.buildStageTracker(status, store, now).Items[0]; it.Superseded != nil {
		t.Fatalf("kept-open still needs decision: %+v", it)
	}
}

func TestClosedSnapshotIsNeverActionable(t *testing.T) {
	raw := []any{map[string]any{"number": 1, "state": "closed"}, map[string]any{"number": 2, "state": "open"}}
	var offered []string
	forEachActionableIssue(nil, "test", "o/r", raw, func(_ map[string]any, ref worksource.Ref) { offered = append(offered, ref.Key()) })
	if len(offered) != 1 || offered[0] != "o/r#2" {
		t.Fatalf("offered=%v", offered)
	}
}

func TestSupersededRepairRowHasDecisionActionAndHeadline(t *testing.T) {
	html := indexHTML(t)
	script := stageTrackerRowActionsPreamble(t, html) + "\n" + jsFunc(t, html, "stageTrackerHeadline") + "\n" + jsFunc(t, html, "stageTrackerCurrentStageHtml") + `
 const item = {kind:'pr',repo:'o/r',number:12,stage:72,superseded:{closer_pr:22}};
 assert.strictEqual(stageTrackerHeadline(item), 'Superseded by #22 — decide');
 assert.ok(stageTrackerCurrentStageHtml(item, [], null, 0).includes('Superseded by #22'));
 assert.strictEqual(stageTrackerDecisionTarget(item), item);
 assert.ok(stageTrackerRowActions(item, {canHold:true}).some(a => a.id === 'decide'));
 `
	runStageTrackerRowActionsScript(t, script)
}
