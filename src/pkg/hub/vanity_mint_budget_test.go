package hub

import (
	"errors"
	"testing"
	"time"
)

// ============================================================
// saas_provision.go — the vanity re-mint loop must be BOUNDED (#5923).
//
// Live failure these tests guard against: kickVanityURLRepairAsync runs on
// every heartbeat (~3,000 invocations/hour fleet-wide) and the only guard on
// SUCCESSFUL repairs was the per-hive in-flight dedupe. A fleet-wide "hosts
// look stale" condition re-minted freely, and every new vanity host is a new
// certificate against the registered domain's shared Let's Encrypt
// 50-certs/168h quota — which a one-hour burst exhausted, locking 15 spokes
// onto the ingress fake certificate for up to a week.
// ============================================================

// A hive whose repair succeeded within the cooldown must not spawn another
// repair goroutine — the seam is never entered and no in-flight entry appears.
func TestVanityRepairKickSkipsWithinSuccessCooldown(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s, entered, _ := newBlockedVanityHub(t)

	h := assignedVllmdHive("hosted-cooldown-fresh")
	h.LastVanityRepairAt = time.Now().Add(-time.Hour) // well inside 24h
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	s.kickVanityURLRepairAsync(h.ID)
	if _, inFlight := s.vanityRepairInFlight.Load(h.ID); inFlight {
		t.Error("kick spawned a repair for a hive inside the success cooldown")
	}
	time.Sleep(50 * time.Millisecond)
	if got := entered.Load(); got != 0 {
		t.Errorf("cooldown kick reached the servability seam %d times, want 0", got)
	}
}

