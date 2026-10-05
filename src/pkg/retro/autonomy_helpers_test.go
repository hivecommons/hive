package retro

import (
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/timeline"
)

// The autonomy helpers below are pure functions that the policy engine and
// Lane.Run route every stage attribute through. The engine-level tests only
// reach the happy arm of each; these pin every fallback arm directly.

func TestAutonomyScopePrecedence(t *testing.T) {
	cases := []struct {
		name      string
		rec       RetroRecord
		wantType  string
		wantValue string
	}{
		{"user wins over repo and class", RetroRecord{ScopeUser: "alice", ScopeRepo: "o/r", ScopeChangeClass: "docs", Actor: "bob", BeadID: "b1"}, "user", "alice"},
		{"repo when no user", RetroRecord{ScopeRepo: "o/r", ScopeChangeClass: "docs", Actor: "bob", BeadID: "b1"}, "repo", "o/r"},
		{"change class when no user or repo", RetroRecord{ScopeChangeClass: "docs", Actor: "bob", BeadID: "b1"}, "change_class", "docs"},
		{"actor falls back to user scope", RetroRecord{Actor: "bob", BeadID: "b1"}, "user", "bob"},
		{"run scope when nothing else is known", RetroRecord{BeadID: "b1"}, "run", "b1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotValue := autonomyScope(tc.rec)
			if gotType != tc.wantType || gotValue != tc.wantValue {
				t.Fatalf("autonomyScope = (%q, %q), want (%q, %q)", gotType, gotValue, tc.wantType, tc.wantValue)
			}
		})
	}
}

func TestAutonomyLevelFallback(t *testing.T) {
	if got := autonomyLevel(RetroRecord{AutonomyLevel: "L5"}, "L3"); got != "L5" {
		t.Fatalf("explicit level = %q, want L5", got)
	}
	if got := autonomyLevel(RetroRecord{}, "L3"); got != "L3" {
		t.Fatalf("fallback level = %q, want L3", got)
	}
}

func TestFirstAttrIntSkipsEmptyAndNonNumeric(t *testing.T) {
	st := &timeline.Stage{Attrs: map[string]string{
		"empty":   "",
		"words":   "three",
		"numeric": "7",
	}}
	if n, ok := firstAttrInt(st, "empty", "words", "numeric"); !ok || n != 7 {
		t.Fatalf("firstAttrInt = (%d, %v), want (7, true)", n, ok)
	}
	if n, ok := firstAttrInt(st, "empty", "words"); ok || n != 0 {
		t.Fatalf("firstAttrInt with no numeric key = (%d, %v), want (0, false)", n, ok)
	}
	if n, ok := firstAttrInt(nil, "numeric"); ok || n != 0 {
		t.Fatalf("firstAttrInt(nil) = (%d, %v), want (0, false)", n, ok)
	}
}

func TestApplyAutonomyStageAttrsNilStageIsNoop(t *testing.T) {
	r := RetroRecord{ScopeUser: "keep"}
	applyAutonomyStageAttrs(nil, &r)
	if r.ScopeUser != "keep" || r.PlanRevisionsObserved || r.PRReworkObserved || r.RollbackEventsObserved {
		t.Fatalf("nil stage mutated record: %#v", r)
	}
}

func TestApplyAutonomyStageAttrsReadsAliasKeys(t *testing.T) {
	// Every field has a canonical key plus aliases; use the LAST alias of each
	// so the test fails if an alias is dropped from the lookup list.
	st := &timeline.Stage{Attrs: map[string]string{
		"approval_revisions":          "2",
		"rework_commits_after_review": "3",
		"rollback_count":              "1",
		"user":                        "alice",
		"repo":                        "o/r",
		"tier":                        "high",
		"acmm_level":                  "L4",
	}}
	var r RetroRecord
	applyAutonomyStageAttrs(st, &r)

	if !r.PlanRevisionsObserved || r.PlanRevisionsBeforeApproval != 2 {
		t.Errorf("plan revisions = (%d, observed=%v), want (2, true)", r.PlanRevisionsBeforeApproval, r.PlanRevisionsObserved)
	}
	if !r.PRReworkObserved || r.PRReworkCommitsAfterReview != 3 {
		t.Errorf("rework commits = (%d, observed=%v), want (3, true)", r.PRReworkCommitsAfterReview, r.PRReworkObserved)
	}
	if !r.RollbackEventsObserved || r.RollbackEvents != 1 {
		t.Errorf("rollbacks = (%d, observed=%v), want (1, true)", r.RollbackEvents, r.RollbackEventsObserved)
	}
	if r.ScopeUser != "alice" || r.ScopeRepo != "o/r" || r.ScopeChangeClass != "high" || r.AutonomyLevel != "L4" {
		t.Errorf("scope fields = %q/%q/%q/%q, want alice/o/r/high/L4", r.ScopeUser, r.ScopeRepo, r.ScopeChangeClass, r.AutonomyLevel)
	}
}

