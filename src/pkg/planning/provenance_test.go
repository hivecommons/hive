package planning

import (
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/worksource"
)

func TestProvenanceGuardMismatchBlocksImplement(t *testing.T) {
	store, epic := provenanceStore(t)
	if err := RecordPlanSpecRevisionFromReceipt(store, epic.ID, outputschema.StageReceipt{
		Stage: worksource.RunStagePlan, InputRevision: "artifact@spec-a", ResultClass: outputschema.ReceiptResultCompleted,
	}); err != nil {
		t.Fatalf("record plan receipt: %v", err)
	}
	if err := store.SetMetadata(epic.ID, MetaSpecRevision, "artifact@spec-b"); err != nil {
		t.Fatalf("set spec revision: %v", err)
	}

	ok, err := GuardImplementStage(store, epic.ID)
	if err != nil {
		t.Fatalf("guard implement: %v", err)
	}
	if ok {
		t.Fatal("stale plan was allowed to implement")
	}
	got, _ := store.Get(epic.ID)
	if got.Meta(MetaRunWaitingOn) != worksource.RunWaitingOnHuman || got.Meta(MetaRunWaitingReason) != WaitingReasonStalePlan {
		t.Fatalf("stale plan hold metadata = waiting_on %q reason %q", got.Meta(MetaRunWaitingOn), got.Meta(MetaRunWaitingReason))
	}
}

func TestProvenanceGuardMatchPassesAndClearsStaleHold(t *testing.T) {
	store, epic := provenanceStore(t)
	for k, v := range map[string]string{
		MetaPlanSpecRevision: "artifact@spec-a",
		MetaSpecRevision:     "artifact@spec-a",
		MetaRunWaitingOn:     worksource.RunWaitingOnHuman,
		MetaRunWaitingReason: WaitingReasonStalePlan,
	} {
		if err := store.SetMetadata(epic.ID, k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	ok, err := GuardImplementStage(store, epic.ID)
	if err != nil {
		t.Fatalf("guard implement: %v", err)
	}
	if !ok {
		t.Fatal("matching revisions should pass")
	}
	got, _ := store.Get(epic.ID)
	if got.Meta(MetaRunWaitingOn) != "" || got.Meta(MetaRunWaitingReason) != "" {
		t.Fatalf("stale hold was not cleared: %+v", got.Metadata)
	}
}

func provenanceStore(t *testing.T) (*beads.Store, *beads.Bead) {
	t.Helper()
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	epic, err := store.Create("run epic", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	if err := store.SetMetadata(epic.ID, MetaPlanStatus, PlanStatusApproved); err != nil {
		t.Fatalf("set plan status: %v", err)
	}
	return store, epic
}
