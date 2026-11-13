package dashboard

import (
	"strings"
	"testing"
)

// The Settings → Features → Code scanning issues section (refs the code
// scanning settings API in api_config_code_scanning.go). Before it, the
// dialog had no control for code_scanning at all. The test runs the shipped
// JavaScript in node.

// codeScanningSectionStubs is the page state and the helpers the section
// calls, cut down to what a render and a save need.
const codeScanningSectionStubs = `
const assert = require('node:assert/strict');
const _configState = { dirty: {}, data: {} };
const esc = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const yamlHint = k => '[' + k + ']';
const settingHelpMark = () => '?';
`

// codeScanningSectionScript renders the section for a hive with four project
// repositories and one stale override, then stages edits and builds the PUT
// body the Save button would send.
const codeScanningSectionScript = `
const saved = {
  'o/a': { enabled: true, min_severity: 'medium', include_tools: ['CodeQL'], exclude_rules: ['cpp/x-*'], max_new_issues_per_run: 2 },
  legacy: { enabled: false, min_severity: '', include_tools: [], exclude_rules: [], max_new_issues_per_run: 0 },
  'o/gone': { enabled: true, min_severity: '', include_tools: [], exclude_rules: [], max_new_issues_per_run: 0 },
};
const cs = {
  enabled: false, interval: '', interval_default: '1h0m0s', min_severity: '', min_severity_default: 'high',
  severities: ['critical', 'high', 'medium', 'low', 'note'], include_tools: [], exclude_rules: ['!SASTID'],
  exclude_rules_default: ['BinaryArtifactsID', 'SASTID'], max_new_issues_per_run: 0, max_new_issues_per_run_default: 5,
  repos: saved,
  project_repos: [
    { repo: 'o/a', enabled: true, enabled_from: 'repo', alerts_access: 'missing_permission', alerts_access_detail: 'The hive cannot read <alerts> in o/a.', checked_at: '2026-11-13T10:00:00Z' },
    { repo: 'o/b', enabled: false, enabled_from: 'default', alerts_access: 'off' },
    { repo: 'o/legacy', enabled: false, enabled_from: 'repo', alerts_access: 'off' },
    { repo: 'o/c', enabled: true, enabled_from: 'repo', alerts_access: 'not_checked' },
  ],
};

// One row per project repository, plus the saved override for a repository
// that is no longer in project.repos.
const html = renderGovCodeScanningSection(cs);
const rows = html.split('<tr class="cs-repo-row"').slice(1);
assert.equal(rows.length, 5, html);
const row = repo => rows.find(r => r.includes('data-repo="' + repo + '"')) || '';
assert(row('o/a').includes('<option value="on" selected>'), row('o/a'));
assert(row('o/b').includes('<option value="" selected>hive-wide (off)'), row('o/b'));
assert(row('o/legacy').includes('<option value="off" selected>'), row('o/legacy'));

// The App-permission state sits next to each switch. A repository the sweep
// has not reached says so; it is never shown as readable. Server text is escaped.
assert(row('o/a').includes('App cannot read alerts (last pass 2026-11-13T10:00:00Z)'), row('o/a'));
assert(row('o/a').includes('The hive cannot read &lt;alerts&gt; in o/a.'), row('o/a'));
assert(row('o/c').includes('Not checked yet') && !row('o/c').includes('can read'), row('o/c'));
assert(row('o/b').includes('Sweep off'), row('o/b'));
assert(row('o/gone').includes('Not in project.repos') && row('o/gone').includes('<option value="drop">'), row('o/gone'));
assert(!row('o/a').includes('<option value="drop">'), row('o/a'));

// A repository's other overrides are shown, since this section does not edit them.
assert(row('o/a').includes('minimum severity medium; max new issues 2; tools CodeQL; extra rules left out cpp/x-*'), row('o/a'));

// One YAML hint per key, and the built-in exclusions are listed.
for (const key of ['code_scanning.enabled', 'code_scanning.interval', 'code_scanning.min_severity', 'code_scanning.max_new_issues_per_run', 'code_scanning.include_tools[]', 'code_scanning.exclude_rules[]', 'code_scanning.repos.<key>.enabled']) {
  assert.equal(html.split('[' + key + ']').length - 1, 1, key);
}
assert(html.includes('Left out by default: BinaryArtifactsID, SASTID.'), html);
assert(html.includes('<option value="" selected>default (high)</option>'), html);
assert(html.includes('placeholder="1h0m0s"') && html.includes('placeholder="5"'), html);

// A severity saved under an alias is offered as it is set, not silently changed.
assert(renderGovCodeScanningSection(Object.assign({}, cs, { min_severity: 'error' })).includes('<option value="error" selected>error (as set)</option>'));
assert(renderGovCodeScanningSection(Object.assign({}, cs, { min_severity: 'High' })).includes('<option value="high" selected>high</option>'));

// An empty section still renders.
assert(renderGovCodeScanningSection({}).includes('No repositories in project.repos yet.'));

// An untouched save (another field changed) sends no repos key: the server keeps the map.
assert.deepEqual(codeScanningSaveBody({ enabled: true }, saved), { enabled: true });

// One row edit sends every saved override plus the edit. The edited
// repository keeps its severity, tool, rule and cap overrides.
markDirtyCodeScanningRepo('o/b', 'on');
markDirtyCodeScanningRepo('o/a', 'off');
let body = codeScanningSaveBody(_configState.dirty['code-scanning'], saved);
assert.deepEqual(body.repos['o/a'], { enabled: false, min_severity: 'medium', include_tools: ['CodeQL'], exclude_rules: ['cpp/x-*'], max_new_issues_per_run: 2 });
assert.deepEqual(body.repos['o/b'], { enabled: true });
assert.deepEqual(body.repos.legacy, saved.legacy);
assert.deepEqual(Object.keys(body.repos).sort(), ['legacy', 'o/a', 'o/b', 'o/gone']);
assert.deepEqual(saved['o/a'].enabled, true, 'the saved map must not be changed in place');

// Back to hive-wide sends enabled null with the other overrides intact (the
// server drops an entry only when nothing is left); a bare saved key is
// reused, not duplicated; "remove this override" drops the stale entry.
markDirtyCodeScanningRepo('o/a', '');
markDirtyCodeScanningRepo('o/legacy', 'on');
markDirtyCodeScanningRepo('o/gone', 'drop');
body = codeScanningSaveBody(_configState.dirty['code-scanning'], saved);
assert.deepEqual(body.repos['o/a'], { enabled: null, min_severity: 'medium', include_tools: ['CodeQL'], exclude_rules: ['cpp/x-*'], max_new_issues_per_run: 2 });
assert.equal(body.repos.legacy.enabled, true);
assert(!('o/legacy' in body.repos) && !('o/gone' in body.repos), JSON.stringify(body.repos));

// A re-render keeps the staged choices.
const again = renderGovCodeScanningSection(cs).split('<tr class="cs-repo-row"').slice(1);
assert(again.find(r => r.includes('data-repo="o/b"')).includes('<option value="on" selected>'));
assert(again.find(r => r.includes('data-repo="o/gone"')).includes('<option value="drop" selected>'));

// Scalars and lists go as staged; the cap must be a whole number.
markCSVDirty('code-scanning', 'include_tools', ' CodeQL , Trivy ,');
markCSVDirty('code-scanning', 'exclude_rules', '');
markDirty('code-scanning', 'interval', ' 30m ');
markDirty('code-scanning', 'min_severity', 'medium');
markDirty('code-scanning', 'max_new_issues_per_run', 0);
body = codeScanningSaveBody(_configState.dirty['code-scanning'], saved);
assert.deepEqual(body.include_tools, ['CodeQL', 'Trivy']);
assert.deepEqual(body.exclude_rules, []);
assert.equal(body.interval, '30m');
assert.equal(body.min_severity, 'medium');
assert.equal(body.max_new_issues_per_run, 0);
assert.throws(() => codeScanningSaveBody({ max_new_issues_per_run: 2.5 }, saved), /whole number/);
assert.throws(() => codeScanningSaveBody({ max_new_issues_per_run: NaN }, saved), /whole number/);
`

