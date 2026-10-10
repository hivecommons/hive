package hub

import (
	"strings"
	"testing"
)

// TestManageAccessModalTwoColumnFixedLayout pins the Manage Access shell: a
// fixed-size flex dialog with non-resizing header/footer and independently
// scrolling left/right panes. The Add User picker lives in the right pane and
// its results list scrolls in that pane instead of expanding the dialog.
func TestManageAccessModalTwoColumnFixedLayout(t *testing.T) {
	for _, want := range []string{
		`class="access-modal-card"`,
		`width:min(1280px,94vw)`,
		`height:min(86vh,900px)`,
		`display:flex;flex-direction:column;overflow:hidden`,
		`.access-modal-header { flex:0 0 auto`,
		`.access-modal-body { flex:1 1 auto;min-height:0;display:grid;grid-template-columns:minmax(0,7fr) minmax(0,5fr)`,
		`.access-left-pane, .access-right-pane { min-height:0;overflow:auto; }`,
		`id="access-left-pane" class="access-left-pane"`,
		`id="access-right-pane" class="access-right-pane"`,
		`class="access-panel access-add-panel"`,
		`.access-add-panel .access-typeahead-list { position:static;z-index:auto;max-height:none;flex:1 1 auto;min-height:0`,
		`@media (max-width: 900px)`,
		`.access-modal-body { grid-template-columns:1fr;overflow:auto; }`,
	} {
		if !strings.Contains(dashboardHTML, want) {
			t.Errorf("dashboardHTML missing %q", want)
		}
	}
}
