package decisionpolicy

import (
	"strings"
	"testing"
)

// The park texts contributor agents left on one target repository on
// 2026-10-19 (evidence in hivecommons/hive#13059), with repo names
// generalised: every one is an AGENTS.md confirmation gate and/or a
// registered sibling checkout reported missing.
var observedGateParks = []string{
	"Implementation is paused at the repository’s explicit AGENTS.md confirmation gate: its “Spec-Worthy Discussion Recognition” rule requires offering a spec and waiting for acceptance, deferral, or decline before implementing a multi-requirement issue. With `spec_trigger_threshold: moderate`, #77 meets that threshold. Should this issue be captured as a spec first, or should spec capture be explicitly declined so implementation can proceed directly?\n\nThere is also a checkout prerequisite: `go run . repo list` stops with `repo_location_missing` for the registered docs location `/home/dev/workspace/acme/widgets-docs/.specs`; AGENTS.md requires reporting that missing path and stopping rather than substituting another directory.",
	"Implementation is waiting on the repository-required spec-workflow decision. `AGENTS.md` (Spec-Worthy Discussion Recognition), with `spec_trigger_threshold: moderate`, requires offering to capture a multi-requirement issue as a spec and waiting for acceptance or an explicit decline before implementation. I offered that choice, but have not received a decision.",
	"Implementation is paused at the repository’s AGENTS.md gates. This issue defines multiple requirements (per-run state, safe concurrent plan updates, isolated working trees, and exactly-once feature completion), so the mandatory spec-trigger rule requires offering spec capture and waiting for explicit confirmation before implementation. Please confirm whether to capture these requirements as a spec, or decline spec capture so implementation can proceed.",
	"Paused before edits to comply with this repository’s AGENTS.md approval gates. This multi-requirement docs review meets the configured `spec_trigger_threshold: moderate`: would you like it captured as a spec first, or should I proceed directly with the review? The required `go run . repo list` also fails with `repo_location_missing` for the registered website location; AGENTS.md says to stop and report missing registered paths. May I provision that website checkout at the registered location?",
	// Short forms the verdict line carries after "decision:".
	"waiting on spec-workflow decision per AGENTS.md",
	"should I capture this as a spec or proceed?",
	"registered sibling checkout missing: /home/dev/workspace/acme/docs",
	"CLAUDE.md says to confirm scope with a maintainer before implementing",
}

func TestIsRepoLocalGateRecognisesObservedParks(t *testing.T) {
	for _, text := range observedGateParks {
		got := Classify(text)
		if got.Escalate || got.Category != CategoryRepoLocalGate || got.Evidence == "" {
			t.Errorf("Classify(%.60q…) = %+v, want %q", text, got, CategoryRepoLocalGate)
		}
		if !IsRepoLocalGate(text) {
			t.Errorf("IsRepoLocalGate(%.60q…) = false, want true", text)
		}
		if !ResolvesWithoutHuman(text) {
			t.Errorf("ResolvesWithoutHuman(%.60q…) = false, want true", text)
		}
	}
}

func TestIsRepoLocalGateKeepsRiskParks(t *testing.T) {
	cases := []string{
		// A gate phrase beside a risk category stays a human decision.
		"AGENTS.md confirmation gate: the change rotates the deploy key",
		"Should I capture this as a spec? It deletes production data.",
		"repo_location_missing for a path that needs the owner's secret to clone",
		// Ordinary design parks are not gates.
		"Waiting on maintainer design guidance before altering workflow templates.",
		"Approach A vs approach B for the config shape",
		"",
	}
	for _, text := range cases {
		if IsRepoLocalGate(text) {
			t.Errorf("IsRepoLocalGate(%q) = true, want false", text)
		}
	}
}

func TestRepoLocalGateEvidenceIsCollapsed(t *testing.T) {
	ev, ok := repoLocalGateEvidence("x\n AGENTS.md\n\n  confirmation   gate y")
	if !ok || strings.Contains(ev, "\n") || strings.Contains(ev, "  ") {
		t.Fatalf("repoLocalGateEvidence = %q, %v; want single-spaced match", ev, ok)
	}
	if _, ok := repoLocalGateEvidence("nothing here"); ok {
		t.Fatal("repoLocalGateEvidence matched plain text")
	}
}

func TestDispatchInstructionsNameTheRiskCategoriesAndTheClone(t *testing.T) {
	for _, want := range []string{"AGENTS.md", "CLAUDE.md", "do not park", "security", "data loss", "irreversible", "governance", "spend", "credentials", "A/B"} {
		if !strings.Contains(DispatchSatisfiesRepoGates, want) {
			t.Errorf("DispatchSatisfiesRepoGates lacks %q", want)
		}
	}
	for _, want := range []string{"<org>/<repo>", "clone", "never park"} {
		if !strings.Contains(MissingCompanionCheckoutRule, want) {
			t.Errorf("MissingCompanionCheckoutRule lacks %q", want)
		}
	}
}
