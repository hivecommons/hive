package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// ── #4263: convergence rollout settings + soak summary UI ──────────────────────
//
// Guard for the renderAll()/hiveToast() class of bug: every function the new
// convergence UI calls must actually be DEFINED in the inline script, so a
// click cannot throw ReferenceError and get mislabelled as an action failure
// by a nearby catch block (the budget_reset_ui_test.go pattern).
func TestConvergenceUIHasNoUndefinedCallees(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	// The Features tab must render the owner-only convergence rollout control
	// (off/shadow/enforce), dirty-tracked under the 'convergence' section, and
	// the soak summary surface.
	for _, snippet := range []string{
		`data-arg0="convergence" data-arg1="mode"`,
		`data-action="loadConvergenceSoak"`,
		`id="convergence-soak-summary"`,
		`'/api/config/convergence'`,
		`/api/convergence/soak`,
		`case 'loadConvergenceSoak': loadConvergenceSoak(); break;`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}

	// Every function the new UI paths call must be defined somewhere in the
	// inline script. A dispatch case or handler that names an undefined
	// function is exactly the renderAll() bug.
	for _, fn := range []string{
		"loadConvergenceSoak",
		"markDirty",
		"esc",
		"showToast",
		"saveErrorMessage",
		"_inceptionAuthHeaders",
	} {
		defined := regexp.MustCompile(`(?:function\s+` + regexp.QuoteMeta(fn) + `\s*\(|(?:const|let|var)\s+` + regexp.QuoteMeta(fn) + `\s*=)`)
		if !defined.MatchString(html) {
			t.Errorf("index.html calls %s() from the convergence UI but never defines it", fn)
		}
	}
}
