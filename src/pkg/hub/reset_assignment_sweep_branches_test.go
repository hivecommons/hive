package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file covers the branches of pkg/hub/saas_reset_assignment.go that the
// existing handler/sweep tests never reach:
//   - syncRegistryProvStatus: the registry-entry MATCH branch (mirror + break)
//   - clearAssignedRequestForHive: skipping requests assigned to OTHER hives,
//     and the best-effort return of the username when the re-open save fails
//   - handleResetAssignment: the already-available 409 and the save-failure 500
//   - sweepStuckAssignments: the missing-age-stamp skip, the unparseable-stamp
//     skip, and the save-failure continue

// ── syncRegistryProvStatus ──────────────────────────────────────────────────

// The whole point of the mirror is that the MATCHING in-memory registry entry
// picks up the new ProvStatus immediately (and only that entry), and that a
// save is requested so the change is durable. The existing sweep/handler tests
// run with an empty registry, so the match branch was never executed.
func TestSyncRegistryProvStatusMirrorsOnlyTheMatchingEntry(t *testing.T) {
	s := &HubServer{
		logger: slog.Default(),
		saveCh: make(chan struct{}, 1),
		registry: Registry{Hives: []RegistryEntry{
			{ID: "hive-other", ProvStatus: statusAssigned},
			{ID: "hive-target", ProvStatus: statusAssigned},
		}},
	}

	s.syncRegistryProvStatus("hive-target", statusAvailable)

	if got := s.registry.Hives[1].ProvStatus; got != statusAvailable {
		t.Errorf("matching entry ProvStatus = %q, want %q", got, statusAvailable)
	}
	if got := s.registry.Hives[0].ProvStatus; got != statusAssigned {
		t.Errorf("non-matching entry ProvStatus = %q, want untouched %q", got, statusAssigned)
	}
	select {
	case <-s.saveCh:
	default:
		t.Error("syncRegistryProvStatus did not request a registry save")
	}
}

// A hive missing from the in-memory registry must not panic or block — the
// dashboard enrich pass will pick the status up from meta.json later — but the
// save request still fires.
func TestSyncRegistryProvStatusUnknownHiveStillRequestsSave(t *testing.T) {
	s := &HubServer{
		logger:   slog.Default(),
		saveCh:   make(chan struct{}, 1),
		registry: Registry{Hives: []RegistryEntry{{ID: "hive-other"}}},
	}

	s.syncRegistryProvStatus("hive-missing", statusAvailable)

	if got := s.registry.Hives[0].ProvStatus; got != "" {
		t.Errorf("unrelated entry ProvStatus = %q, want untouched", got)
	}
	select {
	case <-s.saveCh:
	default:
		t.Error("syncRegistryProvStatus did not request a registry save")
	}
}

// ── clearAssignedRequestForHive ─────────────────────────────────────────────

// An approved request assigned to a DIFFERENT hive must be left completely
// alone: re-opening someone else's fulfilled request would put a granted
// assignment back in the admin queue.
func TestClearAssignedRequestForHiveSkipsRequestsForOtherHives(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	pr := &ProvisionRequest{
		Username:     "other-tenant",
		Org:          "other-org",
		Status:       provisionStatusApproved,
		AssignedHive: "hive-other",
	}
	if err := saveProvisionRequest(pr); err != nil {
		t.Fatal(err)
	}

	if got := clearAssignedRequestForHive("hive-being-reset"); got != "" {
		t.Errorf("clearAssignedRequestForHive = %q, want \"\" (no request touched)", got)
	}

	after := loadProvisionRequest("other-tenant")
	if after == nil {
		t.Fatal("unrelated request vanished")
	}
	if after.Status != provisionStatusApproved || after.AssignedHive != "hive-other" {
		t.Errorf("unrelated request mutated: status=%q assigned=%q", after.Status, after.AssignedHive)
	}
}

// When the re-open save fails, the linkage must STILL be reported so the
// operator knows a manual re-open is needed — that is the documented
// best-effort contract of the error branch.
func TestClearAssignedRequestForHiveReportsUsernameWhenSaveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; read-only dir cannot force a write failure")
	}
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	pr := &ProvisionRequest{
		Username:     "wedged-tenant",
		Org:          "wedged-org",
		Status:       provisionStatusApproved,
		AssignedHive: "hive-wedged",
	}
	if err := saveProvisionRequest(pr); err != nil {
		t.Fatal(err)
	}

	// Make the requests dir read-only so saveProvisionRequest's tmp-file write
	// fails. Restore before cleanup so TempDir removal succeeds.
	reqDir := provisionRequestsDir
	if err := os.Chmod(reqDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(reqDir, 0o755) })

	if got := clearAssignedRequestForHive("hive-wedged"); got != "wedged-tenant" {
		t.Errorf("clearAssignedRequestForHive = %q, want %q despite save failure", got, "wedged-tenant")
	}

	// The on-disk record must be unchanged — the flip never landed.
	after := loadProvisionRequest("wedged-tenant")
	if after == nil {
		t.Fatal("request vanished")
	}
	if after.Status != provisionStatusApproved {
		t.Errorf("on-disk status = %q, want still %q after failed save", after.Status, provisionStatusApproved)
	}
}

// ── handleResetAssignment ───────────────────────────────────────────────────

