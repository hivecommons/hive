package config

import "testing"

// TestAuthorizedRoleReadWrite pins the parse of a "user:read-write" allowlist
// entry. The hub's Manage Access screen grants exactly three roles — read,
// read-write, owner — but read-write was missing from the spoke's accepted
// suffixes, so splitAuthorizedEntry took its "unknown suffix" branch and folded
// the whole entry into the username. The allowlist then held a user literally
// named "cbrooker27:read-write", every lookup missed, and a correctly granted
// user was rejected from their own hive.
func TestAuthorizedRoleReadWrite(t *testing.T) {
	d := DashboardConfig{AuthorizedUsers: []string{
		"zacburns:owner",
		"cbrooker27:read-write",
		"clubanderson:owner",
	}}

	role, ok := d.AuthorizedRole("cbrooker27")
	if !ok {
		t.Fatal("cbrooker27 not authorized — read-write entry failed to parse")
	}
	if role != RoleReadWrite {
		t.Errorf("role = %q, want %q", role, RoleReadWrite)
	}

	// Case-insensitive on the username, as for the other roles.
	if role, ok := d.AuthorizedRole("CBrooker27"); !ok || role != RoleReadWrite {
		t.Errorf("case-insensitive lookup = (%q,%v), want (%q,true)", role, ok, RoleReadWrite)
	}

	// The other roles must be unaffected.
	if role, ok := d.AuthorizedRole("zacburns"); !ok || role != RoleOwner {
		t.Errorf("owner = (%q,%v), want (owner,true)", role, ok)
	}

	// A genuinely unknown suffix must still fall back to treating the whole
	// entry as a username, so a stray colon cannot silently grant access.
	d2 := DashboardConfig{AuthorizedUsers: []string{"someone:bogusrole"}}
	if _, ok := d2.AuthorizedRole("someone"); ok {
		t.Error("unknown role suffix should NOT resolve to a bare username match")
	}

	// Not on the list at all.
	if _, ok := d.AuthorizedRole("stranger"); ok {
		t.Error("unlisted user must not be authorized")
	}
}

// TestSplitAuthorizedEntryReadWrite checks the parser directly.
func TestSplitAuthorizedEntryReadWrite(t *testing.T) {
	name, role := splitAuthorizedEntry("cbrooker27:read-write")
	if name != "cbrooker27" || role != RoleReadWrite {
		t.Errorf("splitAuthorizedEntry = (%q,%q), want (cbrooker27,read-write)", name, role)
	}
}
