package hub

import (
	"testing"
	"time"
)

// ── availablePlaceholderProjectName ─────────────────────────────────────────

// TestAvailablePlaceholderProjectNamePublic verifies the display name for a
// public pool slot matches the "Available slot <id>" shape.
func TestAvailablePlaceholderProjectNamePublic(t *testing.T) {
	h := &SaaSHive{ID: "oke-11-r05x", IsPublic: true}
	got := availablePlaceholderProjectName(h)
	want := "Available slot oke-11-r05x"
	if got != want {
		t.Errorf("availablePlaceholderProjectName(public) = %q, want %q", got, want)
	}
}

// TestAvailablePlaceholderProjectNamePrivate verifies the "(private)" qualifier
// is present for a non-public slot.
func TestAvailablePlaceholderProjectNamePrivate(t *testing.T) {
	h := &SaaSHive{ID: "oke-11-r05x", IsPublic: false}
	got := availablePlaceholderProjectName(h)
	want := "Available slot (private) oke-11-r05x"
	if got != want {
		t.Errorf("availablePlaceholderProjectName(private) = %q, want %q", got, want)
	}
}

// ── hiveHasAssignedIdentity ─────────────────────────────────────────────────

// TestHiveHasAssignedIdentity covers the boundary cases: a placeholder org
// (available-*) does NOT count as an assigned identity, a real org does, and a
// non-empty primary_repo does (even without a real org).
func TestHiveHasAssignedIdentity(t *testing.T) {
	tests := []struct {
		name string
		hive *SaaSHive
		want bool
	}{
		{
			name: "placeholder org only",
			hive: &SaaSHive{Org: placeholderOrgPrefix + "test-id"},
			want: false,
		},
		{
			name: "empty org",
			hive: &SaaSHive{},
			want: false,
		},
		{
			name: "real org",
			hive: &SaaSHive{Org: "acme-corp"},
			want: true,
		},
		{
			name: "primary repo only, placeholder org",
			hive: &SaaSHive{Org: placeholderOrgPrefix + "x", PrimaryRepo: "my-repo"},
			want: true,
		},
		{
			name: "primary repo only, empty org",
			hive: &SaaSHive{PrimaryRepo: "my-repo"},
			want: true,
		},
		{
			name: "real org and primary repo",
			hive: &SaaSHive{Org: "acme-corp", PrimaryRepo: "my-repo"},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hiveHasAssignedIdentity(tc.hive); got != tc.want {
				t.Errorf("hiveHasAssignedIdentity = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── assignmentInFlight ──────────────────────────────────────────────────────

// TestAssignmentInFlightPredicateBoundary covers the two required conditions and
// their negations: Status=statusAssigned AND !ClaimDelivered.
func TestAssignmentInFlightPredicateBoundary(t *testing.T) {
	tests := []struct {
		name string
		hive *SaaSHive
		want bool
	}{
		{
			name: "assigned and not delivered",
			hive: &SaaSHive{Status: statusAssigned, ClaimDelivered: false},
			want: true,
		},
		{
			name: "assigned and delivered",
			hive: &SaaSHive{Status: statusAssigned, ClaimDelivered: true},
			want: false,
		},
		{
			name: "available and not delivered",
			hive: &SaaSHive{Status: statusAvailable, ClaimDelivered: false},
			want: false,
		},
		{
			name: "empty status and not delivered",
			hive: &SaaSHive{Status: "", ClaimDelivered: false},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := assignmentInFlight(tc.hive); got != tc.want {
				t.Errorf("assignmentInFlight = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── resetHiveToAvailablePlaceholder ─────────────────────────────────────────

// TestResetHiveToAvailablePlaceholderFieldTransitions verifies every field that
// resetHiveToAvailablePlaceholder clears/sets, beyond what the handler tests
// exercise indirectly.
func TestResetHiveToAvailablePlaceholderFieldTransitions(t *testing.T) {
	h := &SaaSHive{
		ID:                 "test-hive",
		Owner:              "old-owner",
		Org:                "old-org",
		Repos:              []string{"repo1", "repo2"},
		PrimaryRepo:        "repo1",
		ProjectName:        "Old Project",
		ACMMLevel:          5,
		RequestedACMMLevel: 5,
		Status:             statusAssigned,
		ClaimDelivered:     false,
		ACMMDelivered:      true,
		ForgeDelivered:     true,
		RequestedGitHubHost: "github.example.com",
		VanityURL:          "https://test.hive.kubestellar.io",
		Error:              "some error",
		AssignedAt:         time.Now().UTC().Format(time.RFC3339),
		IsPublic:           true,
	}

	resetHiveToAvailablePlaceholder(h)

	if h.Status != statusAvailable {
		t.Errorf("Status = %q, want %q", h.Status, statusAvailable)
	}
	if h.Owner != hubAdminUsername {
		t.Errorf("Owner = %q, want %q", h.Owner, hubAdminUsername)
	}
	if h.Org != placeholderOrgPrefix+"test-hive" {
		t.Errorf("Org = %q, want %q", h.Org, placeholderOrgPrefix+"test-hive")
	}
	if h.Repos != nil {
		t.Errorf("Repos = %v, want nil", h.Repos)
	}
	if h.PrimaryRepo != "" {
		t.Errorf("PrimaryRepo = %q, want empty", h.PrimaryRepo)
	}
	if h.ACMMLevel != defaultAssignACMMLevel {
		t.Errorf("ACMMLevel = %d, want %d", h.ACMMLevel, defaultAssignACMMLevel)
	}
	if h.RequestedACMMLevel != 0 {
		t.Errorf("RequestedACMMLevel = %d, want 0", h.RequestedACMMLevel)
	}
	if h.ClaimDelivered {
		t.Error("ClaimDelivered = true, want false")
	}
	if h.ACMMDelivered {
		t.Error("ACMMDelivered = true, want false")
	}
	if h.ForgeDelivered {
		t.Error("ForgeDelivered = true, want false")
	}
	if h.RequestedGitHubHost != "" {
		t.Errorf("RequestedGitHubHost = %q, want empty", h.RequestedGitHubHost)
	}
	if h.VanityURL != "" {
		t.Errorf("VanityURL = %q, want empty", h.VanityURL)
	}
	if h.Error != "" {
		t.Errorf("Error = %q, want empty", h.Error)
	}
	if h.AssignedAt != "" {
		t.Errorf("AssignedAt = %q, want empty", h.AssignedAt)
	}
	// ProjectName is set to the available placeholder display name.
	if h.ProjectName == "" || h.ProjectName == "Old Project" {
		t.Errorf("ProjectName = %q, want available placeholder name", h.ProjectName)
	}
}

// ── clearAssignedRequestForHive edge cases ──────────────────────────────────

// TestClearAssignedRequestForHiveNoMatch verifies that a hive with no linked
// provision request returns "".
func TestClearAssignedRequestForHiveNoMatch(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	got := clearAssignedRequestForHive("nonexistent-hive")
	if got != "" {
		t.Errorf("clearAssignedRequestForHive(no-match) = %q, want empty", got)
	}
}

// TestClearAssignedRequestForHiveDeniedNotReopened verifies that a DENIED
// provision request (not approved) is left alone even when its AssignedHive
// matches — only approved requests should be re-opened.
func TestClearAssignedRequestForHiveDeniedNotReopened(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const hiveID = "test-hive-denied"
	if err := saveProvisionRequest(&ProvisionRequest{
		Username:     "charlie",
		Org:          "acme",
		Repos:        "repo-a",
		PrimaryRepo:  "repo-a",
		Status:       provisionStatusDenied,
		AssignedHive: hiveID,
	}); err != nil {
		t.Fatal(err)
	}

	got := clearAssignedRequestForHive(hiveID)
	if got != "" {
		t.Errorf("denied request re-opened: got %q, want empty", got)
	}

	pr := loadProvisionRequest("charlie")
	if pr != nil && pr.Status != provisionStatusDenied {
		t.Errorf("denied request status changed to %q", pr.Status)
	}
}

// ── isResettableWedge additional edge cases ─────────────────────────────────

// TestIsResettableWedgeAvailableWithRealOrg verifies the defensive case: a hive
// at statusAvailable but carrying a real org (a legacy state that should not
// exist but has been observed) IS resettable.
func TestIsResettableWedgeAvailableWithRealOrg(t *testing.T) {
	h := &SaaSHive{
		ID:     "wedge",
		Org:    "real-org",
		Status: statusAvailable,
	}
	if !isResettableWedge(h) {
		t.Error("available + real org should be resettable (defensive case)")
	}
}

// TestIsResettableWedgeProvisioningNoIdentity verifies that a non-wedged state
// (e.g. provisioning without assigned identity) is NOT resettable.
func TestIsResettableWedgeProvisioningNoIdentity(t *testing.T) {
	h := &SaaSHive{
		ID:     "provisioning",
		Org:    placeholderOrgPrefix + "provisioning",
		Status: "provisioning",
	}
	if isResettableWedge(h) {
		t.Error("provisioning + no identity should NOT be resettable")
	}
}
