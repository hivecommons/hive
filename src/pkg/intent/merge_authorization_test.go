package intent

import (
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

// MergeAllowed is the final gate before a verdict enters merge-eligible.json;
// pin all four authorization×alignment combinations.
func TestMergeAllowed(t *testing.T) {
	aligned := &AlignmentVerdict{Status: AlignmentStatusAligned}
	misaligned := &AlignmentVerdict{Status: AlignmentStatusMisaligned}

	tests := []struct {
		name string
		v    Verdict
		want bool
	}{
		{"unauthorized, no alignment", Verdict{Authorized: false}, false},
		{"unauthorized wins over aligned", Verdict{Authorized: false, Alignment: aligned}, false},
		{"authorized, phase-2 absent", Verdict{Authorized: true}, true},
		{"authorized and aligned", Verdict{Authorized: true, Alignment: aligned}, true},
		{"authorized but misaligned", Verdict{Authorized: true, Alignment: misaligned}, false},
		{"misaligned finding excludes", Verdict{Authorized: true, Alignment: &AlignmentVerdict{
			Status:                AlignmentStatusAligned,
			DeterministicFindings: []AlignmentFinding{{Status: AlignmentStatusMisaligned}},
		}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v.MergeAllowed(); got != tt.want {
				t.Fatalf("MergeAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}

// EvaluateForAppSelfMerge drops ONLY the human-approval requirement, ONLY
// when the caller confirmed the PR is the App's own. Every other tier gate
// must match Evaluate exactly.
func TestEvaluateForAppSelfMerge(t *testing.T) {
	agentClass := func(tier Tier) Classification {
		return Classification{Tier: tier, AgentPR: true}
	}

	t.Run("callAllowed=false is exactly Evaluate for every tier and evidence", func(t *testing.T) {
		evidences := []Evidence{
			{},
			{LinkedIssue: true},
			{ApprovedPlan: true},
			{HumanApproval: true},
		}
		for _, tier := range []Tier{Tier0, Tier1, Tier2, Tier3} {
			for _, ev := range evidences {
				class := agentClass(tier)
				got := EvaluateForAppSelfMerge(class, ev, false)
				want := Evaluate(class, ev)
				if got != want {
					t.Fatalf("tier %d evidence %+v: got %+v, want Evaluate result %+v", tier, ev, got, want)
				}
			}
		}
	})

	t.Run("tier3 without approval is authorized as app self-merge", func(t *testing.T) {
		got := EvaluateForAppSelfMerge(agentClass(Tier3), Evidence{}, true)
		if !got.Authorized {
			t.Fatalf("expected authorized, got %+v", got)
		}
		if got.Reason != ReasonAppSelfMergeAuthorized {
			t.Fatalf("Reason = %q, want %q", got.Reason, ReasonAppSelfMergeAuthorized)
		}
		if got.Tier != Tier3 || !got.AgentPR {
			t.Fatalf("verdict must preserve tier and agent flag: %+v", got)
		}
	})

	t.Run("tier2 without evidence is authorized as app self-merge", func(t *testing.T) {
		got := EvaluateForAppSelfMerge(agentClass(Tier2), Evidence{}, true)
		if !got.Authorized || got.Reason != ReasonAppSelfMergeAuthorized {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("tier2 with approved plan keeps Evaluate's reason", func(t *testing.T) {
		ev := Evidence{ApprovedPlan: true}
		got := EvaluateForAppSelfMerge(agentClass(Tier2), ev, true)
		want := Evaluate(agentClass(Tier2), ev)
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		if got.Reason == ReasonAppSelfMergeAuthorized {
			t.Fatal("approved-plan authorization must not be relabeled as app self-merge")
		}
	})

	t.Run("tier3 with human approval keeps Evaluate's reason", func(t *testing.T) {
		ev := Evidence{HumanApproval: true}
		got := EvaluateForAppSelfMerge(agentClass(Tier3), ev, true)
		want := Evaluate(agentClass(Tier3), ev)
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("tier1 without linked issue stays unauthorized even when callAllowed", func(t *testing.T) {
		got := EvaluateForAppSelfMerge(agentClass(Tier1), Evidence{}, true)
		if got.Authorized {
			t.Fatalf("tier1 gate must be unchanged by self-merge path: %+v", got)
		}
	})

	t.Run("tier0 additive stays authorized with Evaluate's reason", func(t *testing.T) {
		got := EvaluateForAppSelfMerge(agentClass(Tier0), Evidence{}, true)
		want := Evaluate(agentClass(Tier0), Evidence{})
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
}

func TestBuildEvidenceDelegatesWithEmptyRepo(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("epic", beads.TypeEpic, beads.PriorityHigh, "architect", "gh-hivecommons/hive#2803"); err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{"architect": store}

	ev := BuildEvidence("Fixes #2803", stores, false)
	if !ev.LinkedIssue {
		t.Error("body issue reference should set LinkedIssue")
	}
	if ev.HumanApproval {
		t.Error("HumanApproval must reflect the argument, not the beads")
	}

	ev = BuildEvidence("no references here", stores, true)
	if ev.LinkedIssue {
		t.Error("LinkedIssue should be false without a reference")
	}
	if !ev.HumanApproval {
		t.Error("HumanApproval argument should pass through")
	}
}

func TestLinkedIssueNumbers(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []int
	}{
		{"empty body", "", nil},
		{"no references", "just prose", nil},
		{"single closing ref", "Fixes #42", []int{42}},
		{"multiple refs in order", "Fixes #42 and refs #7, part of #1000", []int{42, 7, 1000}},
		{"cross-repo ref", "Fixes hivecommons/hive#2803", []int{2803}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LinkedIssueNumbers(tt.body)
			if len(got) != len(tt.want) {
				t.Fatalf("LinkedIssueNumbers(%q) = %v, want %v", tt.body, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("LinkedIssueNumbers(%q) = %v, want %v", tt.body, got, tt.want)
				}
			}
		})
	}
}

func TestLinkedIssueInBeads(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("epic", beads.TypeEpic, beads.PriorityHigh, "architect", "gh-hivecommons/hive#2803"); err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{"architect": store}

	if LinkedIssueInBeads(stores, nil) {
		t.Error("no refs must never match")
	}
	if !LinkedIssueInBeads(stores, []IssueRef{{Repo: "hivecommons/hive", Number: 2803}}) {
		t.Error("bead external-ref should match the issue ref")
	}
	if LinkedIssueInBeads(stores, []IssueRef{{Repo: "hivecommons/hive", Number: 9999}}) {
		t.Error("unrelated issue number must not match")
	}
	if LinkedIssueInBeads(nil, []IssueRef{{Repo: "hivecommons/hive", Number: 2803}}) {
		t.Error("nil stores must not match")
	}
}
