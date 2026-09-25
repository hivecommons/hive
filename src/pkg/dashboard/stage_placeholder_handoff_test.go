package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// The hub executor advances a run to implement under its own identity and
// then leaves that lease as the stage's placeholder (Tick only runs spec and
// plan). A relay adopting the stage must replace the placeholder exactly as
// it replaces an admission lease, or the run is listed twice and the
// placeholder — kept alive forever by keepPendingStageLeasesAlive — never
// goes away.
func TestRecordLeaseForKeyStage_RelayAdoptionEvictsHubExecutorPlaceholder(t *testing.T) {
	now := time.Now()
	h := &ContributeWSHub{logger: covBLogger()}
	const key = "kubestellar/console!kubestellar/console#23725:spec"
	exec := config.DefaultSpektacularHubExecutorIdentity
	// Two fan-out waves on the same key+stage: distinguished by gen only.
	for _, task := range []string{"fan-w1", "fan-w2"} {
		if err := h.recordLeaseForKeyStage(runFanoutIdentity, task, "kubestellar/console", 0, key, "contributor", StageImplement, 1, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.recordLeaseForKeyStage(exec, "run-hub-x", "kubestellar/console", 0, key, "trusted", StageImplement, 6, now); err != nil {
		t.Fatal(err)
	}
	h.leaseMu.Lock()
	h.leaseForLocked(exec, "run-hub-x").triageVerdict = "proceed"
	h.leaseMu.Unlock()

	if err := h.recordLeaseForKeyStage("c-relay", "ct-1", "kubestellar/console", 0, key, "contributor", StageImplement, 8, now); err != nil {
		t.Fatal(err)
	}

	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	if h.leaseForLocked(exec, "run-hub-x") != nil {
		t.Error("hub executor placeholder survived relay adoption")
	}
	relay := h.leaseForLocked("c-relay", "ct-1")
	if relay == nil || relay.triageVerdict != "proceed" {
		t.Errorf("relay lease = %+v, want the placeholder's triage verdict carried over", relay)
	}
	for _, task := range []string{"fan-w1", "fan-w2"} {
		if h.leaseForLocked(runFanoutIdentity, task) == nil {
			t.Errorf("fan-out lease %s evicted; waves share key+stage and must survive one adoption", task)
		}
	}
}

// The hub executor re-claiming under its own identity is not an adoption and
// evicts nothing of its own.
func TestRecordLeaseForKeyStage_SameIdentityDoesNotEvictItself(t *testing.T) {
	now := time.Now()
	h := &ContributeWSHub{logger: covBLogger()}
	const key = "kubestellar/console!kubestellar/console#1:spec"
	exec := config.DefaultSpektacularHubExecutorIdentity
	for _, task := range []string{"a", "b"} {
		if err := h.recordLeaseForKeyStage(exec, task, "kubestellar/console", 0, key, "trusted", StagePlan, 2, now); err != nil {
			t.Fatal(err)
		}
	}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	if h.leaseForLocked(exec, "a") == nil || h.leaseForLocked(exec, "b") == nil {
		t.Error("same-identity record evicted a sibling lease")
	}
}

// Adoption evicted the placeholder, so a relay that ages out would take the
// run's only record with it. Expiry must hand the stage back to the offer
// pool at the same stage and generation.
func TestPruneExpiredLeases_ReoffersOrphanedRelayStage(t *testing.T) {
	now := time.Now()
	h := &ContributeWSHub{logger: covBLogger()}
	const key = "kubestellar/console!kubestellar/console#23725:spec"
	if err := h.recordLeaseForKeyStage(runAdmissionIdentity, "run-admit-x", "kubestellar/console", 23725, key, "trusted", StageImplement, 6, now); err != nil {
		t.Fatal(err)
	}
	h.leaseMu.Lock()
	h.leaseForLocked(runAdmissionIdentity, "run-admit-x").triageVerdict = "proceed"
	h.leaseMu.Unlock()
	if err := h.recordLeaseForKeyStage("c-relay", "ct-1", "kubestellar/console", 23725, key, "trusted", StageImplement, 6, now); err != nil {
		t.Fatal(err)
	}
	// A plain (non-stage) relay lease expiring alongside must mint nothing.
	if err := h.recordLeaseForKey("c-other", "plain", "kubestellar/console", 7, "kubestellar/console#7", "contributor", 1, now); err != nil {
		t.Fatal(err)
	}

	later := now.Add(leaseTTL + time.Minute)
	if dropped := h.pruneExpiredLeases(later); dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}

	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	if h.leaseForLocked("c-relay", "ct-1") != nil {
		t.Error("expired relay lease still present")
	}
	var placeholders []*taskLease
	for _, l := range h.leases {
		if l.key == key {
			placeholders = append(placeholders, l)
		}
	}
	if len(placeholders) != 1 {
		t.Fatalf("leases on run key = %d, want exactly one re-minted placeholder", len(placeholders))
	}
	p := placeholders[0]
	if p.identity != runAdmissionIdentity || p.stage != StageImplement || p.gen != 6 || p.triageVerdict != "proceed" || p.tier != "trusted" {
		t.Errorf("placeholder = %+v, want admission identity at implement gen 6 carrying verdict and tier", p)
	}
	if !p.expiresAt.After(later) {
		t.Errorf("placeholder expires %v, not after prune time %v", p.expiresAt, later)
	}
	if n := len(h.leases); n != 1 {
		t.Errorf("total leases = %d, want 1 (no placeholder for the plain lease)", n)
	}
}

// When another live lease still covers key+stage — a second fan-out wave, or
// a relay that re-adopted — nothing is minted.
func TestPruneExpiredLeases_NoReofferWhenStageStillCovered(t *testing.T) {
	now := time.Now()
	h := &ContributeWSHub{logger: covBLogger()}
	const key = "kubestellar/console!kubestellar/console#23725:spec"
	if err := h.recordLeaseForKeyStage("c-relay", "ct-1", "kubestellar/console", 23725, key, "trusted", StageImplement, 1, now.Add(-leaseTTL)); err != nil {
		t.Fatal(err)
	}
	if err := h.recordLeaseForKeyStage(runFanoutIdentity, "fan-w2", "kubestellar/console", 23725, key, "trusted", StageImplement, 2, now); err != nil {
		t.Fatal(err)
	}
	if dropped := h.pruneExpiredLeases(now.Add(time.Minute)); dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	if n := len(h.leases); n != 1 {
		t.Errorf("leases = %d, want only the surviving fan-out wave", n)
	}
}
