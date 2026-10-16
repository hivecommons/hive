package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #12076: turning on dashboard.experiment_lab did nothing visible on a spoke
// below ACMM L4. applyDashboardFeatureVisibility revealed the sidebar entry
// and section on the status poll, then applyACMMVisibility re-hid both behind
// ACMM_NOUS_MIN_LEVEL = 4 on the next tick, and the toggle never said so.
// The owner's decision: the gate stays but drops to L3, and both gates must
// pass. This test EXECUTES applyACMMVisibility against a DOM stub and pins
// that the nous-section nav item, section and sidebar summary are hidden at
// L0/L2 even with the flag on, shown at L3/L6 with the flag on, and hidden at
// L6 with the flag off.
func TestApplyACMMVisibilityExperimentLabRequiresFlagAndL3_12076(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the Experiment Lab visibility rule was NOT executed by this run")
	}

	html := indexHTML(t)
	script := jsFunc(t, html, "applyACMMVisibility") + "\n" + experimentLabVisibilityAssertions

	dir := t.TempDir()
	path := filepath.Join(dir, "experiment-lab-visibility.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("Experiment Lab visibility behaviour check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

const experimentLabVisibilityAssertions = `
function makeEl(attrs) {
  return {
    style: { display: '' },
    _attrs: attrs || {},
    getAttribute(k) { return (k in this._attrs) ? this._attrs[k] : null; },
    querySelector() { return null; },
    querySelectorAll() { return []; },
  };
}
const byId = {};
['governor','advisory-section','token-panel','repos-section','contributors-section','leaderboard-section','nous-section','nous-sidebar-config','debug-section','logs-section','agents','oc-agent-detail'].forEach(id => { byId[id] = makeEl({ id }); });
const navNous = makeEl({ 'data-section': 'nous-section' });
const navTokens = makeEl({ 'data-section': 'token-panel' });
global.document = {
  getElementById(id) { return byId[id] || null; },
  querySelectorAll(sel) { return sel === '.oc-nav-item' ? [navNous, navTokens] : []; },
};
let flag = true;
function experimentLabEnabled() { return flag; }

let fails = 0;
function check(name, cond) { if (!cond) { fails++; console.log('FAIL ' + name); } }

// Flag on, below L3: hidden — the flag alone is not enough.
for (const lvl of [0, 2]) {
  applyACMMVisibility(lvl, null);
  check('L' + lvl + ' + flag on: sidebar entry hidden', navNous.style.display === 'none');
  check('L' + lvl + ' + flag on: section hidden', byId['nous-section'].style.display === 'none');
  check('L' + lvl + ' + flag on: sidebar scope/mode summary hidden', byId['nous-sidebar-config'].style.display === 'none');
}

// Flag on, L3 (the new ACMM_NOUS_MIN_LEVEL) and L6: visible.
for (const lvl of [3, 6]) {
  applyACMMVisibility(lvl, null);
  check('L' + lvl + ' + flag on: sidebar entry visible', navNous.style.display !== 'none');
  check('L' + lvl + ' + flag on: section visible', byId['nous-section'].style.display !== 'none');
  check('L' + lvl + ' + flag on: sidebar scope/mode summary visible', byId['nous-sidebar-config'].style.display !== 'none');
}
check('L6: tokens nav visible (control)', navTokens.style.display !== 'none');

// Flag off, L6: hidden regardless of level.
flag = false;
applyACMMVisibility(6, null);
check('L6 + flag off: sidebar entry hidden', navNous.style.display === 'none');
check('L6 + flag off: section hidden', byId['nous-section'].style.display === 'none');
check('L6 + flag off: sidebar scope/mode summary hidden', byId['nous-sidebar-config'].style.display === 'none');

process.exit(fails ? 1 : 0);
`

// TestExperimentLabToggleACMMNotice12076 executes the Settings-toggle notice
// helpers: below L3 the helper text names the current level and the flip-time
// notice/toast fire; at L3+ only the standing "Requires ACMM Level 3 or
// higher." line renders and nothing else is surfaced.
func TestExperimentLabToggleACMMNotice12076(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the Experiment Lab toggle notice was NOT executed by this run")
	}
	html := indexHTML(t)
	script := "const EXPERIMENT_LAB_MIN_ACMM_LEVEL = 3;\n" +
		"function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;'); }\n" +
		jsFunc(t, html, "currentACMMLevelForSettings") + "\n" +
		jsFunc(t, html, "experimentLabACMMNotice") + "\n" +
		jsFunc(t, html, "experimentLabToggleHelpHTML") + "\n" +
		jsFunc(t, html, "experimentLabNoticeElements") + "\n" +
		jsFunc(t, html, "noteExperimentLabToggle") + "\n" +
		jsFunc(t, html, "noteExperimentLabSaved") + "\n" +
		experimentLabToggleNoticeAssertions
	dir := t.TempDir()
	path := filepath.Join(dir, "experiment-lab-toggle-notice.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("Experiment Lab toggle notice behaviour check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

const experimentLabToggleNoticeAssertions = `
const toasts = [];
function showToast(msg, type) { toasts.push({ msg, type }); }
// Both surfaces that expose dashboard.experiment_lab render a notice slot:
// the Dashboard tab toggle and the Features tab "Nous / Experiment Lab" card.
const SURFACES = ['dashboard', 'features'];
const notices = SURFACES.map(() => ({ hidden: true, textContent: '' }));
global.document = { querySelectorAll(sel) { return sel === '[data-experiment-lab-acmm-notice]' ? notices.slice() : []; }, getElementById() { return null; } };
global.window = { _lastStatus: { acmmLevel: 2 } };

let fails = 0;
function check(name, cond) { if (!cond) { fails++; console.log('FAIL ' + name); } }

// Below L3: helper text on BOTH surfaces states the requirement AND the current level.
let help;
for (const surface of SURFACES) {
  help = experimentLabToggleHelpHTML(2, surface);
  check(surface + ' L2 help states requirement', help.includes('Requires ACMM Level 3 or higher.'));
  check(surface + ' L2 help names current level', help.includes('Your hive is at Level 2 — Experiment Lab will appear once it reaches Level 3.'));
  check(surface + ' L2 help has surface-scoped ids', help.includes('id="experiment-lab-acmm-help-' + surface + '"') && help.includes('id="experiment-lab-acmm-notice-' + surface + '"'));
}

// Flip on at L2: inline notice on every surface + ONE warning toast, no native dialog.
noteExperimentLabToggle(true);
notices.forEach((notice, i) => check(SURFACES[i] + ' L2 flip on: inline notice shown', notice.hidden === false && notice.textContent.includes('stay hidden until your hive reaches ACMM Level 3 (currently Level 2)')));
check('L2 flip on: single toast raised', toasts.length === 1 && toasts[0].type === 'warning' && toasts[0].msg.includes('ACMM Level 3'));
// Flip back off: notices clear.
noteExperimentLabToggle(false);
notices.forEach((notice, i) => check(SURFACES[i] + ' L2 flip off: inline notice cleared', notice.hidden === true && notice.textContent === ''));
// Successful save at L2 with the flag on: "saved, but hidden until L3" toast.
noteExperimentLabSaved({ experiment_lab: true });
check('L2 save: toast says saved but hidden', toasts.length === 2 && toasts[1].msg === 'Experiment Lab setting saved, but the section stays hidden until your hive reaches ACMM Level 3 (currently Level 2).');
noteExperimentLabSaved({ experiment_lab: false });
check('L2 save with flag off: no toast', toasts.length === 2);

// L3 and above: requirement line only, no level warning, no toast, no notice.
toasts.length = 0;
for (const lvl of [3, 6]) {
  for (const surface of SURFACES) {
    help = experimentLabToggleHelpHTML(lvl, surface);
    check(surface + ' L' + lvl + ' help states requirement', help.includes('Requires ACMM Level 3 or higher.'));
    check(surface + ' L' + lvl + ' help has no below-level text', !help.includes('Your hive is at Level') && !help.includes('experiment-lab-acmm-below'));
  }
  window._lastStatus.acmmLevel = lvl;
  noteExperimentLabToggle(true);
  noteExperimentLabSaved({ experiment_lab: true });
  check('L' + lvl + ': no toast', toasts.length === 0);
  notices.forEach((notice, i) => check(SURFACES[i] + ' L' + lvl + ': no inline notice', notice.hidden === true));
}
process.exit(fails ? 1 : 0);
`