// availableSlot returns a genuinely-clean available placeholder — the shape a
// reset or never-claimed pool slot carries.
func availableSlot(id string) *SaaSHive {
	return &SaaSHive{
		ID:          id,
		Owner:       hubAdminUsername,
		Org:         placeholderOrgPrefix + id,
		ProjectName: "Available slot " + id,
		Status:      statusAvailable,
		ClusterID:   "hive-oke",
	}
}

// Resetting a slot that is ALREADY an available placeholder is a no-op the
// handler must refuse with the specific already-available message, not the
// generic wedge refusal.
func TestHandleResetAssignmentRefusesAlreadyAvailableSlot(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	s := newResetTestHub()

	const id = "hosted-available-oke-12-placeholder-idle"
	if err := saveSaaSHive(availableSlot(id)); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/reset-assignment", "", hubAdminUsername), "id", id)
	s.handleResetAssignment(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "already an available placeholder") {
		t.Errorf("body = %q, want the already-available refusal", rec.Body.String())
	}
}

// A reset whose persist fails must report 500 and leave the on-disk record in
// its original assigned state — a half-applied reset that only mutated the
// in-memory copy would strand the slot.
func TestHandleResetAssignmentReportsSaveFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; read-only dir cannot force a write failure")
	}
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	s := newResetTestHub()

	const id = "hosted-available-oke-13-placeholder-wdge"
	if err := saveSaaSHive(stuckPlaceholder(id)); err != nil {
		t.Fatal(err)
	}

	hiveDir := filepath.Join(saasHivesDir, id)
	if err := os.Chmod(hiveDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(hiveDir, 0o755) })

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/reset-assignment", "", hubAdminUsername), "id", id)
	s.handleResetAssignment(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}

	after := loadSaaSHive(id)
	if after == nil {
		t.Fatal("hive vanished")
	}
	if after.Status != statusAssigned {
		t.Errorf("on-disk Status = %q, want still %q after failed save", after.Status, statusAssigned)
	}
}

// ── sweepStuckAssignments ───────────────────────────────────────────────────

// oldStamp is an RFC3339 timestamp comfortably older than assignStuckResetTimeout.
func oldStamp() string {
	return time.Now().Add(-2 * assignStuckResetTimeout).UTC().Format(time.RFC3339)
}

// A wedge with NEITHER AssignedAt nor CreatedAt has an unknowable age; the
// sweep must leave it for the operator's manual reset rather than reset it on
// a guessed age.
func TestSweepStuckAssignmentsSkipsWedgeWithoutAnyAgeStamp(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newResetTestHub()

	const id = "hosted-available-oke-14-placeholder-nost"
	wedge := stuckPlaceholder(id)
	wedge.AssignedAt = ""
	wedge.CreatedAt = ""
	if err := saveSaaSHive(wedge); err != nil {
		t.Fatal(err)
	}

	s.sweepStuckAssignments()

	after := loadSaaSHive(id)
	if after == nil {
		t.Fatal("hive vanished")
	}
	if after.Status != statusAssigned {
		t.Errorf("Status = %q, want still %q (stamp-less wedge must not be auto-reset)", after.Status, statusAssigned)
	}
}

// An age stamp that fails to parse is equally unknowable — skip, don't guess.
func TestSweepStuckAssignmentsSkipsWedgeWithUnparseableStamp(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newResetTestHub()

	const id = "hosted-available-oke-15-placeholder-junk"
	wedge := stuckPlaceholder(id)
	wedge.AssignedAt = "not-a-timestamp"
	wedge.CreatedAt = ""
	if err := saveSaaSHive(wedge); err != nil {
		t.Fatal(err)
	}

	s.sweepStuckAssignments()

	after := loadSaaSHive(id)
	if after == nil {
		t.Fatal("hive vanished")
	}
	if after.Status != statusAssigned {
		t.Errorf("Status = %q, want still %q (unparseable stamp must not be auto-reset)", after.Status, statusAssigned)
	}
}

// A wedge whose reset cannot be persisted must be logged and SKIPPED — no
// registry mirror, no request re-open — so the sweep never advertises a reset
// it did not durably apply. The next due sweep retries it.
func TestSweepStuckAssignmentsSkipsWedgeWhoseSaveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; read-only dir cannot force a write failure")
	}
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newResetTestHub()
	s.registry = Registry{Hives: []RegistryEntry{{ID: "hosted-available-oke-16-placeholder-rofs", ProvStatus: statusAssigned}}}

	const id = "hosted-available-oke-16-placeholder-rofs"
	wedge := stuckPlaceholder(id)
	wedge.AssignedAt = oldStamp()
	if err := saveSaaSHive(wedge); err != nil {
		t.Fatal(err)
	}

	hiveDir := filepath.Join(saasHivesDir, id)
	if err := os.Chmod(hiveDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(hiveDir, 0o755) })

	s.sweepStuckAssignments()

	// The failed reset must not have been mirrored into the registry.
	if got := s.registry.Hives[0].ProvStatus; got != statusAssigned {
		t.Errorf("registry ProvStatus = %q, want untouched %q after failed save", got, statusAssigned)
	}
	select {
	case <-s.saveCh:
		t.Error("a failed reset must not request a registry save")
	default:
	}
}
