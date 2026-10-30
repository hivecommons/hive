package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#15734 as the hive filed it: a dedupe checklist parked
// at 045 with "Decision required: yes", one of three items already ticked.
const advisory15734Body = "<!-- hive:dedupe-checklist repo=acme/console -->\nDecision required: yes\n\n" +
	"## Summary\n\nFound 3 near-duplicate pairs, 0 same-root-cause clusters, 0 items that appear already resolved, and 0 missing PR↔issue links.\n\n" +
	"## Near-duplicates: close one\n\n" +
	"- [x] #15312 — close as superseded by #15646\n" +
	"- [ ] #15433 — close as superseded by #15649\n" +
	"- [ ] #15482 — close as superseded by #15650\n"

var advisory15734Labels = []string{"help wanted", "agent/dedupe", "stage/045-needs-decision"}

func TestStageTrackerAdvisoryWorkingWording(t *testing.T) {
	adv := stageTrackerAdvisoryFor(advisory15734Labels, advisory15734Body, nil)
	if adv == nil || adv.Source != stageTrackerAdvisorySourceChecklist || adv.Items != 3 || adv.Resolved != 1 {
		t.Fatalf("#15734 advisory = %+v", adv)
	}
	stageTrackerAdvisoryWorking(adv, true)
	want := "Advisory · hive is working the items (1/3 resolved)"
	if !adv.Working || adv.Chip != want || adv.Caption != want || adv.Lead != "Close now" {
		t.Errorf("working advisory = %+v", adv)
	}

	off := stageTrackerAdvisoryFor(advisory15734Labels, advisory15734Body, nil)
	stageTrackerAdvisoryWorking(off, false)
	if off.Working || off.Chip != StageTrackerAdvisoryChip || off.Lead != "" {
		t.Errorf("auto-close off = %+v, want the read & close advisory", off)
	}
	// No task list: nothing for the hive to work.
	plain := stageTrackerAdvisoryFor([]string{"triage/advisory"}, "", nil)
	stageTrackerAdvisoryWorking(plain, true)
	if plain.Working || plain.Chip != StageTrackerAdvisoryChip {
		t.Errorf("label-only advisory = %+v", plain)
	}
}

func TestStageTrackerAdvisoryAutocloseKnob(t *testing.T) {
	off := false
	if !stageTrackerAdvisoryAutoclose(nil) || !stageTrackerAdvisoryAutoclose(&config.Config{}) {
		t.Error("unset knob must read as on")
	}
	if stageTrackerAdvisoryAutoclose(&config.Config{Dedupe: config.DedupeAdvisoryConfig{AdvisoryAutoclose: &off}}) {
		t.Error("knob set false must read as off")
	}
}

// An advisory is never parked on a person: the 045 park, its pill and the
// decision / blocker blocks come off, and the hive is the actor while it
// works the items.
func TestApplyStageTrackerAdvisoryNeverParks(t *testing.T) {
	adv := stageTrackerAdvisoryFor(advisory15734Labels, advisory15734Body, nil)
	stageTrackerAdvisoryWorking(adv, true)
	it := StageTrackerItem{Kind: "issue", Number: 15734, Stage: 45, StageSlug: "needs-decision", StageLabel: "Needs human decision",
		Modifiers: []string{modNeedsDecision, modStalled}, DecisionNeeded: &StageTrackerDecision{}, Blocker: &StageTrackerBlocker{}}
	applyStageTrackerAdvisory(&it, adv)
	if it.Stage != 0 || it.StageSlug != "" || it.StageLabel != "" {
		t.Errorf("stage = %d/%q/%q, want the 045 park dropped", it.Stage, it.StageSlug, it.StageLabel)
	}
	if it.DecisionNeeded != nil || it.Blocker != nil || hasMod(it, modNeedsDecision) || hasMod(it, modStalled) {
		t.Errorf("decision=%+v blocker=%+v mods=%v, want no park on a person", it.DecisionNeeded, it.Blocker, it.Modifiers)
	}
	if it.Caption != adv.Caption || it.Actor != StageActorHive || it.StageActor == nil || it.StageActor.Kind != StageActorHive {
		t.Errorf("caption=%q actor=%q stage_actor=%+v", it.Caption, it.Actor, it.StageActor)
	}
	if stageTrackerNeedsHuman(it) {
		t.Error("advisory counted as needs-human")
	}
	if kind, _ := stageTrackerWaitKind(it); kind != "" {
		t.Errorf("wait kind = %q, want none", kind)
	}
}

// The built row for #15734: the enumeration's hint carries the counts, the
// row reads "hive is working the items" with no decision block.
func TestStageTrackerAdvisoryWorkingRow(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	created := now.Add(-3 * time.Hour)

	s.statusMu.Lock()
	repo := &s.status.Repos[0]
	repo.NonActionableIssues = []any{
		github.RepoWorkIssue{Repo: "acme/console", Number: 15734, Title: "triage: duplicate and overlapping open issues/PRs to consolidate", Author: "hive-app[bot]",
			Labels: advisory15734Labels, Bucket: "needs-decision", Reason: "parked", CreatedAt: created, UpdatedAt: created,
			Advisory: github.IssueAdvisoryFromBody(advisory15734Body)},
	}
	s.statusMu.Unlock()

	resp := s.buildStageTracker(s.status, store, now)
	it := trackerItem(t, resp, "issue", 15734)
	if it.Advisory == nil || !it.Advisory.Working || it.Advisory.Lead != "Close now" {
		t.Fatalf("#15734 advisory = %+v", it.Advisory)
	}
	if want := "Advisory · hive is working the items (1/3 resolved)"; it.Caption != want || it.Advisory.Chip != want {
		t.Errorf("#15734 caption/chip = %q / %q", it.Caption, it.Advisory.Chip)
	}
	if it.DecisionNeeded != nil || it.Stage != 0 || it.NeedsHuman || it.WaitingOnYou || hasMod(it, modNeedsDecision) {
		t.Errorf("#15734 parked on a person: decision=%+v stage=%d needs_human=%v waiting=%v mods=%v", it.DecisionNeeded, it.Stage, it.NeedsHuman, it.WaitingOnYou, it.Modifiers)
	}
}
