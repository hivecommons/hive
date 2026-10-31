package mutation

import (
	"context"
	"errors"
	"testing"

	"github.com/hivecommons/hive/pkg/effects"
)

func TestBoundaryJournalRequiredRefusesOffAndMissingState(t *testing.T) {
	claim := effects.Claim{Repo: "o/r", Kind: effects.KindBranchUpdate, Target: "refs/heads/v5", Actor: "integration-aware"}
	for _, state := range []string{"nil", "off", "missing-ledger", "missing-journal", "nonpersistent"} {
		t.Run(state, func(t *testing.T) {
			boundary := testBoundary(t, "enforce")
			switch state {
			case "nil":
				boundary = nil
			case "off":
				boundary.Executor.Mode = "off"
			case "missing-ledger":
				boundary.Executor.Ledger = nil
			case "missing-journal":
				boundary.Executor.Journal = nil
			case "nonpersistent":
				boundary.Executor.Journal.path = ""
			}
			_, err := boundary.ExecuteJournaled(context.Background(), claim, func(context.Context) (effects.Result, error) {
				t.Fatal("required journal was bypassed")
				return effects.Result{}, nil
			})
			if !errors.Is(err, effects.ErrDenied) {
				t.Fatalf("state=%s did not fail closed: %v", state, err)
			}
		})
	}
}

func TestBoundaryJournalRequiredFreezesModeAndReplays(t *testing.T) {
	boundary := testBoundary(t, "enforce")
	mode, reads, calls := "enforce", 0, 0
	boundary.Mode = func() string {
		reads++
		return mode
	}
	claim := effects.Claim{Repo: "o/r", Kind: effects.KindBranchUpdate, Target: "refs/heads/v5", Actor: "integration-aware"}
	apply := func(context.Context) (effects.Result, error) {
		calls++
		mode = "off"
		return effects.Result{Provenance: "tested-sha"}, nil
	}
	out, err := boundary.ExecuteJournaled(context.Background(), claim, apply)
	if err != nil || out.Provenance != "tested-sha" || reads != 1 || calls != 1 {
		t.Fatalf("mode was not frozen: result=%+v err=%v reads=%d calls=%d", out, err, reads, calls)
	}
	op, found := boundary.Executor.Journal.Get(boundaryEffect(claim).LogicalID())
	if !found || op.Status != StatusApplied || op.Result != "tested-sha" {
		t.Fatalf("callback mode change lost the journal: %+v found=%v", op, found)
	}
	if _, err := boundary.ExecuteJournaled(context.Background(), claim, apply); !errors.Is(err, effects.ErrDenied) || calls != 1 {
		t.Fatalf("off-mode journal-required effect ran: err=%v calls=%d", err, calls)
	}
	mode = "enforce"
	reopened, err := OpenJournal(boundary.Executor.Journal.path)
	if err != nil {
		t.Fatal(err)
	}
	boundary.Executor.Journal = reopened
	if _, err := boundary.ExecuteJournaled(context.Background(), claim, apply); err != nil || calls != 1 {
		t.Fatalf("journal replay repeated the external effect: err=%v calls=%d", err, calls)
	}
}

func TestJournalRequiredSyncRefusesMissingState(t *testing.T) {
	boundary := testBoundary(t, "enforce")
	if err := boundary.Executor.Journal.syncPersistent(); err == nil {
		t.Fatal("missing journal file was considered synced")
	}
	boundary.Executor.Journal.path = ""
	if err := boundary.Executor.Journal.syncPersistent(); err == nil {
		t.Fatal("empty journal path was considered synced")
	}
}
