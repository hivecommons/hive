package hub

// Tests for the untested branches of handleHubAutoUpgrade (saas_upgrade.go):
//
//   1. Preference save failure -> 500 (the toggle must not report ok when the
//      preference did not persist — the poller reads the file, not memory).
//   2. Kill switch engaged -> the preference SAVES but the immediate rollout is
//      suppressed. Per the house rule for pause tests (see upgrade_pause_test.go)
//      this carries a POSITIVE CONTROL: the identical scenario with the switch
//      off must attempt the rollout.
//   3. Enable-while-behind -> the initial trigger targets the HUB image cache
//      SHA, and a rollout failure is swallowed (logged, still 200): a doomed
//      immediate trigger must not fail saving the preference.
//   4. Enable-while-current -> no trigger.
//
// These paths guard the hub's self-upgrade control plane; before this file the
// handler sat at 61% coverage with every trigger/suppression branch untested.

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// probeHubImageExists stubs the GHCR image check with a recorder so tests can
// observe whether (and for which SHA) the handler attempted a rollout, without
// any network or kubectl. Returning false makes rolloutHubToSHA fail fast at
// the image gate, before it can reach kubectl.
func probeHubImageExists(t *testing.T) *[]string {
	t.Helper()
	var asked []string
	old := hubImageExists
	hubImageExists = func(sha string, _ *slog.Logger) bool {
		asked = append(asked, sha)
		return false
	}
	t.Cleanup(func() { hubImageExists = old })
	return &asked
}

func TestHandleHubAutoUpgradeSaveFailure(t *testing.T) {
	defer helperSetupTempDirs(t)()

	// Point the preference file into a directory that does not exist so the
	// WriteFile fails; the handler must surface that as a 500, not ok:true.
	old := hubAutoUpgradePath
	hubAutoUpgradePath = filepath.Join(t.TempDir(), "missing-dir", "hub-auto-upgrade")
	t.Cleanup(func() { hubAutoUpgradePath = old })

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	rec := httptest.NewRecorder()
	s.handleHubAutoUpgrade(rec, reqWithUser(http.MethodPut, "/hub-au", `{"auto_upgrade":true}`, "admin"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("save-failure status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to save preference") {
		t.Errorf("save-failure body = %q, want save-preference error", rec.Body.String())
	}
}

func TestHandleHubAutoUpgradePauseSuppressesInitialTrigger(t *testing.T) {
	defer helperSetupTempDirs(t)()
	resetSHACaches(t)
	setV2HubLatest(t, "bbbbbbb") // hub is behind: latest differs from running hash

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, hubGitBranch: "v2", hubGitHash: "aaaaaaa"}
	asked := probeHubImageExists(t)

	// Kill switch ON: the preference must still save, but no rollout attempt.
	pauseHub(t, s, true)
	rec := httptest.NewRecorder()
	s.handleHubAutoUpgrade(rec, reqWithUser(http.MethodPut, "/hub-au", `{"auto_upgrade":true}`, "admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("paused toggle status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"auto_upgrade":true`) {
		t.Errorf("paused toggle body = %q, want auto_upgrade:true", rec.Body.String())
	}
	if data, err := os.ReadFile(hubAutoUpgradePath); err != nil || strings.TrimSpace(string(data)) != "true" {
		t.Errorf("preference file = %q err=%v, want %q saved despite pause", data, err, "true")
	}
	if len(*asked) != 0 {
		t.Fatalf("rollout attempted while hub upgrades paused: image check called for %v", *asked)
	}

	// POSITIVE CONTROL — switch OFF, same request: the initial trigger must
	// fire, and it must target the HUB image cache SHA.
	pauseHub(t, s, false)
	rec = httptest.NewRecorder()
	s.handleHubAutoUpgrade(rec, reqWithUser(http.MethodPut, "/hub-au", `{"auto_upgrade":true}`, "admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("resumed toggle status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(*asked) != 1 || (*asked)[0] != "bbbbbbb" {
		t.Fatalf("resumed toggle image checks = %v, want exactly one for %q", *asked, "bbbbbbb")
	}
}

func TestHandleHubAutoUpgradeTriggerFailureStillSavesPreference(t *testing.T) {
	defer helperSetupTempDirs(t)()
	resetSHACaches(t)
	setV2HubLatest(t, "bbbbbbb")

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, hubGitBranch: "v2", hubGitHash: "aaaaaaa"}
	asked := probeHubImageExists(t) // returns false -> rolloutHubToSHA errors at the image gate

	rec := httptest.NewRecorder()
	s.handleHubAutoUpgrade(rec, reqWithUser(http.MethodPut, "/hub-au", `{"auto_upgrade":true}`, "admin"))

	// The doomed immediate rollout is best-effort: the toggle itself succeeds.
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger-failure status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("trigger-failure body = %q, want ok:true", rec.Body.String())
	}
	if len(*asked) != 1 {
		t.Fatalf("image checks = %v, want exactly one attempt", *asked)
	}
	if data, err := os.ReadFile(hubAutoUpgradePath); err != nil || strings.TrimSpace(string(data)) != "true" {
		t.Errorf("preference file = %q err=%v, want %q despite failed trigger", data, err, "true")
	}
	// The failed attempt never armed an upgrade (no kubectl ran).
	s.hubUpgradeMu.Lock()
	target := s.hubUpgradeTarget
	s.hubUpgradeMu.Unlock()
	if target != "" {
		t.Errorf("hubUpgradeTarget = %q after failed rollout, want empty", target)
	}
}

func TestHandleHubAutoUpgradeNoTriggerWhenCurrent(t *testing.T) {
	defer helperSetupTempDirs(t)()
	resetSHACaches(t)
	setV2HubLatest(t, "aaaaaaa") // latest == running hash: nothing to roll to

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, hubGitBranch: "v2", hubGitHash: "aaaaaaa"}
	asked := probeHubImageExists(t)

	rec := httptest.NewRecorder()
	s.handleHubAutoUpgrade(rec, reqWithUser(http.MethodPut, "/hub-au", `{"auto_upgrade":true}`, "admin"))

	if rec.Code != http.StatusOK {
		t.Fatalf("current toggle status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(*asked) != 0 {
		t.Fatalf("rollout attempted while already current: image check called for %v", *asked)
	}
}
