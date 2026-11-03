package hub

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

const admissionFallbackHint = "cluster admission webhook (e.g. Kyverno) is rejecting or timing out Deployment patches — a cluster admin must fix or exempt the webhook; falling back to a process restart"

// TestHeartbeatUpgradePendingRestartKeepsLatchAndHint: a spoke whose
// Deployment PATCH was refused by an admission webhook but which is exiting so
// the kubelet re-pulls its floating tag reports upgrade_pending_restart. That
// is still an in-flight upgrade — not a failure, and not a spent attempt.
func TestHeartbeatUpgradePendingRestartKeepsLatchAndHint(t *testing.T) {
	srv := degradedCarryTestServer(t)
	const hiveID = "pending-restart-hive"

	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"hivecommons","primary_repo":"hive","repos":["hive"],"git_hash":"14f41ee"}`)
	postBeat(t, srv, `{"hive_id":"`+hiveID+`","git_hash":"14f41ee","upgrade_target_sha":"fcf0e78","upgrading":true,"upgrade_pending_restart":true,"upgrade_error":"`+admissionFallbackHint+`"}`)

	entry := registryEntryByID(t, srv, hiveID)
	if entry.UpgradeFailed {
		t.Fatal("UpgradeFailed = true; a pending-restart beat must not count as a failed upgrade")
	}
	if entry.UpgradeError != "" {
		t.Errorf("UpgradeError = %q, want empty — the hint belongs in UpgradeRestartFallback, not the terminal error", entry.UpgradeError)
	}
	if !entry.Upgrading {
		t.Error("Upgrading = false, want the latch kept while the spoke restarts")
	}
	if !strings.Contains(entry.UpgradeRestartFallback, "admission webhook") {
		t.Errorf("UpgradeRestartFallback = %q, want the admission-webhook hint", entry.UpgradeRestartFallback)
	}
	if entry.Org != "hivecommons" || entry.Name != "hivecommons/hive" {
		t.Errorf("Org/Name = %q/%q, want project identity carried across the minimal beat", entry.Org, entry.Name)
	}

	// The next ordinary beat from the (still old) pod does not repeat the
	// field; the note must survive while the upgrade is in flight.
	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"hivecommons","primary_repo":"hive","repos":["hive"],"git_hash":"14f41ee","upgrading":true}`)
	entry = registryEntryByID(t, srv, hiveID)
	if !strings.Contains(entry.UpgradeRestartFallback, "admission webhook") {
		t.Errorf("UpgradeRestartFallback = %q after an ordinary in-flight beat, want carried forward", entry.UpgradeRestartFallback)
	}
	if entry.UpgradeFailed {
		t.Error("UpgradeFailed flipped true on an ordinary beat")
	}
}

// TestHeartbeatUpgradePendingRestartClearsWhenLanded: once the restarted pod
// reports a new git hash and no upgrade in flight, the fallback note goes
// with the latch — it described a rollout that has now completed.
func TestHeartbeatUpgradePendingRestartClearsWhenLanded(t *testing.T) {
	srv := degradedCarryTestServer(t)
	const hiveID = "pending-restart-landed"

	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"hivecommons","git_hash":"14f41ee"}`)
	postBeat(t, srv, `{"hive_id":"`+hiveID+`","git_hash":"14f41ee","upgrade_target_sha":"fcf0e78","upgrading":true,"upgrade_pending_restart":true,"upgrade_error":"`+admissionFallbackHint+`"}`)
	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"hivecommons","git_hash":"fcf0e78"}`)

	entry := registryEntryByID(t, srv, hiveID)
	if entry.Upgrading {
		t.Error("Upgrading = true after the new build reported in")
	}
	if entry.UpgradeRestartFallback != "" {
		t.Errorf("UpgradeRestartFallback = %q after landing, want cleared with the latch", entry.UpgradeRestartFallback)
	}
	if entry.UpgradeFailed {
		t.Error("UpgradeFailed = true for an upgrade that landed")
	}
}

