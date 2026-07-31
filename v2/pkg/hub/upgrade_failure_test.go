package hub

import (
	"net/http"
	"testing"
	"time"
)

// Before this change the hub only ever learned about upgrades that SUCCEEDED.
// A spoke that could not upgrade (e.g. HTTP 403 patching its own Deployment)
// looked identical to one still working on it, so the registry kept
// Upgrading=true forever and the hub re-instructed the same target every beat.

func TestHandleHeartbeat_UpgradeFailedClearsUpgradingAndRecordsCause(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	s.registry.Hives = []RegistryEntry{{
		ID: "h1", Upgrading: true, UpgradeTarget: "fc32ae4", GitHash: "c11643a",
	}}
	// Arm the fallback instruction so we can prove it gets dropped.
	s.heartbeatUpgrade = map[string]string{"h1": "fc32ae4"}

	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"c11643a",`+
		`"upgrade_target_sha":"fc32ae4","upgrade_failed":true,`+
		`"upgrade_error":"HTTP 403: cannot patch resource deployments"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	s.mu.RLock()
	h := s.registry.Hives[0]
	_, stillArmed := s.heartbeatUpgrade["h1"]
	s.mu.RUnlock()

	if h.Upgrading {
		t.Error("Upgrading must be cleared on a reported failure — the UI showing a permanent spinner is the bug")
	}
	if !h.UpgradeFailed {
		t.Error("UpgradeFailed not recorded")
	}
	if h.UpgradeError == "" {
		t.Error("UpgradeError must carry the underlying cause so the failure is diagnosable")
	}
	if h.UpgradeFailedAt.IsZero() {
		t.Error("UpgradeFailedAt not stamped")
	}
	if stillArmed {
		t.Error("a refused instruction must be dropped, otherwise the hub re-sends the same failing target forever")
	}
}

func TestHandleHeartbeat_UpgradeFailurePersistsUntilTheHiveActuallyMoves(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	// Ordinary heartbeats do not repeat the failure flag; the hub must not
	// forget the failure just because the next beat was silent about it.
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", GitHash: "c11643a", UpgradeFailed: true,
		UpgradeError: "HTTP 403", UpgradeFailedAt: time.Now(),
	}}

	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"c11643a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	s.mu.RLock()
	h := s.registry.Hives[0]
	s.mu.RUnlock()
	if !h.UpgradeFailed || h.UpgradeError != "HTTP 403" {
		t.Errorf("failure state lost on a plain heartbeat: %+v", h)
	}
}

func TestHandleHeartbeat_UpgradeFailureClearsOnceTheHiveReportsANewSHA(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	s.registry.Hives = []RegistryEntry{{
		ID: "h1", GitHash: "c11643a", UpgradeFailed: true,
		UpgradeError: "HTTP 403", UpgradeFailedAt: time.Now(),
	}}

	// The hive finally moved — the failure is history and must not stick.
	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"fc32ae4"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	s.mu.RLock()
	h := s.registry.Hives[0]
	s.mu.RUnlock()
	if h.UpgradeFailed || h.UpgradeError != "" {
		t.Errorf("failure state must clear once the spoke reports a new SHA: %+v", h)
	}
}

func TestEvaluateAlerts_FailedUpgradeIsCritical(t *testing.T) {
	// A failed upgrade never resolves itself, so it must surface as an alert
	// rather than hiding behind an "Upgrading" badge.
	hives := []alertHive{{
		ID: "h1", Name: "hive-1", Online: true,
		UpgradeFailed: true, UpgradeTarget: "fc32ae4",
		UpgradeError: "HTTP 403: cannot patch resource deployments",
	}}
	got := evaluateAlerts(newAlertState(), hives, nil, time.Now())

	var found *Alert
	for i := range got.Alerts {
		if got.Alerts[i].Type == AlertTypeFailedUpgrade {
			found = &got.Alerts[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no %s alert raised; alerts=%+v", AlertTypeFailedUpgrade, got.Alerts)
	}
	if found.Severity != AlertSeverityCritical {
		t.Errorf("severity = %q, want %q", found.Severity, AlertSeverityCritical)
	}
	if found.Reason == "" {
		t.Error("alert reason must include the cause")
	}
}
