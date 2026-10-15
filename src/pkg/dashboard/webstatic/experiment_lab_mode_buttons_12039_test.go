package webstatic

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestStaticExperimentLabModeButtonsWiring pins the #12039 fix on the client:
// the section's Observe/Suggest/Evolve and Governor/Repo/Both buttons and the
// config dialog's Trust Ladder share one handler pair, are disabled with a
// reason for non-owners (the server gate is requireOwnerRole), and every
// /api/nous/* mutation checks the HTTP status instead of assuming success.
func TestStaticExperimentLabModeButtonsWiring(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		`const NOUS_MODES = ['observe', 'suggest', 'evolve'];`,
		`const NOUS_SCOPES = ['governor', 'repo', 'both'];`,
		`const NOUS_OWNER_ONLY_TITLE = `,
		`function nousCanChangeMode() {`,
		`return dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');`,
		`async function nousErrorFrom(r, fallback) {`,
		`function nousAfterModeChange(status) {`,
		`function nousRefreshTrustLadder(mode) {`,
		// Section segmented controls: shared handler, role-gated, pressed state.
		`data-action="nousSetMode" data-mode="${m}"${canSetNous ? '' : ' disabled'}`,
		`data-action="nousSetScope" data-scope="${sc}"`,
		`aria-pressed="${m === mode}"`,
		`aria-pressed="${sc === scope}"`,
		// Trust Ladder steps are real buttons dispatching the same handler.
		`<button type="button" class="nous-trust-step${isCurrent ? ' current' : ''}"`,
		`data-action="nousSetMode" data-mode="${step.id}" data-color="${step.color}"`,
		`${canSetTrust ? '' : ' disabled'}`,
		// Handlers refuse locally when the role is insufficient and toast why.
		`if (!nousCanChangeMode()) { showToast(NOUS_OWNER_ONLY_TITLE, 'error'); return; }`,
		`.nous-trust-step:disabled, .nous-mode-btn:disabled { cursor: not-allowed;`,
		// The dialog's ladder follows the served status, not the campaign file.
		`const mode = (_nousCache && _nousCache.status && _nousCache.status.mode) || camp.mode || 'observe';`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard Experiment Lab mode wiring missing %q", want)
		}
	}
	if strings.Contains(html, `<div class="nous-trust-step`) {
		t.Fatalf("Trust Ladder steps must be buttons, not inert divs")
	}
	// The dispatcher keeps routing both actions to the shared handlers.
	for _, want := range []string{
		`case 'nousSetMode': nousSetMode(el.dataset.mode || el.dataset.arg0); break;`,
		`case 'nousSetScope': nousSetScope(el.dataset.scope || el.dataset.arg0); break;`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("data-action dispatcher missing %q", want)
		}
	}
}

var nousMutationFetchRE = regexp.MustCompile(`(?s)fetch\('/api/nous/[^']*',\s*\{[^}]*method:\s*'(PUT|POST|DELETE|PATCH)'`)

// TestStaticExperimentLabMutationsCheckResponseStatus is the grep-level
// guard behind #12039: a fetch('/api/nous/…') mutation that never looks at
// r.ok (or r.status) swallows 401/403/400 and leaves the operator with a
// button that "does nothing". Every mutation must bind the response and
// inspect it within its handler, and none may fall back to window.confirm.
func TestStaticExperimentLabMutationsCheckResponseStatus(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)

	matches := nousMutationFetchRE.FindAllStringIndex(html, -1)
	if len(matches) < 10 {
		t.Fatalf("expected the Experiment Lab mutation fetches to be present, found %d", len(matches))
	}
	for _, loc := range matches {
		start := loc[0]
		lineStart := strings.LastIndex(html[:start], "\n") + 1
		prefix := strings.TrimSpace(html[lineStart:start])
		pathStart := start + len("fetch('")
		path := html[pathStart : pathStart+strings.Index(html[pathStart:], "'")]
		if prefix != "const r = await" {
			t.Errorf("mutation fetch(%q) does not bind its response as `const r = await fetch(`: %q", path, prefix)
			continue
		}
		// The handler body that follows the call, up to the end of the enclosing function.
		rest := html[loc[1]:]
		if end := strings.Index(rest, "\n    }\n"); end >= 0 {
			rest = rest[:end]
		}
		if !strings.Contains(rest, "r.ok") && !strings.Contains(rest, "r.status") {
			t.Errorf("mutation fetch(%q) never checks r.ok / r.status after the call", path)
		}
	}

	// The Experiment Lab never uses native dialogs; downgrades go through hiveConfirm.
	start := strings.Index(html, "async function nousSetMode(mode) {")
	end := strings.Index(html, "// ── Knowledge Base ──")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("could not locate the Experiment Lab handler block")
	}
	block := html[start:end]
	for _, forbidden := range []string{"window.confirm(", " confirm(", "window.alert(", " alert("} {
		if strings.Contains(block, forbidden) {
			t.Fatalf("Experiment Lab handlers must not use native dialogs (%q)", forbidden)
		}
	}
	if !strings.Contains(block, "await hiveConfirm({ title: 'Lower the trust level?'") {
		t.Fatalf("downgrade path must confirm through hiveConfirm")
	}
}
