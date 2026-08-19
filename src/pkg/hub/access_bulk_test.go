package hub

import (
	"strings"
	"testing"
)

// TestManageAccessBulkControls pins the bulk multi-select affordances in the
// Manage Access dialog (issue #4147): a checkbox per user row, a bulk bar
// with a role dropdown and remove button that only appears once 2+ users are
// selected, a confirmation before bulk remove, and the last-owner guard that
// refuses to remove (or demote) every owner at once.
func TestManageAccessBulkControls(t *testing.T) {
	for _, want := range []string{
		// Per-row checkbox wired to selection state.
		`class="access-bulk-cb"`,
		`onchange="toggleBulkAccess(this,`,
		// Bulk bar markup: role dropdown + remove button.
		`id="access-bulk-bar"`,
		`id="access-bulk-role"`,
		`onclick="bulkChangeAccessRole()"`,
		`onclick="bulkRemoveAccess()"`,
		// Bar only appears with 2+ selected.
		`if (count >= 2) {`,
		// Confirmation before bulk remove.
		`hiveConfirm('Remove access for ' + selected.length + ' users`,
		// Last-owner guard: cannot bulk-remove or bulk-demote all owners.
		`function wouldOrphanOwners(selected)`,
		`Cannot remove all owners`,
		`Cannot demote all owners`,
	} {
		if !strings.Contains(dashboardHTML, want) {
			t.Errorf("dashboardHTML missing %q", want)
		}
	}
}

// TestManageAccessLastOwnerHasNoBulkCheckbox pins that the last owner's row
// renders a spacer instead of a checkbox — it can never be bulk-removed or
// bulk-demoted, so offering to select it would only mislead.
func TestManageAccessLastOwnerHasNoBulkCheckbox(t *testing.T) {
	idx := strings.Index(dashboardHTML, "var checkbox = isLastOwner ?")
	if idx == -1 {
		t.Fatal("dashboardHTML missing last-owner checkbox branch")
	}
}
