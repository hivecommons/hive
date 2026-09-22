package dashboard

import (
	"testing"
	"time"
)

// kubestellar/hive#6909: the clientGen==0 escape made the #2568 Gate opt-in.
// A client could bypass fencing at every call site just by omitting task_gen,
// because an absent JSON field decodes to 0 — the value that disabled the check.
// The escape is now ratcheted: it survives only for connections that have never
// echoed a real generation.
func TestGenerationAccepted_LegacyClientStillAccepted(t *testing.T) {
	// An unversioned relay never learned a generation and never sets the flag.
	// It must keep working: this is the backward-compatibility contract.
	if !generationAccepted(0, 5, false) {
		t.Fatal("unstamped legacy client rejected; backward compatibility broken")
	}
}

func TestGenerationAccepted_FencedClientCannotDowngrade(t *testing.T) {
	// The bug: this connection has already proven it speaks the fenced protocol,
	// then sends a message with task_gen omitted. Before #6909 this returned true
	// and the stale message was accepted.
	if generationAccepted(0, 5, true) {
		t.Fatal("fenced connection was allowed to downgrade to unstamped: #2568 Gate is bypassable")
	}
}

func TestGenerationAccepted_StaleGenerationRejected(t *testing.T) {
	// Unchanged #2568 behaviour: a worker whose task was revoked/reassigned
	// echoes an older generation and must not renew a lease it no longer owns.
	if generationAccepted(3, 5, true) {
		t.Fatal("stale generation accepted")
	}
	if generationAccepted(3, 5, false) {
		t.Fatal("stale generation accepted for unratcheted connection")
	}
}

func TestGenerationAccepted_CurrentGenerationAccepted(t *testing.T) {
	if !generationAccepted(5, 5, true) {
		t.Fatal("matching generation rejected")
	}
}

// The ratchet is armed by observing any non-zero task_gen on the connection, so a
// client that has fenced even once cannot revert. This mirrors the assignment the
// message handlers make before consulting generationAccepted.
func TestGenerationRatchet_ArmsOnFirstStampedMessage(t *testing.T) {
	c := &ContributorConnection{currentTaskGen: 7}

	if c.sawTaskGen {
		t.Fatal("ratchet armed before any stamped message")
	}

	// An unstamped message from a never-fenced connection is accepted and must
	// NOT arm the ratchet.
	if gen := uint64(0); gen != 0 {
		c.sawTaskGen = true
	}
	if !generationAccepted(0, c.currentTaskGen, c.sawTaskGen) {
		t.Fatal("legacy client rejected before ratchet armed")
	}

	// A stamped message arms it permanently.
	if gen := uint64(7); gen != 0 {
		c.sawTaskGen = true
	}
	if !c.sawTaskGen {
		t.Fatal("ratchet did not arm on a stamped message")
	}

	// Now the same connection may no longer fall back to unstamped.
	if generationAccepted(0, c.currentTaskGen, c.sawTaskGen) {
		t.Fatal("ratchet did not hold: connection downgraded to unstamped after fencing")
	}
}

func TestLookupLease_FencesOldGenerationAfterStageAdvance(t *testing.T) {
	hub, _ := covK2Hub(t)
	now := time.Now()
	if err := hub.recordLeaseForKeyStage("c-fence", "task-fence", "myorg/repo1", 8297,
		"myorg/repo1#8297", "contributor", StageSpec, 30, now); err != nil {
		t.Fatalf("record staged lease: %v", err)
	}
	advanced, err := hub.advanceLeaseStage("c-fence", "task-fence", StagePlan, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("advance stage: %v", err)
	}

	if got := hub.lookupLease("c-fence", "task-fence", "myorg/repo1", 8297, 30, now); got != nil {
		t.Fatalf("old stage generation was accepted after advance: %+v", got)
	}
	if got := hub.lookupLease("c-fence", "task-fence", "myorg/repo1", 8297, advanced.gen, now); got == nil {
		t.Fatal("new stage generation was rejected")
	}
}
