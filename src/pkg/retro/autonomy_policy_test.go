package retro

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
)

type captureAutonomySink struct{ decisions []AutonomyDecision }

func (s *captureAutonomySink) RecordAutonomyDecision(d AutonomyDecision) {
	s.decisions = append(s.decisions, d)
}

func autonomyTestConfig(t *testing.T, level int) *config.Config {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wd, ".retro-test", sanitizeName(t.Name()+"-hive.yaml"))
	return &config.Config{
		Project:    config.ProjectConfig{Org: "hivecommons", Repos: []string{"hive"}},
		ACMMLevel:  &level,
		Autonomy:   config.AutonomyConfig{AutoPromote: true, AutoDemote: true},
		SourcePath: path,
	}
}

func addAutonomyFact(t *testing.T, store *beads.Store, pattern, repo string) *beads.Bead {
	t.Helper()
	b, err := store.Create("autonomy signal: "+repo+" "+pattern, beads.TypeAdvisory, beads.PriorityLow, Actor, "")
	if err != nil {
		t.Fatal(err)
	}
	direction := "qualifies"
	if pattern == PatternRunRolledBack {
		direction = "should lose"
	}
	for k, v := range map[string]string{
		metadataPattern:             pattern,
		metadataAutonomyScopeType:   "repo",
		metadataAutonomyScopeValue:  repo,
		metadataAutonomyDirection:   direction,
		metadataAutonomyLevel:       "L4",
		metadataAutonomyRun:         b.ID,
		metadataAutonomyRepo:        repo,
		metadataAutonomyChangeClass: "scheduler",
	} {
		if err := store.SetMetadata(b.ID, k, v); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func TestAutonomyPolicyFlagsOffNoChange(t *testing.T) {
	store := newStore(t, "policy-off")
	cfg := autonomyTestConfig(t, 3)
	cfg.Autonomy = config.AutonomyConfig{}
	for i := 0; i < config.DefaultAutonomyPromoteAfter; i++ {
		addAutonomyFact(t, store, PatternPlanAcceptedFirstPass, "hivecommons/hive")
	}

	sink := &captureAutonomySink{}
	engine := NewAutonomyPolicyEngine(cfg, store, sink)
	if got := engine.Evaluate(); len(got) != 0 {
		t.Fatalf("decisions = %#v, want none", got)
	}
	if cfg.ACMMLevelOrZero() != 3 || len(sink.decisions) != 0 {
		t.Fatalf("level/sink changed with flags off: level=%d decisions=%#v", cfg.ACMMLevelOrZero(), sink.decisions)
	}
}

func TestAutonomyPolicyPromotesOnceAndCooldownBlocksFourth(t *testing.T) {
	store := newStore(t, "policy-promote")
	cfg := autonomyTestConfig(t, 4)
	repoLevel := 3
	cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "hivecommons/hive", ACMMLevel: &repoLevel}}
	cfg.Autonomy.CooldownDays = config.DefaultAutonomyCooldownDays
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	for i := 0; i < config.DefaultAutonomyPromoteAfter; i++ {
		addAutonomyFact(t, store, PatternPlanAcceptedFirstPass, "hivecommons/hive")
	}

	sink := &captureAutonomySink{}
	engine := NewAutonomyPolicyEngine(cfg, store, sink)
	engine.SetClock(func() time.Time { return now })
	decisions := engine.Evaluate()
	if len(decisions) != 1 || decisions[0].From != 3 || decisions[0].To != 4 {
		t.Fatalf("decisions = %#v, want one L3->L4", decisions)
	}
	if got := cfg.EffectiveACMMLevelForRepo("hivecommons/hive"); got != 4 {
		t.Fatalf("effective repo level = %d, want 4", got)
	}

	addAutonomyFact(t, store, PatternPRMergedNoRework, "hivecommons/hive")
	engine.SetClock(func() time.Time { return now.Add(24 * time.Hour) })
	if got := engine.Evaluate(); len(got) != 0 {
		t.Fatalf("cooldown decisions = %#v, want none", got)
	}
	if got := cfg.EffectiveACMMLevelForRepo("hivecommons/hive"); got != 4 {
		t.Fatalf("effective repo level after cooldown-blocked fourth = %d, want 4", got)
	}
}

func TestAutonomyPolicyRollbackDemotesPinnedStaysPut(t *testing.T) {
	store := newStore(t, "policy-demote")
	cfg := autonomyTestConfig(t, 4)
	addAutonomyFact(t, store, PatternRunRolledBack, "hivecommons/hive")

	sink := &captureAutonomySink{}
	engine := NewAutonomyPolicyEngine(cfg, store, sink)
	decisions := engine.Evaluate()
	if len(decisions) != 1 || decisions[0].Direction != "demote" || decisions[0].From != 4 || decisions[0].To != 3 {
		t.Fatalf("decisions = %#v, want rollback demotion L4->L3", decisions)
	}
	if got := cfg.EffectiveACMMLevelForRepo("hivecommons/hive"); got != 3 {
		t.Fatalf("effective repo level = %d, want 3", got)
	}

	storePinned := newStore(t, "policy-pinned")
	cfgPinned := autonomyTestConfig(t, 4)
	cfgPinned.Project.RepoPolicies = []config.RepoPolicy{{Repo: "hivecommons/hive", ACMMPinned: true}}
	addAutonomyFact(t, storePinned, PatternRunRolledBack, "hivecommons/hive")
	if got := NewAutonomyPolicyEngine(cfgPinned, storePinned, &captureAutonomySink{}).Evaluate(); len(got) != 0 {
		t.Fatalf("pinned decisions = %#v, want none", got)
	}
	if cfgPinned.ACMMLevelOrZero() != 4 {
		t.Fatalf("pinned level = %d, want 4", cfgPinned.ACMMLevelOrZero())
	}
}

func TestAutonomyPolicyReworkDemoteMode(t *testing.T) {
	store := newStore(t, "policy-rework")
	cfg := autonomyTestConfig(t, 5)
	cfg.Autonomy.DemoteOn = "rework"
	addAutonomyFact(t, store, PatternPRReworkedAfterReview, "hivecommons/hive")

	decisions := NewAutonomyPolicyEngine(cfg, store, &captureAutonomySink{}).Evaluate()
	if len(decisions) != 1 || decisions[0].Direction != "demote" || decisions[0].To != 4 {
		t.Fatalf("decisions = %#v, want rework demotion", decisions)
	}
}
