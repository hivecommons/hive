package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/climemory"
)

// TestPrimeKnowledge_AutoMemoryDraftsNeverPrimedUntilApproved is the #17058
// acceptance test: a fact the CLI-memory drain wrote reaches no kick while it
// is a draft — not for an approved-only scope, not for a scope that admits
// drafts, not even when the primer config itself admits drafts — and the same
// fact is primed once an operator approves it through the lifecycle state API.
func TestPrimeKnowledge_AutoMemoryDraftsNeverPrimedUntilApproved(t *testing.T) {
	memDir := filepath.Join(t.TempDir(), ".claude", "projects", "-data-agents-developer", "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const drained = "Webhook retry needs the idempotency key on every resend"
	if err := os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("- "+drained+".\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vaultDir := t.TempDir()
	const approved = "Webhook retry approved fact"
	if err := os.WriteFile(filepath.Join(vaultDir, "approved.md"), []byte("---\ntitle: "+approved+"\ntype: gotcha\nlayer: project\nstate: approved\n---\nRelevant to the webhook retry path.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	api := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, logger)
	if err := api.ConnectVault(vaultDir, "bead-synth-wiki"); err != nil {
		t.Fatal(err)
	}
	store := api.GetVaultStore(vaultDir)
	drainer := &climemory.Drainer{Dir: vaultDir, Store: store, Layer: knowledge.LayerProject}
	res := drainer.Drain(climemory.Request{Agent: "developer", Sources: []climemory.Source{{CLI: climemory.CLIClaude, Path: memDir}}})
	if res.Drained != 1 || len(res.Errors) != 0 {
		t.Fatalf("drain = %+v, want one drafted fact", res)
	}
	files, err := filepath.Glob(filepath.Join(vaultDir, "cli-memory-*.md"))
	if err != nil || len(files) != 1 {
		t.Fatalf("drained entries = %v, %v; want one", files, err)
	}
	slug := strings.TrimSuffix(filepath.Base(files[0]), ".md")
	if f, err := store.ReadPage(slug); err != nil || f.EffectiveState() != knowledge.StateDraft {
		t.Fatalf("drained entry %s = %+v, %v; want a draft", slug, f, err)
	}

	issues := []github.Issue{makeIssue("org/repo", 1, "Webhook retry fails", "", 5, []string{"webhook", "retry", "idempotency"}, false)}
	newSched := func(primerStates []knowledge.LifecycleState) *Scheduler {
		primer := knowledge.NewPrimer(nil, knowledge.PrimerConfig{MaxFacts: 25, IncludeStates: primerStates}, logger)
		primer.AddFileStore("bead-synth-wiki", store, knowledge.LayerProject)
		s := newScheduler()
		s.cfg.Knowledge.AgentScopes = map[string]config.KnowledgeAgentScope{
			"quality":   {Layers: []string{"project"}},
			"developer": {IncludeStates: []string{"draft"}},
			"architect": {IncludeStates: []string{"all"}},
		}
		s.SetPrimer(primer)
		return s
	}
	defaultPrimer := newSched(nil)
	draftPrimer := newSched([]knowledge.LifecycleState{knowledge.StateDraft})
	agents := []string{"quality", "developer", "architect", "outreach"}

	for _, s := range []*Scheduler{defaultPrimer, draftPrimer} {
		for _, agent := range agents {
			got := s.primeKnowledge(agent, issues)
			if !strings.Contains(got, approved) {
				t.Fatalf("%s: approved control fact missing, the kick was not primed:\n%s", agent, got)
			}
			if strings.Contains(got, drained) {
				t.Errorf("%s: auto-memory draft reached the kick:\n%s", agent, got)
			}
		}
	}

	if _, err := api.SetEntryState(slug, knowledge.StateApproved, ""); err != nil {
		t.Fatalf("approving %s through the state API: %v", slug, err)
	}
	for _, agent := range agents {
		got := defaultPrimer.primeKnowledge(agent, issues)
		if !strings.Contains(got, drained) {
			t.Errorf("%s: approved auto-memory fact not primed:\n%s", agent, got)
		}
		if strings.Contains(got, "(draft)") {
			t.Errorf("%s: approved auto-memory fact still rendered as a draft:\n%s", agent, got)
		}
	}
}
