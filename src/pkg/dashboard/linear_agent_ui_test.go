package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// ── RFC #4492 Part 2, component G: Linear agent dashboard surface ────────────
//
// Guard for the renderAll()/hiveToast() class of bug (the
// convergence_ui_test.go pattern): every function the Linear agent panel
// dispatches to must actually be DEFINED in the inline script, so a click
// cannot throw ReferenceError and get mislabelled as an action failure.
func TestLinearAgentUIHasNoUndefinedCallees(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	// The Features tab must render the Linear Agent panel shell, its three
	// actions, and load status when the tab opens.
	for _, snippet := range []string{
		`id="linear-agent-panel"`,
		`data-action="linearAgentConnect"`,
		`data-action="linearAgentDisconnect"`,
		`data-action="loadLinearAgentStatus"`,
		`case 'loadLinearAgentStatus': loadLinearAgentStatus(); break;`,
		`case 'linearAgentConnect': linearAgentConnect(); break;`,
		`case 'linearAgentDisconnect': linearAgentDisconnect(); break;`,
		`if (tabId === 'Features') loadLinearAgentStatus();`,
		`/api/linear/agent/status`,
		`/api/linear/agent/install`,
		`/api/linear/agent/disconnect`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}

	// Every function the panel's paths call must be defined in the inline
	// script — a dispatch case naming an undefined function is exactly the
	// renderAll() bug.
	for _, fn := range []string{
		"loadLinearAgentStatus",
		"linearAgentConnect",
		"linearAgentDisconnect",
		"esc",
		"showToast",
	} {
		defined := regexp.MustCompile(`(?:function\s+` + regexp.QuoteMeta(fn) + `\s*\(|(?:const|let|var)\s+` + regexp.QuoteMeta(fn) + `\s*=)`)
		if !defined.MatchString(html) {
			t.Errorf("index.html calls %s() from the Linear agent UI but never defines it", fn)
		}
	}

	// The panel reads exactly the JSON field names the status handler and the
	// session tracker emit — a silent rename on either side blanks the UI.
	for _, field := range []string{
		"st.configured", "st.connected", "st.viewer_id", "st.session_agent",
		"st.webhook_path", "st.sessions",
		"issue_identifier", "issue_title", "issue_url", "last_event_at",
	} {
		if !strings.Contains(html, field) {
			t.Errorf("index.html Linear panel no longer references %q", field)
		}
	}
}
