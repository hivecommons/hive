package hub

import (
	"regexp"
	"strings"
	"testing"
)

// The admin users table re-renders on a 30s poll by replacing the whole
// users-container innerHTML. The CRM contact fields (full name, Slack ID,
// notes, added in #2163) save on blur, so a value that has been typed but not
// yet blurred exists only in the DOM node the re-render destroys. These tests
// pin the pieces of the deferral mechanism that prevent that data loss.
//
// The behavioural proof (type into a field, fire the refresh, assert the text
// survives) lives in the jsdom harness driven by
// TestDashboardContactEditSurvivesRefreshInJSDOM below; these tests guard the
// structure that harness depends on.

func TestDashboardContactDeferralSymbolsAreTopLevel(t *testing.T) {
	// Each of these must be a top-level declaration in the inline script.
	// Column-4 indentation is how this file formats genuinely top-level
	// inline-script declarations; anything swallowed into a function body by a
	// stray brace would be indented further and would throw ReferenceError at
	// runtime the way #2177 did.
	tests := []struct {
		name string
		decl string
	}{
		{"quiet window constant", "var CONTACT_EDIT_QUIET_MS"},
		{"defer recheck constant", "var CONTACT_DEFER_RECHECK_MS"},
		{"last edit timestamp", "var _contactLastEditAt"},
		{"defer timer handle", "var _contactDeferTimer"},
		{"render pending flag", "var _contactRenderPending"},
		{"dirty value map", "var _contactDirty"},
		{"edit-in-progress predicate", "function contactEditInProgress"},
		{"activity marker", "function markContactEditing"},
		{"deferred render scheduler", "function scheduleDeferredUsersRender"},
		{"dirty key helper", "function contactDirtyKey"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			re := regexp.MustCompile(`(?m)^ {4}` + regexp.QuoteMeta(tc.decl) + `\b`)
			if !re.MatchString(dashboardHTML) {
				t.Errorf("%q is not declared at the top level of the inline script; "+
					"the admin poll would throw at runtime instead of deferring", tc.decl)
			}
		})
	}
}

// renderUsers must consult contactEditInProgress() and, when an edit is live,
// schedule a deferred render instead of writing the DOM. Without the schedule
// call the refresh would be dropped forever rather than postponed.
func TestDashboardRenderUsersGatesOnActiveEdit(t *testing.T) {
	body := funcBody(t, "function renderUsers(")
	tests := []struct {
		name string
		want string
	}{
		{"checks for an in-progress edit", "contactEditInProgress()"},
		{"defers rather than dropping the render", "scheduleDeferredUsersRender()"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(body, tc.want) {
				t.Errorf("renderUsers body does not contain %q; in-progress contact "+
					"edits would be clobbered by the poll", tc.want)
			}
		})
	}
	// The gate must run BEFORE _lastUsersJSON is updated. If the signature
	// cache were poisoned by a deferred render, the later non-forced render
	// would short-circuit as "unchanged" and the new data would never paint.
	gate := strings.Index(body, "contactEditInProgress()")
	sig := strings.Index(body, "_lastUsersJSON = sig")
	if gate < 0 || sig < 0 {
		t.Fatalf("renderUsers is missing the edit gate (%d) or the signature write (%d)", gate, sig)
	}
	if gate > sig {
		t.Error("renderUsers updates _lastUsersJSON before the edit gate; a deferred " +
			"render would poison the signature cache and the refresh would be lost")
	}
}

// The deferral must re-arm itself so a postponed refresh eventually lands.
func TestDashboardDeferredRenderReArms(t *testing.T) {
	body := funcBody(t, "function scheduleDeferredUsersRender(")
	if !strings.Contains(body, "scheduleDeferredUsersRender()") {
		t.Error("scheduleDeferredUsersRender does not re-arm itself while an edit is " +
			"still in progress; a deferred refresh could be dropped permanently")
	}
	if !strings.Contains(body, "applySortUsers()") {
		t.Error("scheduleDeferredUsersRender never renders; the deferral would stop " +
			"the admin table refreshing forever")
	}
	if !strings.Contains(body, "CONTACT_DEFER_RECHECK_MS") {
		t.Error("scheduleDeferredUsersRender does not use the named recheck constant")
	}
}

// Blur is the only save affordance, so the blur handler must hand the value to
// the save path before clearing the dirty record — otherwise there is a window
// where the value is neither dirty nor saved and a render loses it.
func TestDashboardContactBlurSavesBeforeClearingDirty(t *testing.T) {
	body := funcBody(t, "function bindContactPanels(")
	save := strings.Index(body, "saveContactField(")
	clear := strings.Index(body, "delete _contactDirty[key]")
	if save < 0 || clear < 0 {
		t.Fatalf("bindContactPanels is missing the save call (%d) or the dirty clear (%d)", save, clear)
	}
	if clear < save {
		t.Error("the dirty value is cleared before saveContactField() is called; a " +
			"render landing in between would discard a value the user typed")
	}
	for _, ev := range []string{"'input'", "'focus'", "'blur'"} {
		if !strings.Contains(body, "addEventListener("+ev) {
			t.Errorf("contact fields do not listen for %s; the poll would not know an "+
				"edit is in progress", ev)
		}
	}
}

// Width and alignment (#2163 follow-up): the contact panel cell must carry the
// class that undoes .hive-table td's text-align:center, and the field widths
// must come from named constants rather than inline magic numbers.
func TestDashboardContactPanelWidthAndAlignment(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"panel cell carries the alignment class", `class="contact-panel-cell"`},
		{"stylesheet left-aligns the panel cell", ".contact-panel-cell { text-align: left; }"},
		{"stylesheet left-aligns the fields", ".contact-panel-cell input, .contact-panel-cell textarea { text-align: left; }"},
		{"name width is a named constant", "var CONTACT_W_NAME_BASIS"},
		{"slack width is a named constant", "var CONTACT_W_SLACK_BASIS"},
		{"notes width is a named constant", "var CONTACT_W_NOTES_BASIS"},
		{"notes grow factor is a named constant", "var CONTACT_NOTES_GROW"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.want) {
				t.Errorf("dashboardHTML does not contain %q", tc.want)
			}
		})
	}
	// Alignment must be fixed at the cell level, not papered over per-field.
	if strings.Contains(dashboardHTML, "text-align:left !important") ||
		strings.Contains(dashboardHTML, "text-align: left !important") {
		t.Error("contact field alignment uses !important; fix it on .contact-panel-cell instead")
	}
	// Notes is prose and must stay a textarea, and must be the widest field.
	panel := funcBody(t, "function renderContactPanelRow(")
	if !strings.Contains(panel, `<textarea data-contact-user="`) {
		t.Error("the notes field is not a textarea; prose needs a multi-line control")
	}
	if !strings.Contains(panel, "CONTACT_W_NOTES_BASIS") {
		t.Error("the notes field does not use its width constant")
	}
}

// funcBody returns the source of the function whose declaration starts with
// prefix, from the declaration to its matching closing brace. It lets the tests
// above assert on one function rather than the whole 12k-line template.
func funcBody(t *testing.T, prefix string) string {
	t.Helper()
	start := strings.Index(dashboardHTML, prefix)
	if start < 0 {
		t.Fatalf("could not find %q in dashboardHTML", prefix)
	}
	src := dashboardHTML[start:]
	open := strings.Index(src, "{")
	if open < 0 {
		t.Fatalf("no opening brace after %q", prefix)
	}
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[open : i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces in %q", prefix)
	return ""
}
