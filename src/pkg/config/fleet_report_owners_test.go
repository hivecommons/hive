package config

import (
	"strings"
	"testing"
)

func TestDashboardConfigOwnerLogins(t *testing.T) {
	d := DashboardConfig{AuthorizedUsers: []string{"alice", "viewer", "bob:owner", "carol:read", "github:dana:owner", "", "dave:"}}
	if got := d.OwnerLogins(); strings.Join(got, ",") != "alice,bob,github:dana" {
		t.Fatalf("OwnerLogins = %v", got)
	}
	if got := (DashboardConfig{AuthorizedUsers: []string{"eve:read", "frank"}}).OwnerLogins(); got != nil {
		t.Fatalf("no owner-role entry should yield nil, got %v", got)
	}
	if got := (DashboardConfig{}).OwnerLogins(); got != nil {
		t.Fatalf("empty allowlist = %v", got)
	}
	var fr FleetReportConfig
	if !fr.IncludesOwners() {
		t.Fatal("include_owners must default to true")
	}
	off := false
	fr.IncludeOwners = &off
	if fr.IncludesOwners() {
		t.Fatal("explicit include_owners=false must win")
	}
}
