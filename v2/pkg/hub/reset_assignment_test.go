package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newResetTestHub builds a hub with the hive-oke pool configured and a buffered
// save channel, matching the shape the approve/assign handler tests use.
func newResetTestHub() *HubServer {
	return &HubServer{
		logger:    slog.Default(),
		hubSecret: testHubSecret,
		saveCh:    make(chan struct{}, 1),
		clusters:  map[string]ClusterConfig{"hive-oke": *dynamicCluster()},
	}
}

// stuckPlaceholder returns a placeholder wedged at Status=statusAssigned &&
// !ClaimDelivered — the exact dead-end the escape hatch targets (EPM /
// z-innersource class): a project stamped, but the spoke never reported it back.
func stuckPlaceholder(id string) *SaaSHive {
	return &SaaSHive{
		ID:                 id,
		Owner:              "acme-owner",
		Org:                "acme",
		Repos:              []string{"repo-a", "repo-b"},
		PrimaryRepo:        "repo-a",
		ProjectName:        "Acme Project",
		ACMMLevel:          3,
		RequestedACMMLevel: 3,
		Status:             statusAssigned,
		ClaimDelivered:     false,
		VanityURL:          "https://hosted-acme-repo-a-xxxx.hive.kubestellar.io",
		ClusterID:          "hive-oke",
		AssignedAt:         time.Now().UTC().Format(time.RFC3339),
	}
}