// Once the cooldown has lapsed (or was never set — pre-field records), the
// kick must reach the repair again.
func TestVanityRepairKickRunsAfterCooldownLapses(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s, _, release := newBlockedVanityHub(t)
	defer release()

	for _, tc := range []struct {
		id   string
		last time.Time
	}{
		{"hosted-cooldown-lapsed", time.Now().Add(-vanityRepairSuccessCooldownDefault - time.Minute)},
		{"hosted-cooldown-unset", time.Time{}}, // pre-field record: zero value
	} {
		h := assignedVllmdHive(tc.id)
		h.LastVanityRepairAt = tc.last
		if err := saveSaaSHive(h); err != nil {
			t.Fatal(err)
		}
		s.kickVanityURLRepairAsync(tc.id)
		deadline := time.Now().Add(waitTimeout)
		spawned := false
		for time.Now().Before(deadline) {
			if _, inFlight := s.vanityRepairInFlight.Load(tc.id); inFlight {
				spawned = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !spawned {
			t.Errorf("%s: kick spawned no repair although the cooldown does not apply", tc.id)
		}
	}
}

// A successful mint must stamp LastVanityRepairAt, so the very next beat is
// already inside the cooldown instead of re-entering the repair.
func TestVanityRepairSuccessStampsCooldown(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	s.clusters["vllm-d"] = ClusterConfig{
		ID: "vllm-d", Name: "vllm-d",
		Domain: "apps.fmaas-vllm-d.example.com", IngressType: "nginx",
	}
	s.vanityHostServable = func(_, _ string, _ *ClusterConfig) error { return nil }

	const id = "hosted-cooldown-stamp"
	if err := saveSaaSHive(assignedVllmdHive(id)); err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	if !s.repairVanityURLForHive(id) {
		t.Fatal("repair did not mint")
	}
	h := loadSaaSHive(id)
	if h == nil || h.VanityURL == "" {
		t.Fatal("mint did not persist a vanity URL")
	}
	if h.LastVanityRepairAt.Before(before) {
		t.Errorf("LastVanityRepairAt = %v, want stamped at mint time", h.LastVanityRepairAt)
	}
}

// The fleet-wide mint budget must stop NEW host mints once exhausted: the
// servability seam is never reached, the placeholder host keeps working, and
// no vanity URL is adopted.
func TestVanityRepairMintBudgetExhaustedSkipsMint(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	s.clusters["vllm-d"] = ClusterConfig{
		ID: "vllm-d", Name: "vllm-d",
		Domain: "apps.fmaas-vllm-d.example.com", IngressType: "nginx",
	}
	seamEntered := 0
	s.vanityHostServable = func(_, _ string, _ *ClusterConfig) error {
		seamEntered++
		return nil
	}
	// Exhaust the budget with recent mints.
	for i := 0; i < vanityMintBudgetDefault; i++ {
		s.recordVanityMint()
	}

	const id = "hosted-budget-exhausted"
	if err := saveSaaSHive(assignedVllmdHive(id)); err != nil {
		t.Fatal(err)
	}

	if s.repairVanityURLForHive(id) {
		t.Error("repair reported success with the mint budget exhausted")
	}
	if seamEntered != 0 {
		t.Errorf("servability seam entered %d times with budget exhausted, want 0", seamEntered)
	}
	if h := loadSaaSHive(id); h == nil || h.VanityURL != "" {
		t.Errorf("vanity URL adopted despite exhausted budget: %+v", h)
	}
}

// Mints that have aged out of the rolling window free their budget slot again.
func TestVanityMintBudgetWindowPrunes(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	old := time.Now().Add(-vanityMintWindowDefault - time.Hour)
	for i := 0; i < vanityMintBudgetDefault; i++ {
		s.vanityMintTimes = append(s.vanityMintTimes, old)
	}
	if !s.vanityMintAllowed() {
		t.Error("budget still exhausted although every mint aged out of the window")
	}
	if got := s.vanityMintRemaining(); got != vanityMintBudgetDefault {
		t.Errorf("remaining = %d, want %d after pruning", got, vanityMintBudgetDefault)
	}
}

func TestVanityMintBudgetPersistsAcrossHubRestart(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(vanityMintBudgetEnv, "2")

	s := newHeartbeatHub()
	s.recordVanityMint()
	s.recordVanityMint()

	restarted := newHeartbeatHub()
	if restarted.vanityMintAllowed() {
		t.Error("new hub server forgot the persisted vanity mint budget ledger")
	}
}

func TestVanityMintBudgetAcquireReservesAtomically(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(vanityMintBudgetEnv, "1")

	s := newHeartbeatHub()
	if !s.acquireVanityMintSlot() {
		t.Fatal("first mint slot was not available")
	}
	if s.acquireVanityMintSlot() {
		t.Fatal("second mint slot succeeded despite budget=1")
	}
	if got := s.vanityMintRemaining(); got != 0 {
		t.Fatalf("remaining = %d, want 0 after atomic reservation", got)
	}
}

func TestVanityRepairFailureBackoffSkipsKick(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	s.clusters["vllm-d"] = ClusterConfig{
		ID: "vllm-d", Name: "vllm-d",
		Domain: "apps.fmaas-vllm-d.example.com", IngressType: "nginx",
	}
	s.vanityHostServable = func(_, _ string, _ *ClusterConfig) error {
		return errors.New("route update failed")
	}

	const id = "hosted-failure-backoff"
	if err := saveSaaSHive(assignedVllmdHive(id)); err != nil {
		t.Fatal(err)
	}
	if s.repairVanityURLForHive(id) {
		t.Fatal("repair unexpectedly succeeded")
	}
	h := loadSaaSHive(id)
	if h == nil || h.LastVanityRepairFailureAt.IsZero() || h.LastVanityRepairFailure == "" {
		t.Fatalf("repair failure was not persisted for backoff: %+v", h)
	}

	s.vanityHostServable = func(_, _ string, _ *ClusterConfig) error {
		t.Fatal("kick entered repair while failure backoff was active")
		return nil
	}
	s.kickVanityURLRepairAsync(id)
	if _, inFlight := s.vanityRepairInFlight.Load(id); inFlight {
		t.Error("kick spawned a repair while failure backoff was active")
	}
}

// The budget only meters NEW host mints. Adopting the host an existing route
// already serves issues no certificate, so it must succeed with the budget
// exhausted — otherwise an exhausted budget would also block the convergent
// drift repair.
func TestVanityReconcileIgnoresMintBudget(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	s.clusters["vllm-d"] = ClusterConfig{
		ID: "vllm-d", Name: "vllm-d",
		Domain: "apps.fmaas-vllm-d.example.com", IngressType: "nginx",
	}
	for i := 0; i < vanityMintBudgetDefault; i++ {
		s.recordVanityMint()
	}

	const id = "hosted-reconcile-budget"
	h := assignedVllmdHive(id)
	h.VanityURL = "https://stale-host.apps.fmaas-vllm-d.example.com"
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	// reconcileStaleVanityURL reads the live host through existingVanityHost,
	// which shells to kubectl and fails under test — returning "" and keeping
	// the stored URL. That path must not consult the budget at all: with the
	// budget exhausted the repair still runs and still returns false only
	// because the cluster is unreadable, never because of the budget.
	if s.repairVanityURLForHive(id) {
		t.Error("reconcile changed something although the cluster is unreadable")
	}
	if got := loadSaaSHive(id); got == nil || got.VanityURL != h.VanityURL {
		t.Errorf("stored vanity URL changed: %+v", got)
	}
}