func TestApplyAutonomyStageAttrsDoesNotOverwriteEarlierScope(t *testing.T) {
	// Lane.Run applies stages in order; a later stage must not clobber scope
	// already learned from an earlier one, but counters are refreshed.
	r := RetroRecord{ScopeUser: "first", ScopeRepo: "o/first", AutonomyLevel: "L2"}
	st := &timeline.Stage{Attrs: map[string]string{
		"scope_user":     "second",
		"scope_repo":     "o/second",
		"autonomy_level": "L5",
		"rollbacks":      "4",
	}}
	applyAutonomyStageAttrs(st, &r)
	if r.ScopeUser != "first" || r.ScopeRepo != "o/first" || r.AutonomyLevel != "L2" {
		t.Fatalf("earlier scope overwritten: %#v", r)
	}
	if !r.RollbackEventsObserved || r.RollbackEvents != 4 {
		t.Fatalf("rollbacks = (%d, %v), want (4, true)", r.RollbackEvents, r.RollbackEventsObserved)
	}
}

func TestApplyAutonomyStageAttrsIgnoresUnparseableCounters(t *testing.T) {
	st := &timeline.Stage{Attrs: map[string]string{
		"plan_revisions_before_approval": "many",
		"pr_rework_commits_after_review": "",
	}}
	var r RetroRecord
	applyAutonomyStageAttrs(st, &r)
	if r.PlanRevisionsObserved || r.PRReworkObserved || r.RollbackEventsObserved {
		t.Fatalf("unparseable counters marked observed: %#v", r)
	}
}

func TestAppendPromotionStreak(t *testing.T) {
	mk := func(pattern string) *beads.Bead {
		return &beads.Bead{ID: pattern, Metadata: map[string]interface{}{metadataPattern: pattern}}
	}
	var streak []*beads.Bead
	streak = appendPromotionStreak(streak, mk(PatternPlanAcceptedFirstPass))
	streak = appendPromotionStreak(streak, mk(PatternPRMergedNoRework))
	if len(streak) != 2 {
		t.Fatalf("qualifying patterns: len = %d, want 2", len(streak))
	}
	// Unrelated patterns neither extend nor reset the streak.
	streak = appendPromotionStreak(streak, mk("retro_unrelated"))
	if len(streak) != 2 {
		t.Fatalf("unrelated pattern changed streak: len = %d, want 2", len(streak))
	}
	if got := appendPromotionStreak(streak, mk(PatternRunRolledBack)); got != nil {
		t.Fatalf("rollback must reset streak, got len %d", len(got))
	}
	if got := appendPromotionStreak(streak, mk(PatternPRReworkedAfterReview)); got != nil {
		t.Fatalf("rework must reset streak, got len %d", len(got))
	}
}

func TestAlreadyUsedEvidence(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Org: "hivecommons", Repos: []string{"hive"}}}
	if alreadyUsed(cfg, "hivecommons/hive", "b1") {
		t.Fatal("no automatic change recorded, evidence must not count as used")
	}
	cfg.Project.RepoPolicies = []config.RepoPolicy{{
		Repo:              "hivecommons/hive",
		ACMMLastAutomatic: &config.AutonomyLevelChange{EvidenceIDs: []string{"b1", "b2"}},
	}}
	if !alreadyUsed(cfg, "hivecommons/hive", "b2") {
		t.Fatal("b2 is in the last change's evidence, want used")
	}
	if alreadyUsed(cfg, "hivecommons/hive", "b3") {
		t.Fatal("b3 is not in the last change's evidence, want unused")
	}
}

func TestLaneSetAutonomyPolicy(t *testing.T) {
	l := &Lane{}
	engine := &AutonomyPolicyEngine{}
	l.SetAutonomyPolicy(engine)
	if l.autonomy != engine {
		t.Fatal("SetAutonomyPolicy did not install the engine")
	}
	l.SetAutonomyPolicy(nil)
	if l.autonomy != nil {
		t.Fatal("SetAutonomyPolicy(nil) did not clear the engine")
	}
}
