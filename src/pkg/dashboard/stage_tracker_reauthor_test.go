package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func reauthorStore(t *testing.T, now time.Time, oldPR, issue int) *stagelabels.Store {
	t.Helper()
	clock := now.Add(-10 * time.Minute)
	store := stagelabels.NewStore("", func() time.Time { return clock })
	store.Observe(stagelabels.Key("acme/docs", oldPR), stagelabels.Fixing, stagelabels.ReauthoringReason(issue))
	store.MarkReauthored(stagelabels.Key("acme/docs", oldPR), issue, "deadbeef", 6, clock)
	return store
}

// The engine re-authored hive PR #90 for issue #12 (fixer exhausted). Until
// the fresh PR is listed the issue sits in PR-in-progress as
// "Re-authoring · superseding #90" with kick evidence; once hive PR #95
// closing #12 is listed it carries lineage re-authored-from-#90 and the
// issue's native words return.
func TestStageTrackerApplyReauthor(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	store := reauthorStore(t, now, 90, 12)
	issue := StageTrackerItem{Repo: "acme/docs", Number: 12, Kind: "issue", Stage: trackerNumberActionable, Caption: "ready for pickup"}

	t.Run("issue alone words the re-author", func(t *testing.T) {
		items := []StageTrackerItem{issue}
		stageTrackerApplyReauthor(items, store, "", now)
		it := items[0]
		if it.Caption != "Re-authoring · superseding #90" || it.Actor != authorKindHive || it.DisplayColumn != trackerNumberPRInProgress {
			t.Fatalf("issue row = caption %q actor %q column %d", it.Caption, it.Actor, it.DisplayColumn)
		}
		if it.Remediation == nil || it.Remediation.Kind != remediationKindKick || it.Remediation.Actor != authorKindHive || it.Remediation.Since == nil || it.Remediation.Source != "re-author" {
			t.Fatalf("remediation = %+v", it.Remediation)
		}
		if it.CodeActivity == nil || it.CodeActivity.State != codeStateRemediating || !it.CodeActivity.Active || !strings.Contains(it.CodeActivity.Detail, "after 6 attempts") || !strings.Contains(it.CodeActivity.Detail, "One automatic re-author per issue") {
			t.Fatalf("code activity = %+v", it.CodeActivity)
		}
		if it.Stage != trackerNumberActionable {
			t.Fatalf("native stage must stand, got %d", it.Stage)
		}
	})

	t.Run("fresh hive PR carries re-authored lineage and releases the issue", func(t *testing.T) {
		created := now.Add(-2 * time.Minute)
		fresh := StageTrackerItem{Repo: "acme/docs", Number: 95, Kind: "pr", AuthorKind: authorKindHive, CreatedAt: &created,
			LinkedIssues: []StageTrackerLinkedIssue{{Number: 12}}}
		items := []StageTrackerItem{issue, fresh}
		stageTrackerApplyReauthor(items, store, "https://github.com", now)
		l := items[1].Lineage
		if l == nil || l.Kind != lineageReauthored || l.FromNumber != 90 || l.FromState != lineageFromStateClosed || l.Reason != lineageReasonFixerExhaust || l.FromAuthorKind != authorKindHive || !strings.HasSuffix(l.FromURL, "/acme/docs/pull/90") {
			t.Fatalf("lineage = %+v", l)
		}
		if items[0].Caption != "ready for pickup" || items[0].Remediation != nil {
			t.Fatalf("issue must fall back to native words once the successor is listed: %+v", items[0])
		}
	})

	t.Run("a contributor PR or an older PR is not the successor", func(t *testing.T) {
		before := now.Add(-time.Hour)
		clanker := StageTrackerItem{Repo: "acme/docs", Number: 96, Kind: "pr", AuthorKind: authorKindClanker, LinkedIssues: []StageTrackerLinkedIssue{{Number: 12}}}
		older := StageTrackerItem{Repo: "acme/docs", Number: 80, Kind: "pr", AuthorKind: authorKindHive, CreatedAt: &before, LinkedIssues: []StageTrackerLinkedIssue{{Number: 12}}}
		items := []StageTrackerItem{clanker, older, issue}
		stageTrackerApplyReauthor(items, store, "", now)
		if items[0].Lineage != nil || items[1].Lineage != nil || items[2].Caption != "Re-authoring · superseding #90" {
			t.Fatalf("items = %+v", items)
		}
	})

	t.Run("old PR still listed on the re-authoring reason reads the same", func(t *testing.T) {
		old := StageTrackerItem{Repo: "acme/docs", Number: 90, Kind: "pr", AuthorKind: authorKindHive, Stage: trackerNumberFixing, Reason: stagelabels.ReauthoringReason(12), Head: "deadbeef"}
		items := []StageTrackerItem{old}
		stageTrackerApplyReauthor(items, store, "", now)
		if items[0].Caption != "Re-authoring · superseding #90" || items[0].Lineage != nil || items[0].CodeActivity == nil || items[0].CodeActivity.Head != "deadbeef" {
			t.Fatalf("old PR = %+v", items[0])
		}
	})

	t.Run("record older than the window no longer promises motion", func(t *testing.T) {
		items := []StageTrackerItem{issue}
		stageTrackerApplyReauthor(items, store, "", now.Add(stageTrackerReauthorWindow+time.Hour))
		if items[0].Caption != "ready for pickup" {
			t.Fatalf("stale record still words the issue: %q", items[0].Caption)
		}
	})
}
