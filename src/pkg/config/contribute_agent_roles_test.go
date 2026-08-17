package config

import "testing"

func TestContributeDelegatableRoleSet_DefaultsAndSupervisorDeny(t *testing.T) {
	h := HubConfig{}
	for _, role := range []string{"scanner", "quality", "outreach"} {
		if !h.IsContributeRoleDelegatable(role) {
			t.Fatalf("default delegatable roles should include %s", role)
		}
	}
	for _, role := range []string{"ci-maintainer", "sec-check", "architect", "supervisor"} {
		if h.IsContributeRoleDelegatable(role) {
			t.Fatalf("default delegatable roles should not include %s", role)
		}
	}
}

func TestContributeDelegatableRoleSet_ExplicitAllowListStillDeniesSupervisor(t *testing.T) {
	h := HubConfig{ContributeDelegatableRoles: []string{"ci-maintainer", " supervisor ", "SCANNER"}}
	if !h.IsContributeRoleDelegatable("ci-maintainer") {
		t.Fatal("explicit allow-list should enable ci-maintainer")
	}
	if !h.IsContributeRoleDelegatable("scanner") {
		t.Fatal("explicit allow-list should normalize scanner")
	}
	if h.IsContributeRoleDelegatable("supervisor") {
		t.Fatal("supervisor must never be delegatable, even when listed")
	}
	if !h.IsContributeRoleDelegatable("quality") || !h.IsContributeRoleDelegatable("outreach") {
		t.Fatal("safe default trio must stay enabled even when privileged roles are explicitly allow-listed")
	}
}
