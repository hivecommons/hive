package dashboard

import (
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// --- #4105: the assignment prompt tells the agent up front the exact
// attribution trailer its PR body must end with, built from the hub's own
// handshake-recorded invocation values ---------------------------------------

func TestAttributionPromptInstruction_FullMeta(t *testing.T) {
	meta := ghpkg.InvocationMeta{
		Agent:   "quality",
		Backend: "claude",
		Model:   "claude-opus-4",
		Effort:  "high",
	}
	got := attributionPromptInstruction(meta)
	if !strings.Contains(got, "At the bottom of your PR body, include exactly this line:") {
		t.Fatalf("instruction missing the directive sentence: %q", got)
	}
	want := "— hive: agent=quality backend=claude model=claude-opus-4 effort=high"
	if !strings.Contains(got, want) {
		t.Fatalf("instruction missing the literal trailer %q: %q", want, got)
	}
}

// All-unknown metadata renders no trailer, so no instruction at all — the agent
// must never be asked to produce an empty footer.
func TestAttributionPromptInstruction_EmptyMeta(t *testing.T) {
	if got := attributionPromptInstruction(ghpkg.InvocationMeta{}); got != "" {
		t.Fatalf("expected empty instruction for empty meta, got %q", got)
	}
}

// promptInvocationMeta snapshots the hub's connection record (never
// relay-self-reported values) and normalizes the model the same way
// reconcilePRAttribution does, so the prompt instruction and the post-merge
// reconciliation trailer always agree.
func TestPromptInvocationMeta_FromConnection(t *testing.T) {
	c := &ContributorConnection{
		role:            "reviewer",
		cliBackend:      "bob",
		model:           "", // bob self-selects → normalized to "auto"
		reasoningEffort: "medium",
	}
	meta := promptInvocationMeta(c)
	if meta.Agent != "reviewer" || meta.Backend != "bob" || meta.Effort != "medium" {
		t.Fatalf("unexpected meta: %+v", meta)
	}
	if meta.Model != "auto" {
		t.Fatalf("bob backend with empty model should normalize to auto, got %q", meta.Model)
	}
}

func TestPromptInvocationMeta_NilConnection(t *testing.T) {
	if got := attributionPromptInstruction(promptInvocationMeta(nil)); got != "" {
		t.Fatalf("nil connection should yield no instruction, got %q", got)
	}
}

// End-to-end through selectTask: the shipped assignment prompt carries the
// literal footer instruction with the connection's real model/effort values.
func TestSelectTask_PromptCarriesAttributionInstruction(t *testing.T) {
	hub, s := covK2Hub(t)
	oneActionableIssue(s)
	c := &ContributorConnection{
		profile:         &ContributorProfile{GitHubUsername: "prompt-attrib-user", ContributorID: "c-pa", TrustTier: "trusted"},
		cliBackend:      "claude",
		model:           "claude-opus-4",
		reasoningEffort: "high",
	}
	msg := hub.selectTask(c)
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", msg)
	}
	if !strings.Contains(msg.Prompt, "At the bottom of your PR body, include exactly this line:") {
		t.Fatalf("assignment prompt missing footer directive: %q", msg.Prompt)
	}
	if !strings.Contains(msg.Prompt, "— hive: backend=claude model=claude-opus-4 effort=high") {
		t.Fatalf("assignment prompt missing real model/effort trailer: %q", msg.Prompt)
	}
}
