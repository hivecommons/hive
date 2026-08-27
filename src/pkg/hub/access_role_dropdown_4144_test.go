package hub

import (
	"strings"
	"testing"
)

// TestManageAccessRoleDropdownExposesAllRoles pins #4144: every grantable role
// — read, read-write, merger, owner — must be selectable in the Manage Access
// "Add User" role dropdown. read-write exists in the permission model
// (config.ValidRole), so hiding it from the dropdown meant owners could not
// grant it from the UI at all.
func TestManageAccessRoleDropdownExposesAllRoles(t *testing.T) {
	for _, role := range []string{"read", "read-write", "merger", "owner"} {
		if !strings.Contains(dashboardHTML, `<option value="`+role+`"`) {
			t.Errorf("Manage Access role dropdown is missing an <option> for %q", role)
		}
	}
}

// TestManageAccessRoleDropdownHasDescriptions pins the second half of #4144:
// every role option in the Add User dropdown carries a title tooltip, an
// inline hint (#access-role-hint) explains the selected role, and the shared
// ROLE_DESCRIPTIONS map describes all four roles so the per-row role pill and
// the pending-request approval select inherit the same tooltips.
func TestManageAccessRoleDropdownHasDescriptions(t *testing.T) {
	// The static Add User dropdown: each option must have a non-empty title.
	for _, opt := range []string{
		`<option value="read" title="`,
		`<option value="read-write" title="`,
		`<option value="merger" title="`,
		`<option value="owner" title="`,
	} {
		if !strings.Contains(dashboardHTML, opt) {
			t.Errorf("Add User role option lacks a title tooltip: want substring %q", opt)
		}
	}

	// The inline hint element and the updater that keeps it in sync with the
	// current selection.
	if !strings.Contains(dashboardHTML, `id="access-role-hint"`) {
		t.Error("Manage Access modal is missing the #access-role-hint inline description element")
	}
	if !strings.Contains(dashboardHTML, `onchange="updateAccessRoleHint()"`) {
		t.Error("Add User role dropdown does not repaint the inline hint on change")
	}
	if !strings.Contains(dashboardHTML, "function updateAccessRoleHint()") {
		t.Error("updateAccessRoleHint() is not defined")
	}

	// The shared description map must cover all four roles so every role
	// select in the modal shows the same explanation.
	for _, key := range []string{"'read':", "'read-write':", "'merger':", "'owner':"} {
		if !strings.Contains(dashboardHTML, key) {
			t.Errorf("ROLE_DESCRIPTIONS is missing an entry keyed %s", key)
		}
	}
	if !strings.Contains(dashboardHTML, "function roleDescription(") {
		t.Error("roleDescription() helper is not defined")
	}

	// The per-row role pill and the pending-request approval select reuse the
	// helper for their option tooltips.
	if got := strings.Count(dashboardHTML, "escAttr(roleDescription("); got < 2 {
		t.Errorf("expected the per-row role pill AND the pending-request select to use roleDescription() for option tooltips; found %d call site(s)", got)
	}
}
