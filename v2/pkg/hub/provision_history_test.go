package hub

import (
	"os"
	"testing"
)

// TestDeniedProvisionRequestIsRetained pins the behaviour the history table
// depends on. Denial used to DELETE the record, which made a denied request
// indistinguishable from one that was never made — the table could only ever
// show approvals, and an admin could not answer "did we already turn this
// person down, and why?".
func TestDeniedProvisionRequestIsRetained(t *testing.T) {
	dir := t.TempDir()
	old := provisionRequestsDir
	provisionRequestsDir = dir
	t.Cleanup(func() { provisionRequestsDir = old })

	pr := &ProvisionRequest{
		Username:    "requester1",
		Org:         "acme",
		PrimaryRepo: "widgets",
		RequestedAt: "2026-07-28T00:00:00Z",
		Status:      provisionStatusPending,
	}
	if err := saveProvisionRequest(pr); err != nil {
		t.Fatalf("saveProvisionRequest: %v", err)
	}

	// Simulate what handleDenyProvision now records.
	pr.Status = provisionStatusDenied
	pr.DecidedBy = "admin1"
	pr.DecidedAt = "2026-07-28T01:00:00Z"
	pr.DenyReason = "no capacity"
	if err := saveProvisionRequest(pr); err != nil {
		t.Fatalf("saveProvisionRequest (denied): %v", err)
	}

	got := loadProvisionRequest("requester1")
	if got == nil {
		t.Fatal("denied request was not retained — history table would lose it")
	}
	if got.Status != provisionStatusDenied {
		t.Errorf("status = %q, want %q", got.Status, provisionStatusDenied)
	}
	if got.DecidedBy != "admin1" || got.DenyReason != "no capacity" {
		t.Errorf("decision audit not persisted: by=%q reason=%q", got.DecidedBy, got.DenyReason)
	}

	// It must still appear in the admin listing that feeds the table.
	all := listProvisionRequests()
	if len(all) != 1 || all[0].Username != "requester1" {
		t.Errorf("listProvisionRequests() = %+v, want the denied record", all)
	}

	// A denied record must NOT block a fresh request: the re-request gate keys
	// on "pending" only, so verify the retained status is not pending.
	if got.Status == provisionStatusPending {
		t.Error("denied record still reads as pending — would block re-requesting")
	}
	_ = os.RemoveAll(dir)
}

// TestApprovedProvisionRequestRecordsAssignedHive covers the other half of the
// audit trail: an approval is only useful in the table if it says which hive
// the requester actually received.
func TestApprovedProvisionRequestRecordsAssignedHive(t *testing.T) {
	dir := t.TempDir()
	old := provisionRequestsDir
	provisionRequestsDir = dir
	t.Cleanup(func() { provisionRequestsDir = old })

	pr := &ProvisionRequest{Username: "requester2", Org: "acme", Status: provisionStatusPending}
	if err := saveProvisionRequest(pr); err != nil {
		t.Fatalf("saveProvisionRequest: %v", err)
	}
	pr.Status = provisionStatusApproved
	pr.DecidedBy = "admin1"
	pr.DecidedAt = "2026-07-28T02:00:00Z"
	pr.AssignedHive = "hosted-available-oke-06-placeholder-04si"
	if err := saveProvisionRequest(pr); err != nil {
		t.Fatalf("saveProvisionRequest (approved): %v", err)
	}

	got := loadProvisionRequest("requester2")
	if got == nil {
		t.Fatal("approved request missing")
	}
	if got.AssignedHive != "hosted-available-oke-06-placeholder-04si" {
		t.Errorf("assigned_hive = %q, want the placeholder id", got.AssignedHive)
	}
	if got.DecidedBy != "admin1" {
		t.Errorf("decided_by = %q, want admin1", got.DecidedBy)
	}
}
