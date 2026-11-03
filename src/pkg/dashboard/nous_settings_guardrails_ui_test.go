package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestNousSettingsDialogIsLabWideGuardrailsOnly renders the ⚙ Experiment Lab
// Configuration dialog under node and checks the split decided after
// hivecommons/hive#12510 / #12543: the dialog keeps the Trust Ladder, Output
// labels, fast-fail, timing, principles and the repo allow-list
// ("Repos experiments may target"); it has no research-question textarea and
// no "Start a new experiment" button — an experiment is defined only via
// "+ New experiment" on the section.
func TestNousSettingsDialogIsLabWideGuardrailsOnly(t *testing.T) {
	html := indexHTML(t)
	render := jsFunctionBody(t, html, "function renderNousConfig(cfg)")
	for _, banned := range []string{"nc-repo-goal", "Research Question", "Goal Definition", "nousStartCampaign", "activeCampaign", "repoCampaign"} {
		if strings.Contains(render, banned) {
			t.Errorf("renderNousConfig still contains %q; the dialog is lab-wide guardrails only", banned)
		}
	}
	save := jsFunctionBody(t, html, "async function saveNousConfig()")
	if strings.Contains(save, "config/goals") || strings.Contains(save, "repoGoal") {
		t.Error("saveNousConfig must not PUT goals; the question lives on the experiment cards")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: the settings dialog was not rendered")
	}
	script := `const assert = require('node:assert/strict');
function escapeHtml(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');}
const NOUS_REPO_ALLOWLIST_HEADING = '📂 Repos experiments may target';
const NOUS_REPO_ALLOWLIST_COPY = 'Experiments can only choose repos from this list; none selected = any repo.';
const overlay = { innerHTML: '' };
global.document = { getElementById: (id) => id === 'nous-config-overlay' ? overlay : null };
// v6 trust-ladder wiring (#12039): the served status is authoritative for
// the mode and the steps are owner-only buttons.
let _nousCache = null;
const NOUS_MODE_TITLES = { observe: 'Observe', suggest: 'Suggest', evolve: 'Evolve' };
const NOUS_OWNER_ONLY_TITLE = 'Owner only';
function nousCanChangeMode() { return true; }
function renderNousResetSection() { return ''; }
` + jsFunctionBody(t, html, "function nousLabShell(bodyHtml, footerHtml)") +
		jsFunctionBody(t, html, "function nousLabCardHead(eyebrow, heading, desc, note)") +
		jsFunctionBody(t, html, "function nousRepoSummary(active, total)") +
		jsFunctionBody(t, html, "function nousRepoPillClass(isOn)") +
		jsFunctionBody(t, html, "function nousRepoPillTitle(repo, isOn)") +
		jsFunctionBody(t, html, "function configEnvVal(cfg, key, fallback)") +
		render + `
renderNousConfig({
  campaign: { mode: 'observe', fast_fail: {}, schedule: {} },
  output: { mode: 'issue', autoHold: true },
  principles: [],
  projectRepos: ['acme/api', 'acme/web', 'acme/docs'],
  repoTargets: ['acme/api'],
  goals: { repoGoal: 'Cut CI time by 20%?' },
  activeCampaign: { id: 1, question: 'Cut CI time by 20%?', snapshots: 854, target: 672 },
});
const h = overlay.innerHTML;
// Lab-wide guardrails present.
assert.match(h, /Experiment Lab Configuration/);
assert.match(h, /Trust Ladder/);
assert.match(h, /nous-trust-step current" data-step="observe"/);
// v6 has no automatic output mode (#11699): promotion is manual on the
// verdict card, only the safety labels are configurable here.
assert.match(h, /📤 Output/);
assert.doesNotMatch(h, /nous-config-output-mode/);
assert.match(h, /id="nc-auto-hold" checked/);
assert.match(h, /⚡ Fast-Fail Bounds/);
assert.match(h, /⏱️ Experiment Timing/);
assert.match(h, /🧠 Principle Store/);
assert.match(h, /Save Configuration/);
// The repo allow-list, reframed.
assert.match(h, /<h3>📂 Repos experiments may target<\/h3>/);
assert.match(h, /Experiments can only choose repos from this list; none selected = any repo\. Click a repo to toggle it; changes apply on Save\./);
assert.match(h, /role="group" aria-label="Repos experiments may target"/);
assert.match(h, /class="nous-config-pill active is-on" aria-pressed="true" data-repo="acme\/api"/);
assert.match(h, /class="nous-config-pill is-off" aria-pressed="false" data-repo="acme\/web"/);
assert.match(h, /id="nc-repo-summary">1 of 3 repos allowed\.</);
assert.doesNotMatch(h, /Repo Targets|watched/);
// No per-experiment definition in the dialog, even when the server still
// sends goals / activeCampaign for API compatibility.
assert.doesNotMatch(h, /<textarea/);
assert.doesNotMatch(h, /nc-repo-goal|Research Question|Goal Definition|Hypothesis<\/span>/);
assert.doesNotMatch(h, /Start a new experiment|nousStartCampaign|Changing the question/);
assert.doesNotMatch(h, /Cut CI time by 20%/, 'the active question is not shown in the dialog');
assert.doesNotMatch(h, /854/);
// Empty allow-list wording.
assert.equal(nousRepoSummary(0, 3), 'No repos selected — experiments may target any repo.');
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node settings dialog rendering failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
