package dashboard

import (
	"os"
	"path/filepath"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/reviewrules"
)

// Hive's own knowledge store is evidence for acmm:cross-session-knowledge
// that no repository path can show (#20128). These tests pin that the
// evaluator credits it through SatisfiedBy only when the kick primer is
// registered and the store holds an approved fact a person reviewed: a
// git-source fact or a reviewer rule. Bead-synth facts never count.

// withACMMGitSourceFacts hands the evaluator git-source facts without
// cloning a repository.
func withACMMGitSourceFacts(t *testing.T, facts ...knowledge.Fact) {
	t.Helper()
	orig := acmmGitSourceFacts
	acmmGitSourceFacts = func(*knowledge.KnowledgeAPI) []knowledge.Fact { return facts }
	t.Cleanup(func() { acmmGitSourceFacts = orig })
}

// acmmKnowledgeServer is acmmWaiverServer with a knowledge API and a primer
// whose registration is registered. The repo fixture holds none of the
// criterion's paths, and the review-rules channel points at a temp dir.
func acmmKnowledgeServer(t *testing.T, registered bool) (*Server, *knowledge.KnowledgeAPI) {
	t.Helper()
	knowledge.SetBaseDirForTest(t, t.TempDir())
	t.Setenv(reviewrules.VaultEnvVar, filepath.Join(t.TempDir(), "review-rules"))
	s := NewServer(0, acmmEvalTestLogger())
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(acmmReviewApplyMux(t).URL, "myorg", []string{"repo1"}, acmmEvalTestLogger())
	api := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, acmmEvalTestLogger())
	deps.Knowledge = api
	deps.KnowledgePrimer = &fakeKnowledgePrimer{status: KnowledgePrimerStatus{Registered: registered}}
	s.RegisterAPI(deps)
	return s, api
}

// connectACMMVault writes one Markdown entry into dir and connects it as a
// vault named name.
func connectACMMVault(t *testing.T, api *knowledge.KnowledgeAPI, dir, name, entry string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "entry.md"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := api.ConnectVault(dir, name); err != nil {
		t.Fatal(err)
	}
}

func approvedGitSourceFact() knowledge.Fact {
	return knowledge.Fact{
		Slug:  "build-steps",
		Title: "Build steps",
		Type:  "pattern",
		Layer: "git_source:docs",
		State: knowledge.StateApproved,
	}
}

const beadSynthEntry = "---\ntitle: Synthesized fact\ntype: pattern\nlayer: project\nconfidence: 0.90\nsource: https://github.com/myorg/repo1/pull/1\n---\n\nAn agent wrote this with no human review.\n"

const approvedReviewRuleEntry = "---\ntitle: R1 Always cite the issue\ntype: reviewer-rule\nlayer: project\nstate: approved\ntags: [review-rule, r1, pin]\nauthor: review-feedback\n---\n\nAlways cite the issue.\n"

func TestEvaluateAllRepos_KnowledgeStoreCreditsCrossSessionKnowledge(t *testing.T) {
	withACMMGitSourceFacts(t)
	s, _ := acmmKnowledgeServer(t, true)
	base := s.evaluateAllRepos()
	if c := acmmResultFor(t, base, acmmCrossSessionKnowledgeID); c.Passed {
		t.Fatalf("fixture repo passed %s with an empty store", acmmCrossSessionKnowledgeID)
	}
	baseL4 := acmmLevelFor(t, base, 4)

	withACMMGitSourceFacts(t, approvedGitSourceFact())
	eval := s.evaluateAllRepos()
	c := acmmResultFor(t, eval, acmmCrossSessionKnowledgeID)
	if !c.Passed || c.Waived || c.SatisfiedBy != acmmHiveKnowledgeStoreProvider || c.SatisfiedReason == "" {
		t.Fatalf("knowledge store not credited: passed=%v waived=%v satisfied_by=%q reason=%q", c.Passed, c.Waived, c.SatisfiedBy, c.SatisfiedReason)
	}
	if gotL4 := acmmLevelFor(t, eval, 4); gotL4.Detected != baseL4.Detected+1 {
		t.Errorf("L4 detected = %d, want %d: Hive credit must count toward the detected score", gotL4.Detected, baseL4.Detected+1)
	}
	for _, row := range eval.CriteriaResults {
		if row.ID == acmmCrossSessionKnowledgeID && (!row.Passed || row.Waived || row.SatisfiedBy != acmmHiveKnowledgeStoreProvider) {
			t.Errorf("aggregate row not credited: %+v", row)
		}
	}
}

func TestEvaluateAllRepos_KnowledgeStoreCreditNeedsRegisteredPrimer(t *testing.T) {
	withACMMGitSourceFacts(t, approvedGitSourceFact())
	for _, registered := range []bool{true, false} {
		s, _ := acmmKnowledgeServer(t, registered)
		c := acmmResultFor(t, s.evaluateAllRepos(), acmmCrossSessionKnowledgeID)
		if c.Passed != registered || (c.SatisfiedBy != "") != registered {
			t.Errorf("primer registered=%v: passed=%v satisfied_by=%q, want passed=%v", registered, c.Passed, c.SatisfiedBy, registered)
		}
	}
}

func TestEvaluateAllRepos_KnowledgeStoreCreditIgnoresBeadSynthFacts(t *testing.T) {
	withACMMGitSourceFacts(t)
	s, api := acmmKnowledgeServer(t, true)
	connectACMMVault(t, api, filepath.Join(t.TempDir(), "bead-synth-wiki"), "bead-synth-wiki", beadSynthEntry)
	if c := acmmResultFor(t, s.evaluateAllRepos(), acmmCrossSessionKnowledgeID); c.Passed {
		t.Fatalf("credited from a bead-synth fact alone: satisfied_by=%q reason=%q", c.SatisfiedBy, c.SatisfiedReason)
	}

	// A draft git-source fact is not reviewed either.
	draft := approvedGitSourceFact()
	draft.State = knowledge.StateDraft
	withACMMGitSourceFacts(t, draft)
	if c := acmmResultFor(t, s.evaluateAllRepos(), acmmCrossSessionKnowledgeID); c.Passed {
		t.Fatalf("credited from a draft git-source fact: satisfied_by=%q", c.SatisfiedBy)
	}

	// An approved reviewer rule in the review-rules channel does count.
	withACMMGitSourceFacts(t)
	connectACMMVault(t, api, reviewrules.VaultDir(), reviewrules.VaultName, approvedReviewRuleEntry)
	c := acmmResultFor(t, s.evaluateAllRepos(), acmmCrossSessionKnowledgeID)
	if !c.Passed || c.SatisfiedBy != acmmHiveKnowledgeStoreProvider {
		t.Fatalf("approved reviewer rule not credited: passed=%v satisfied_by=%q", c.Passed, c.SatisfiedBy)
	}
}
