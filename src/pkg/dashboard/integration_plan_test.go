package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/worksource"
)

func TestIntegrationDispatchUsesApprovedCanonicalDAGAndMergedPrerequisite(t *testing.T) {
	server := orchestrationServer(t, config.IntegrationAdvisory)
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	epic, err := store.Create("approved plan", beads.TypeEpic, beads.PriorityMedium, "architect", "")
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{planning.MetaPlanStatus: planning.PlanStatusApproved, planning.MetaDesignRevision: "2"} {
		if err := store.SetMetadata(epic.ID, key, value); err != nil {
			t.Fatal(err)
		}
	}
	var children []*beads.Bead
	for _, ref := range []string{"gh-o/r#1", "gh-o/r#2", "gh-o/r#3"} {
		child, err := store.Create(ref, beads.TypeTask, beads.PriorityMedium, "architect", ref)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetMetadata(child.ID, planning.MetaParentEpic, epic.ID); err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}
	if err := store.AddDependency(children[1].ID, children[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(children[0].ID, planning.MetaPRURL, "https://github.com/o/r/pull/9"); err != nil {
		t.Fatal(err)
	}
	server.deps.BeadStores = map[string]*beads.Store{"architect": store}
	intent := server.integrationDispatchIntent(worksource.Ref{Repo: "o/r", Number: 2}, "v5", time.Now())
	if intent.Plan != epic.ID || intent.PlanRevision != 2 || intent.ScopeKnown || len(intent.Prerequisites) != 1 || intent.Prerequisites[0].Key != "o/r#1" {
		t.Fatalf("approved DAG not attached to canonical dispatch: %+v", intent)
	}
	approved, dependencies := server.integrationPlan(intent)
	if !approved || dependencies["o/r#1"].Known {
		t.Fatalf("unmerged closure inferred readiness: %+v", dependencies)
	}
	ledger, err := server.integrationStore()
	if err != nil {
		t.Fatal(err)
	}
	receipt := integration.IntegrationReceipt{Repo: "o/r", PR: 9, Head: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40), MergedAt: time.Now(), Source: "github-signed-merged-webhook-v1"}
	if err := ledger.Append(integration.Event{Version: 1, ID: "merged-prerequisite", At: receipt.MergedAt, Integrated: &receipt}); err != nil {
		t.Fatal(err)
	}
	approved, dependencies = server.integrationPlan(intent)
	if !approved || !dependencies["o/r#1"].Known || !dependencies["o/r#1"].Satisfied {
		t.Fatalf("%v %+v", approved, dependencies)
	}
	omitted := intent
	omitted.Prerequisites = nil
	if approved, _ := server.integrationPlan(omitted); approved {
		t.Fatal("omitting approved dependency bypassed DAG")
	}
	unrelated := server.integrationDispatchIntent(worksource.Ref{Repo: "o/r", Number: 3}, "v5", time.Now())
	if approved, deps := server.integrationPlan(unrelated); !approved || len(deps) != 0 {
		t.Fatal("independent approved node inherited another consumer's dependency")
	}
	if err := store.SetMetadata(epic.ID, planning.MetaDesignRevision, "3"); err != nil {
		t.Fatal(err)
	}
	if approved, _ := server.integrationPlan(intent); approved {
		t.Fatal("changed plan generation reused dependency proof")
	}
}