// TestSweepExhaustedHintNamesAdmissionWebhook: when the orphan sweep finally
// gives up on a hive whose spoke reported a refused Deployment patch, the
// terminal cause names the webhook, not the misleading floating-tag guess.
func TestSweepExhaustedHintNamesAdmissionWebhook(t *testing.T) {
	s := &HubServer{
		logger:           slog.Default(),
		heartbeatUpgrade: make(map[string]string),
	}
	s.registry.Hives = []RegistryEntry{{
		ID:                     "kyverno-blocked",
		GitHash:                "14f41ee",
		UpgradeTarget:          "fcf0e78",
		UpgradeRestartFallback: admissionFallbackHint,
	}}
	for i := 1; i <= maxOrphanedUpgradeSweeps; i++ {
		h := &s.registry.Hives[0]
		h.Upgrading = true
		h.UpgradeStartedAt = time.Now().Add(-68 * time.Minute)
		h.LastHeartbeat = rfc3339(time.Now().Add(-30 * time.Second))
		s.sweepOrphanedUpgrades()
	}
	h := s.registry.Hives[0]
	if !h.UpgradeFailed {
		t.Fatal("expected the sweep to give up after the budget")
	}
	if !strings.Contains(h.UpgradeError, "could not patch its own Deployment") || !strings.Contains(h.UpgradeError, "admission webhook") {
		t.Errorf("UpgradeError = %q, want the admission-webhook cause", h.UpgradeError)
	}
	if strings.Contains(h.UpgradeError, "floating tag") {
		t.Errorf("UpgradeError = %q must not fall back to the floating-tag guess when the spoke reported the real cause", h.UpgradeError)
	}
}

// TestSweepExhaustedHintKeepsFloatingTagGuessWithoutFallback is the negative
// control: with no spoke-reported cause the historical hint still applies.
func TestSweepExhaustedHintKeepsFloatingTagGuessWithoutFallback(t *testing.T) {
	s := &HubServer{
		logger:           slog.Default(),
		heartbeatUpgrade: make(map[string]string),
	}
	s.registry.Hives = []RegistryEntry{{ID: "no-cause", GitHash: "14f41ee", UpgradeTarget: "fcf0e78"}}
	for i := 1; i <= maxOrphanedUpgradeSweeps; i++ {
		h := &s.registry.Hives[0]
		h.Upgrading = true
		h.UpgradeStartedAt = time.Now().Add(-68 * time.Minute)
		h.LastHeartbeat = rfc3339(time.Now().Add(-30 * time.Second))
		s.sweepOrphanedUpgrades()
	}
	if got := s.registry.Hives[0].UpgradeError; !strings.Contains(got, "floating tag") {
		t.Errorf("UpgradeError = %q, want the floating-tag hint when no cause was reported", got)
	}
}

// TestStuckUpgradeAlertCarriesRestartFallback: the in-flight "Upgrading 46m"
// warning tells the operator about the refused patch while the latch lasts.
func TestStuckUpgradeAlertCarriesRestartFallback(t *testing.T) {
	h := baseHive("kyverno-blocked")
	h.Upgrading = true
	h.UpgradeStartedAt = fixedNow.Add(-staleUpgradeTimeout - time.Minute)
	h.UpgradeTarget = "fcf0e78"
	h.UpgradeRestartFallback = admissionFallbackHint

	got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
	var found bool
	for _, a := range got.Alerts {
		if a.HiveID != "kyverno-blocked" || a.Type != AlertTypeStuckUpgrade {
			continue
		}
		found = true
		if !strings.Contains(a.Reason, "admission webhook") {
			t.Errorf("stuck-upgrade reason = %q, want the admission-webhook cause", a.Reason)
		}
	}
	if !found {
		t.Fatalf("expected a stuck-upgrade alert, got %+v", got.Alerts)
	}
}
