package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
)

// TestReadmittedRunStartsPastStaleReceipts is hivecommons/hive#10065: a run key
// admitted again after an earlier run wrote spec-gen1.json must not mint spec
// gen 1, or the stale receipt parks the new spec at its checkpoint before any
// work.
func TestReadmittedRunStartsPastStaleReceipts(t *testing.T) {
	now := time.Now()
	s, runKey := admittedSpecRun(t, now)
	held := finishAdmittedSpec(t, s, runKey, now)
	if held.gen != 1 {
		t.Fatalf("first admission gen = %d, want 1", held.gen)
	}
	if !s.runCheckpointStageHeld(runKey, StageSpec, held.gen) {
		t.Fatal("first spec not held after its receipt")
	}

	// The held spec checkpoint extends the first lease by the checkpoint hold
	// duration, not leaseTTL; admission is a no-op while that lease is live.
	later := now.Add(s.runCheckpointHoldDuration() + time.Minute)
	if err := s.AdmitTriagedRun("myorg/repo1", 8450, "feature", "spec", "re-triaged", later); err != nil {
		t.Fatalf("re-admit: %v", err)
	}
	readmitted, ok := s.contributeHub.runLeaseHolder(runKey, later)
	if !ok || readmitted.stage != StageSpec {
		t.Fatalf("re-admitted lease = %+v ok=%v, want spec", readmitted, ok)
	}
	if readmitted.gen != 2 {
		t.Fatalf("re-admitted gen = %d, want 2", readmitted.gen)
	}
	if s.runCheckpointStageHeld(runKey, StageSpec, readmitted.gen) {
		t.Fatal("re-admitted spec held before any work")
	}
	if got := pendingStageNames(t, s); len(got) != 1 || got[0] != StageSpec {
		t.Fatalf("pending stages after re-admission = %v, want [spec]", got)
	}
}

func TestNextRunAdmissionGen(t *testing.T) {
	old := runReceiptsDir
	runReceiptsDir = filepath.Join(t.TempDir(), "receipts")
	t.Cleanup(func() { runReceiptsDir = old })
	runKey := "acme/widgets#7"
	if got := nextRunAdmissionGen(runKey); got != 1 {
		t.Fatalf("no receipts gen = %d, want 1", got)
	}
	dir := filepath.Join(runReceiptsDir, sanitizeReceiptSegment(runKey))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"spec-gen1.json", "plan-gen2.json", "implement-gen3.transcript.json", "notes.txt", "spec-genx.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := nextRunAdmissionGen(runKey); got != 4 {
		t.Fatalf("gen past receipts = %d, want 4", got)
	}
}

// TestStartDesignSpektacularKeepsApprovedDesign is hivecommons/hive#10067: the
// governor re-enters startDesignSpektacular every cycle while the design label
// stays on the issue, and must not reset an approved design to requested.
func TestStartDesignSpektacularKeepsApprovedDesign(t *testing.T) {
	s := covApiServer(t)
	s.contributeHub.persistTaskLedgers = false
	s.deps.Config.Runs.Spektacular.Enabled = true
	disableSpekHubExecutorForRelayTests(s)
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s.deps.BeadStores = map[string]*beads.Store{planning.ArchitectAgentName: store}
	issue := ghpkg.Issue{Repo: "acme/widgets", Number: 43, Title: "Design widgets"}
	epic, _, err := s.StartDesignSpektacularFromIssue(context.Background(), store, issue)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if got := planning.DesignStatus(epic); got != planning.DesignStatusRequested {
		t.Fatalf("design status after first start = %q, want requested", got)
	}
	if err := planning.ApproveDesign(store, epic.ID); err != nil {
		t.Fatalf("ApproveDesign: %v", err)
	}
	again, runKey, err := s.StartDesignSpektacularFromIssue(context.Background(), store, issue)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if again.ID != epic.ID || runKey != "acme/widgets#43" {
		t.Fatalf("second start = epic %q key %q, want epic %q", again.ID, runKey, epic.ID)
	}
	current, _ := store.Get(epic.ID)
	if got := planning.DesignStatus(current); got != planning.DesignStatusApproved {
		t.Fatalf("design status after second start = %q, want approved", got)
	}
}