func TestCodeScanningSettingsSectionRendersAndSavesTheWholeReposMap(t *testing.T) {
	html := indexHTML(t)
	features := extractJSFunction(t, html, "renderGovFeatures")
	if !strings.Contains(features, "${renderGovCodeScanningSection((_configState.data || {}).code_scanning || {})}") {
		t.Fatal("Settings → Features does not render the code scanning section from the settings bundle's code_scanning block")
	}
	section := extractJSFunction(t, html, "renderGovCodeScanningSection")
	for _, want := range []string{
		`data-action="toggleConfigSwitch" data-section="code-scanning" data-key="enabled"`,
		`data-change-action="markDirty" data-arg0="code-scanning" data-arg1="min_severity"`,
		`data-change-action="markDirty" data-arg0="code-scanning" data-arg1="max_new_issues_per_run"`,
		`data-change-action="markDirty" data-arg0="code-scanning" data-arg1="interval"`,
		`data-change-action="markCSVDirty" data-arg0="code-scanning" data-arg1="include_tools"`,
		`data-change-action="markCSVDirty" data-arg0="code-scanning" data-arg1="exclude_rules"`,
		`data-change-action="markDirtyCodeScanningRepo"`,
		"yamlHint('code_scanning.enabled')",
		"yamlHint('code_scanning.repos.<key>.enabled')",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("the code scanning section has no %q", want)
		}
	}
	// The save goes to the section's own endpoint, through the body builder.
	for _, want := range []string{
		"section === 'code-scanning'",
		"codeScanningSaveBody(values, ((_configState.data || {}).code_scanning || {}).repos)",
		"fetch('/api/config/code-scanning'",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("saveConfig has no %q", want)
		}
	}
	helpers := ""
	for _, name := range []string{
		"markDirty",
		"markCSVDirty",
		"decisionsDelegateSameRepo",
		"markDirtyCodeScanningRepo",
		"codeScanningReposBody",
		"codeScanningSaveBody",
		"renderGovCodeScanningSection",
	} {
		helpers += jsFunc(t, html, name) + "\n"
	}
	runNodeScript(t, codeScanningSectionStubs+helpers+codeScanningSectionScript)
}
