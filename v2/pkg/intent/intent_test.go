package intent

import (
	"testing"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		pr   PR
		want Tier
	}{
		{"docs only", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: "docs/readme.md", Additions: 3}}}, Tier0},
		{"test additive", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: "pkg/foo/foo_test.go", Additions: 8}}}, Tier0},
		{"test deletion escalates", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: "pkg/foo/foo_test.go", Additions: 1, Deletions: 1}}}, Tier1},
		{"removed test escalates", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: "tests/e2e_test.go", Status: "removed"}}}, Tier1},
		{"guardrail workflow", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: ".github/workflows/ci.yml"}}}, Tier3},
		{"guardrail owners", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: "OWNERS"}}}, Tier3},
		{"feature label", PR{AgentAuthor: true, Title: "add widget", Labels: []string{"enhancement"}, Files: []ChangedFile{{Filename: "pkg/widget/widget.go"}}}, Tier2},
		{"default bugfix chore", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: "pkg/foo/foo.go"}}}, Tier1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.pr, Config{}); got.Tier != tt.want {
				t.Fatalf("Tier = %d (%s), want %d", got.Tier, got.Reason, tt.want)
			}
		})
	}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name string
		tier Tier
		ev   Evidence
		want bool
	}{
		{"tier0 allowed", Tier0, Evidence{}, true},
		{"tier1 needs issue", Tier1, Evidence{}, false},
		{"tier1 linked issue", Tier1, Evidence{LinkedIssue: true}, true},
		{"tier2 needs plan", Tier2, Evidence{LinkedIssue: true}, false},
		{"tier2 approved plan", Tier2, Evidence{ApprovedPlan: true}, true},
		{"tier2 human approval", Tier2, Evidence{HumanApproval: true}, true},
		{"tier3 ignores plan", Tier3, Evidence{ApprovedPlan: true}, false},
		{"tier3 human approval", Tier3, Evidence{HumanApproval: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(Classification{Tier: tt.tier, AgentPR: true}, tt.ev)
			if got.Authorized != tt.want {
				t.Fatalf("Authorized = %v (%s), want %v", got.Authorized, got.Reason, tt.want)
			}
		})
	}
}

func TestEvidence(t *testing.T) {
	if !LinkedIssueInBody("Fixes #2803") || !LinkedIssueInBody("Closes kubestellar/hive#2803") {
		t.Fatal("expected closing issue refs to be detected")
	}
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	epic, _ := store.Create("epic", beads.TypeEpic, beads.PriorityHigh, "architect", "gh-kubestellar/hive#2803")
	if err := store.SetMetadata(epic.ID, BeadPlanStatusKey, BeadPlanApprovedStatus); err != nil {
		t.Fatal(err)
	}
	ev := BuildEvidenceForRepo("Fixes #2803", "kubestellar/hive", map[string]*beads.Store{"architect": store}, false)
	if !ev.LinkedIssue || !ev.ApprovedPlan {
		t.Fatalf("evidence = %+v, want linked issue and approved plan", ev)
	}
	unrelated := BuildEvidenceForRepo("Fixes #9999", "kubestellar/hive", map[string]*beads.Store{"architect": store}, false)
	if unrelated.ApprovedPlan {
		t.Fatalf("unrelated approved plan should not authorize PR evidence: %+v", unrelated)
	}
	otherRepoSameNumber := BuildEvidenceForRepo("Fixes #2803", "other/repo", map[string]*beads.Store{"architect": store}, false)
	if otherRepoSameNumber.ApprovedPlan {
		t.Fatalf("same issue number in another repo should not match approved plan: %+v", otherRepoSameNumber)
	}
}
