package retro

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

func TestAutonomySignalDetectorsCarryScopeFields(t *testing.T) {
	rec := RetroRecord{
		BeadID:                      "run-1",
		Actor:                       "alice",
		PRRef:                       "hivecommons/hive#99",
		PRState:                     "merged",
		PlanRevisionsObserved:       true,
		PlanRevisionsBeforeApproval: 0,
		PRReworkObserved:            true,
		PRReworkCommitsAfterReview:  0,
		RollbackEventsObserved:      true,
		RollbackEvents:              1,
		ScopeRepo:                   "hivecommons/hive",
		ScopeChangeClass:            "docs",
		AutonomyLevel:               "L4",
	}

	got := Detect(rec, Thresholds{})
	want := []string{PatternPlanAcceptedFirstPass, PatternPRMergedNoRework, PatternRunRolledBack}
	if len(got) != len(want) {
		t.Fatalf("findings = %d, want %d: %#v", len(got), len(want), got)
	}
	for i, pattern := range want {
		if got[i].Pattern != pattern {
			t.Fatalf("finding[%d].Pattern = %q, want %q", i, got[i].Pattern, pattern)
		}
		if got[i].Fields[metadataAutonomyScopeType] != "repo" || got[i].Fields[metadataAutonomyScopeValue] != "hivecommons/hive" {
			t.Fatalf("finding[%d] scope fields = %#v", i, got[i].Fields)
		}
		if got[i].Fields[metadataAutonomyLevel] != "L4" {
			t.Fatalf("finding[%d] level = %q, want L4", i, got[i].Fields[metadataAutonomyLevel])
		}
	}
	if got[2].Fields[metadataAutonomyDirection] != "should lose" {
		t.Fatalf("rollback direction = %q, want should lose", got[2].Fields[metadataAutonomyDirection])
	}
}

func TestAutonomySignalAdvisoryGolden(t *testing.T) {
	source := newStore(t, "autonomy-source")
	retroStore := newStore(t, "autonomy-retro")
	b, err := source.Create("smooth run", beads.TypeTask, beads.PriorityMedium, "alice", "hivecommons/hive#8313")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Update(b.ID, func(bd *beads.Bead) {
		bd.Status = beads.StatusClosed
		bd.Metadata = map[string]interface{}{
			"pr_ref":                         "hivecommons/hive#9001",
			"pr_state":                       "merged",
			"plan_revisions_before_approval": "0",
			"pr_rework_commits_after_review": "0",
			"rollback_events":                "0",
			"scope_repo":                     "hivecommons/hive",
			"scope_change_class":             "scheduler",
			"autonomy_level":                 "L4",
		}
	}); err != nil {
		t.Fatal(err)
	}
	lane := NewLane(map[string]*beads.Store{"alice": source, Actor: retroStore}, retroStore, nil, nil, Config{ScanIntervalS: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if created := lane.Run(context.Background()); created != 2 {
		t.Fatalf("created = %d, want first-pass plan + no-rework PR advisories", created)
	}
	advisories := retroStore.List(beads.ListFilter{})
	if len(advisories) != 2 {
		t.Fatalf("advisories = %d, want 2", len(advisories))
	}
	for _, adv := range advisories {
		if !strings.HasPrefix(adv.Title, "autonomy signal: hivecommons/hive qualifies for L4") {
			t.Fatalf("advisory title = %q", adv.Title)
		}
		if adv.Meta(metadataAutonomyScopeType) != "repo" || adv.Meta(metadataAutonomyScopeValue) != "hivecommons/hive" {
			t.Fatalf("advisory scope metadata = %#v", adv.Metadata)
		}
		if adv.Meta(metadataAutonomyDirection) != "qualifies" || adv.Meta(metadataAutonomyChangeClass) != "scheduler" {
			t.Fatalf("advisory autonomy metadata = %#v", adv.Metadata)
		}
	}
}

func TestAutonomySignalMalformedCountsAreNotObserved(t *testing.T) {
	source := newStore(t, "autonomy-malformed-source")
	b, err := source.Create("unknown run", beads.TypeTask, beads.PriorityMedium, "alice", "hivecommons/hive#8313")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Update(b.ID, func(bd *beads.Bead) {
		bd.Status = beads.StatusClosed
		bd.Metadata = map[string]interface{}{
			"pr_ref":                         "hivecommons/hive#9002",
			"pr_state":                       "merged",
			"plan_revisions_before_approval": "unknown",
			"pr_rework_commits_after_review": "n/a",
			"scope_repo":                     "hivecommons/hive",
		}
	}); err != nil {
		t.Fatal(err)
	}
	gotBead, err := source.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec := Reconstruct(gotBead, nil, nil)
	if rec.PlanRevisionsObserved || rec.PRReworkObserved {
		t.Fatalf("malformed counts should not be observed: %#v", rec)
	}
	if got := Detect(rec, Thresholds{}); len(got) != 0 {
		t.Fatalf("malformed counts emitted findings: %#v", got)
	}
}
