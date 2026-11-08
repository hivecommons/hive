package knowledge

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func slugSet(facts []Fact) map[string]bool {
	out := make(map[string]bool, len(facts))
	for _, f := range facts {
		out[f.Slug] = true
	}
	return out
}

// TestAutoMemoryDraftsNeverReachAgentFilters pins the #17058 hard exclusion:
// a drained CLI-memory draft passes the operator filter when drafts are asked
// for, but no agent-path filter admits it, whatever include_states or author
// match would admit any other draft. Once approved it is ordinary knowledge.
func TestAutoMemoryDraftsNeverReachAgentFilters(t *testing.T) {
	tagged := Fact{Slug: "mem-tagged", State: StateDraft, Author: "scanner", Tags: []string{"agent:scanner", "CLI-Memory"}}
	sourced := Fact{Slug: "mem-sourced", State: StateDraft, Author: "scanner", Origin: "cli-memory"}
	approvedMem := Fact{Slug: "mem-approved", State: StateApproved, Tags: []string{AutoMemoryTag}}
	ownDraft := Fact{Slug: "own-draft", State: StateDraft, Author: "scanner"}
	facts := []Fact{tagged, sourced, approvedMem, ownDraft}

	operator := slugSet(FilterByLifecycle(facts, []LifecycleState{StateDraft}))
	for _, slug := range []string{"mem-tagged", "mem-sourced", "mem-approved", "own-draft"} {
		if !operator[slug] {
			t.Errorf("operator filter with include draft dropped %s; drafts must stay reviewable", slug)
		}
	}

	kick := slugSet(FilterByLifecycleFor(facts, AllLifecycleStates, []string{"scanner"}))
	if kick["mem-tagged"] || kick["mem-sourced"] {
		t.Errorf("kick filter admitted an auto-memory draft: %v", kick)
	}
	if !kick["mem-approved"] || !kick["own-draft"] {
		t.Errorf("kick filter dropped an approved auto-memory fact or an ordinary own draft: %v", kick)
	}

	agent := AgentScope(nil, nil, nil, nil, []string{"all"})
	request := TOCScope{IncludeStates: AllLifecycleStates}
	for _, f := range []Fact{tagged, sourced} {
		if got := agent.ExclusionReason(f); got != ExcludedLifecycle {
			t.Errorf("agent scope ExclusionReason(%s) = %q, want %q", f.Slug, got, ExcludedLifecycle)
		}
		if !request.InScope(f) {
			t.Errorf("operator scope with all states hid %s", f.Slug)
		}
	}
	if !agent.InScope(approvedMem) || !agent.InScope(ownDraft) {
		t.Error("agent scope with all states must admit approved auto-memory and ordinary drafts")
	}
}

// TestSetEntryStateReviewsDrainedEntriesInBeadSynthVault proves the review
// path the drain depends on: a drained entry in the reserved bead-synth vault
// can be approved through SetEntryState, while a synthesizer entry in the
// same vault stays off-limits.
func TestSetEntryStateReviewsDrainedEntriesInBeadSynthVault(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cli-memory-aaaa.md", "---\ntitle: Run go test with -run\ntype: reference\nstate: draft\nsource: cli-memory\ntags: [cli-memory, agent:scanner]\n---\n\nRun go test with -run.\n")
	write("synth.md", "---\ntitle: Synthesized fact\ntype: gotcha\n---\n\nFrom a bead.\n")

	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, testLogger())
	if api.AutoMemoryVault() != nil {
		t.Fatal("AutoMemoryVault before the vault is connected")
	}
	if err := api.ConnectVault(dir, reservedVaultName); err != nil {
		t.Fatal(err)
	}
	if v := api.AutoMemoryVault(); v == nil || v.RootDir() != dir {
		t.Fatalf("AutoMemoryVault = %v, want the bead-synth vault at %s", v, dir)
	}

	change, err := api.SetEntryState("cli-memory-aaaa", StateApproved, "")
	if err != nil {
		t.Fatalf("approving a drained entry: %v", err)
	}
	if change.Previous != StateDraft || change.Fact.EffectiveState() != StateApproved || change.Channel != reservedVaultName {
		t.Fatalf("change = %+v (state %s)", change, change.Fact.EffectiveState())
	}
	if _, err := api.SetEntryState("synth", StateDeprecated, ""); !errors.Is(err, ErrEntryNotWritable) {
		t.Fatalf("synthesizer entry state change err = %v, want ErrEntryNotWritable", err)
	}
}