// TestResetAssignmentReturnsPlaceholderToPool is the happy path: an assigned but
// undelivered placeholder resets to the available-placeholder shape and is
// re-assignable afterward.
func TestResetAssignmentReturnsPlaceholderToPool(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	s := newResetTestHub()

	const id = "hosted-available-oke-11-placeholder-r05x"
	if err := saveSaaSHive(stuckPlaceholder(id)); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/reset-assignment", "", hubAdminUsername), "id", id)
	s.handleResetAssignment(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	got := loadSaaSHive(id)
	if got == nil {
		t.Fatal("hive vanished after reset")
	}
	// Available-placeholder shape (inverse of assign).
	if got.Status != statusAvailable {
		t.Errorf("Status = %q, want %q", got.Status, statusAvailable)
	}
	if got.Owner != hubAdminUsername {
		t.Errorf("Owner = %q, want admin %q", got.Owner, hubAdminUsername)
	}
	if got.Org == "" || got.Org[:len(placeholderOrgPrefix)] != placeholderOrgPrefix {
		t.Errorf("Org = %q, want %q-prefixed", got.Org, placeholderOrgPrefix)
	}
	if len(got.Repos) != 0 {
		t.Errorf("Repos = %v, want empty", got.Repos)
	}
	if got.PrimaryRepo != "" {
		t.Errorf("PrimaryRepo = %q, want empty", got.PrimaryRepo)
	}
	if got.ACMMLevel != defaultAssignACMMLevel {
		t.Errorf("ACMMLevel = %d, want default %d", got.ACMMLevel, defaultAssignACMMLevel)
	}
	if got.RequestedACMMLevel != 0 {
		t.Errorf("RequestedACMMLevel = %d, want 0", got.RequestedACMMLevel)
	}
	if got.ClaimDelivered {
		t.Error("ClaimDelivered = true, want false after reset")
	}
	if got.VanityURL != "" {
		t.Errorf("VanityURL = %q, want cleared", got.VanityURL)
	}
	if got.AssignedAt != "" {
		t.Errorf("AssignedAt = %q, want cleared", got.AssignedAt)
	}

	// The whole point: it is an available placeholder again, so the assign path
	// (which requires statusAvailable) now accepts it.
	if !isReassignable(id) {
		t.Error("placeholder is not re-assignable after reset (assign guard would 409)")
	}

	// A subsequent assign succeeds end to end.
	assignBody := `{"owner":"newuser","org":"newco","repos":"newrepo","primary_repo":"newrepo","acmm_level":2}`
	rec2 := httptest.NewRecorder()
	req2 := setPathValue(reqWithUser(http.MethodPost, "/assign", assignBody, hubAdminUsername), "id", id)
	s.handleAssignHive(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("re-assign after reset status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}
	reassigned := loadSaaSHive(id)
	if reassigned == nil || reassigned.Owner != "newuser" || reassigned.Org != "newco" {
		t.Fatalf("re-assign did not take: %+v", reassigned)
	}
}

// isReassignable mirrors the availability guard handleAssignHive enforces
// (statusAvailable). It is the machine check behind "re-assignable afterward".
func isReassignable(id string) bool {
	h := loadSaaSHive(id)
	return h != nil && h.Status == statusAvailable
}

// TestResetAssignmentRefusedWhenClaimDelivered guards a LIVE hive: a fully
// claimed placeholder (ClaimDelivered==true) must never be reset.
func TestResetAssignmentRefusedWhenClaimDelivered(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	s := newResetTestHub()

	const id = "hosted-live-hive"
	h := stuckPlaceholder(id)
	h.ClaimDelivered = true // a working, fully-claimed hive
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/reset-assignment", "", hubAdminUsername), "id", id)
	s.handleResetAssignment(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reset of a delivered claim status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	// The live hive is untouched.
	got := loadSaaSHive(id)
	if got.Status != statusAssigned || got.Owner != "acme-owner" || !got.ClaimDelivered {
		t.Errorf("live hive mutated by a refused reset: %+v", got)
	}
}

// TestResetAssignmentAlreadyAvailableIsNoOp: resetting an already-available slot
// is a 409 no-op, not a double reset.
func TestResetAssignmentAlreadyAvailableIsNoOp(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	s := newResetTestHub()

	const id = "hosted-available-slot"
	if err := saveSaaSHive(&SaaSHive{ID: id, Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/reset-assignment", "", hubAdminUsername), "id", id)
	s.handleResetAssignment(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reset of an available slot status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestResetAssignmentNotFound: a missing hive 404s.
func TestResetAssignmentNotFound(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	s := newResetTestHub()

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/reset-assignment", "", hubAdminUsername), "id", "nope")
	s.handleResetAssignment(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reset of a missing hive status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestResetAssignmentNonAdminForbidden: the route is admin-gated. We exercise
// the requireAdmin wrapper the mux applies, since the handler itself trusts the
// gate.
func TestResetAssignmentNonAdminForbidden(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	mkUser(t, "notadmin")
	s := newResetTestHub()

	const id = "hosted-stuck"
	if err := saveSaaSHive(stuckPlaceholder(id)); err != nil {
		t.Fatal(err)
	}

	gated := s.requireAdmin(s.handleResetAssignment)
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/reset-assignment", "", "notadmin"), "id", id)
	gated(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin reset status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	// And the hive is untouched.
	if got := loadSaaSHive(id); got.Status != statusAssigned {
		t.Errorf("non-admin reset mutated the hive: %+v", got)
	}
}

// TestResetAssignmentReopensProvisionRequest: an approved provision request whose
// AssignedHive points at the reset hive is flipped back to pending so re-approve
// works end to end.
func TestResetAssignmentReopensProvisionRequest(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	s := newResetTestHub()

	const id = "hosted-stuck-with-request"
	if err := saveSaaSHive(stuckPlaceholder(id)); err != nil {
		t.Fatal(err)
	}
	if err := saveProvisionRequest(&ProvisionRequest{
		Username:     "bob",
		Org:          "acme",
		Repos:        "repo-a",
		PrimaryRepo:  "repo-a",
		ACMMLevel:    3,
		Status:       provisionStatusApproved,
		DecidedBy:    hubAdminUsername,
		DecidedAt:    time.Now().UTC().Format(time.RFC3339),
		AssignedHive: id,
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/reset-assignment", "", hubAdminUsername), "id", id)
	s.handleResetAssignment(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response JSON: %v", err)
	}
	if resp["reopened_request_for"] != "bob" {
		t.Errorf("reopened_request_for = %v, want bob", resp["reopened_request_for"])
	}

	pr := loadProvisionRequest("bob")
	if pr == nil {
		t.Fatal("provision request vanished")
	}
	if pr.Status != provisionStatusPending {
		t.Errorf("request Status = %q, want %q (so re-approve works)", pr.Status, provisionStatusPending)
	}
	if pr.AssignedHive != "" {
		t.Errorf("request AssignedHive = %q, want cleared", pr.AssignedHive)
	}
}

// TestSweepStuckAssignments: a placeholder stuck past the timeout is auto-reset;
// one within the window (and one already delivered) is left alone.
func TestSweepStuckAssignments(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	s := newResetTestHub()

	// Stuck long past the timeout -> should be reset.
	stuck := stuckPlaceholder("hosted-stuck-old")
	stuck.AssignedAt = time.Now().Add(-2 * assignStuckResetTimeout).UTC().Format(time.RFC3339)
	if err := saveSaaSHive(stuck); err != nil {
		t.Fatal(err)
	}

	// Freshly assigned, within the window -> left alone.
	fresh := stuckPlaceholder("hosted-stuck-fresh")
	fresh.AssignedAt = time.Now().UTC().Format(time.RFC3339)
	if err := saveSaaSHive(fresh); err != nil {
		t.Fatal(err)
	}

	// Assigned long ago but claim delivered -> a live hive, must be left alone.
	live := stuckPlaceholder("hosted-live")
	live.ClaimDelivered = true
	live.AssignedAt = time.Now().Add(-2 * assignStuckResetTimeout).UTC().Format(time.RFC3339)
	if err := saveSaaSHive(live); err != nil {
		t.Fatal(err)
	}

	// No AssignedAt stamp (age unknowable) -> left alone even though stuck.
	unstamped := stuckPlaceholder("hosted-unstamped")
	unstamped.AssignedAt = ""
	if err := saveSaaSHive(unstamped); err != nil {
		t.Fatal(err)
	}

	s.sweepStuckAssignments()

	if got := loadSaaSHive("hosted-stuck-old"); got.Status != statusAvailable {
		t.Errorf("stuck-old Status = %q, want auto-reset to %q", got.Status, statusAvailable)
	}
	if got := loadSaaSHive("hosted-stuck-fresh"); got.Status != statusAssigned {
		t.Errorf("stuck-fresh Status = %q, want left assigned (within window)", got.Status)
	}
	if got := loadSaaSHive("hosted-live"); got.Status != statusAssigned || !got.ClaimDelivered {
		t.Errorf("live hive was swept: %+v", got)
	}
	if got := loadSaaSHive("hosted-unstamped"); got.Status != statusAssigned {
		t.Errorf("unstamped hive Status = %q, want left assigned (unknowable age)", got.Status)
	}
}
