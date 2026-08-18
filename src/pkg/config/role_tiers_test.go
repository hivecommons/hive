package config

import "testing"

func TestRoleAtLeastOrdering(t *testing.T) {
	tests := []struct {
		role string
		tier string
		want bool
	}{
		{RoleRead, RoleRead, true},
		{RoleRead, RoleReadWrite, false},
		{RoleReadWrite, RoleRead, true},
		{RoleReadWrite, RoleMerger, false},
		{RoleMerger, RoleReadWrite, true},
		{RoleMerger, RoleOwner, false},
		{RoleOwner, RoleMerger, true},
		{"MERGER", RoleReadWrite, true},
		{"unknown", RoleRead, false},
	}
	for _, tt := range tests {
		if got := RoleAtLeast(tt.role, tt.tier); got != tt.want {
			t.Fatalf("RoleAtLeast(%q, %q) = %v, want %v", tt.role, tt.tier, got, tt.want)
		}
	}
}

func TestValidRoleAcceptsMerger(t *testing.T) {
	for _, role := range []string{RoleRead, RoleReadWrite, RoleMerger, RoleOwner, " MERGER "} {
		if !ValidRole(role) {
			t.Fatalf("ValidRole(%q) = false, want true", role)
		}
	}
	if ValidRole("admin") {
		t.Fatalf("ValidRole(admin) = true, want false")
	}
}
