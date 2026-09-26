package dashboard

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// extractJSOneLiner returns the single-line function declaration starting at
// "function <name>(" — extractJSFunction needs a multi-line body.
func extractJSOneLiner(t *testing.T, html, name string) string {
	t.Helper()
	start := strings.Index(html, "function "+name+"(")
	if start < 0 {
		t.Fatalf("function %s( not found in index.html -- was it renamed?", name)
	}
	rest := html[start:]
	return rest[:strings.Index(rest, "\n")]
}

// Changing CLI Pin Value must rebuild the Model select for the new backend.
// It used to keep the list rendered for the backend the dialog opened with,
// so an operator moving an agent from claude to codex had to save and reopen
// the dialog before a codex model could be picked.
func TestCliPinChangeRefreshesModelOptions(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "updateLaunchCmd(sel.value);\n      refreshModelOptions(sel.value);") {
		t.Error("onCliPinChange must call refreshModelOptions with the new backend")
	}

	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed; the wiring assertion above still ran")
	}
	script := extractJSFunction(t, html, "esc") + "\n    }\n" +
		extractJSOneLiner(t, html, "_normalizeModel") + "\n" +
		extractJSOneLiner(t, html, "_modelsEqual") + "\n" +
		extractJSFunction(t, html, "refreshModelOptions") + "\n    }\n" +
		`const MODELS = {
			claude: [{value: 'claude-opus-5', label: 'claude-opus-5'}],
			codex: [{value: 'gpt-6-luna', label: 'gpt-6-luna'}, {value: 'gpt-6-sol', label: 'gpt-6-sol'}],
		};
		function modelsForBackend(b) { return MODELS[b]; }
		const sel = {value: '', innerHTML: ''};
		const document = {getElementById: () => sel};
		const run = (backend, current) => {
			sel.value = current;
			refreshModelOptions(backend);
			return {value: sel.value, html: sel.innerHTML};
		};
		console.log(JSON.stringify([
			run('codex', 'claude-opus-5'),
			run('codex', 'gpt-6-sol'),
			run('codex', ''),
		]));`

	out, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	var got []struct{ Value, HTML string }
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("could not decode node output %q: %v", out, err)
	}

	for i, g := range got {
		if !strings.Contains(g.HTML, `<option value="gpt-6-luna">`) || !strings.Contains(g.HTML, `<option value="gpt-6-sol">`) {
			t.Errorf("case %d: codex models not offered: %s", i, g.HTML)
		}
	}
	// A model the new backend does not offer stays selected, marked current,
	// rather than being swapped for something the operator did not choose.
	if got[0].Value != "claude-opus-5" || !strings.Contains(got[0].HTML, "claude-opus-5 (current)") {
		t.Errorf("unoffered current model not kept: %+v", got[0])
	}
	if got[1].Value != "gpt-6-sol" || strings.Contains(got[1].HTML, "(current)") {
		t.Errorf("offered current model not reselected: %+v", got[1])
	}
	if got[2].Value != "" || strings.Contains(got[2].HTML, "(current)") {
		t.Errorf("empty model must stay on (from launch command): %+v", got[2])
	}
}
