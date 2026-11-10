package dashboard

import (
	"strings"
	"testing"
)

// Dashboard controls for settings the admin MCP could already change
// (refs #17555, #17556): auto_merge.no_ci_ok and allow_unprotected_base,
// decisions.delegate repos and effect_timeout, and an agent whose mode is
// NO_GITHUB. Each test runs the shipped JavaScript in node.

// parityMarkDirtyStub is markDirty plus the state it writes, so a test can
// read what a save would send.
const parityMarkDirtyStub = `
const assert = require('node:assert/strict');
const _configState = { dirty: {}, data: {} };
`

func TestDashboardParityAutoMergeNoCIAndUnprotectedBaseStageFullLists(t *testing.T) {
	html := indexHTML(t)
	features := extractJSFunction(t, html, "renderGovFeatures")
	for _, want := range []string{
		`id="am-no-ci-ok"`,
		"amRepoPolicyOptions(am.no_ci_ok)",
		`id="am-allow-unprotected-base"`,
		"amRepoPolicyOptions(am.allow_unprotected_base)",
		"yamlHint('auto_merge.no_ci_ok')",
		"no CI checks at all",
	} {
		if !strings.Contains(features, want) {
			t.Fatalf("Settings → Features → Auto-merge has no %q", want)
		}
	}
	runNodeScript(t, parityMarkDirtyStub+jsFunc(t, html, "markDirty")+"\n"+jsFunc(t, html, "markDirtyAutoMergeRepoPolicy")+`
const selects = {
  'am-squash-only': ['o/a'],
  'am-no-ci-ok': ['o/docs', 'o/site'],
  'am-allow-unprotected-base': [],
};
const document = {
  getElementById: id => selects[id] ? { selectedOptions: selects[id].map(value => ({ value })) } : null,
  querySelectorAll: () => [],
};
global.document = document;

// Editing another list leaves both keys out of the PUT, so the server keeps them.
markDirtyAutoMergeRepoPolicy.call({ id: 'am-squash-only', classList: { contains: () => false } });
assert.deepEqual(Object.keys(_configState.dirty['auto-merge']), ['squash_only']);

// Editing the no-CI list stages the whole selection.
markDirtyAutoMergeRepoPolicy.call({ id: 'am-no-ci-ok', classList: { contains: () => false } });
assert.deepEqual(_configState.dirty['auto-merge'].no_ci_ok, ['o/docs', 'o/site']);

// Deselecting every repository clears the deprecated list.
markDirtyAutoMergeRepoPolicy.call({ id: 'am-allow-unprotected-base', classList: { contains: () => false } });
assert.deepEqual(_configState.dirty['auto-merge'].allow_unprotected_base, []);
`)
}

func TestDashboardParityDelegateSaveSendsFullReposMapAndEffectTimeout(t *testing.T) {
	html := indexHTML(t)
	helpers := ""
	for _, name := range []string{
		"markDirty",
		"markDirtyDecisionsDelegateRepo",
		"decisionsDelegateSameRepo",
		"decisionsDelegateReposBody",
		"decisionsDelegateEffectTimeoutValid",
		"decisionsDelegateSaveBody",
		"renderGovDecisionsDelegateSection",
	} {
		helpers += jsFunc(t, html, name) + "\n"
	}
	runNodeScript(t, parityMarkDirtyStub+`
const esc = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const yamlHint = k => '[' + k + ']';
const settingHelpMark = () => '?';
`+helpers+`
const saved = { 'o/a': { enabled: false }, legacy: { enabled: true } };
const dd = { enabled: true, repos: saved, project_repos: ['o/a', 'o/b'], effect_timeout: '', effect_timeout_effective: '30m0s', llm: {} };

// Rows: every project repository plus the saved override outside project.repos.
const html = renderGovDecisionsDelegateSection(dd);
const rows = html.split('<tr class="dd-repo-row"').slice(1);
assert.equal(rows.length, 3, html);
const row = repo => rows.find(r => r.includes('data-repo="' + repo + '"')) || '';
assert(row('o/a').includes('<option value="off" selected>'), row('o/a'));
assert(row('o/b').includes('<option value="" selected>'), row('o/b'));
assert(row('legacy').includes('<option value="on" selected>'), row('legacy'));
assert(html.includes('[decisions.delegate.effect_timeout]'), html);
assert(html.includes('placeholder="30m0s"'), html);

// An untouched save (another field changed) sends no repos key: the server keeps the map.
assert.deepEqual(decisionsDelegateSaveBody({ enabled: false }, saved), { enabled: false });

// One row edit sends every saved override plus the edit, never the edited row alone.
markDirtyDecisionsDelegateRepo('o/b', 'on');
const body = decisionsDelegateSaveBody(_configState.dirty['decisions-delegate'], saved);
assert.deepEqual(body.repos, { 'o/a': { enabled: false }, legacy: { enabled: true }, 'o/b': { enabled: true } });

// A row set back to hive-wide drops out; a bare saved key is replaced, not duplicated.
markDirtyDecisionsDelegateRepo('o/a', '');
markDirtyDecisionsDelegateRepo('legacy', 'off');
const body2 = decisionsDelegateSaveBody(_configState.dirty['decisions-delegate'], saved);
assert.deepEqual(body2.repos, { legacy: { enabled: false }, 'o/b': { enabled: true } });

// effect_timeout is validated like the server: a duration above zero, or empty.
assert.equal(decisionsDelegateSaveBody({ effect_timeout: ' 45m ' }, saved).effect_timeout, '45m');
assert.equal(decisionsDelegateSaveBody({ effect_timeout: '' }, saved).effect_timeout, '');
assert.throws(() => decisionsDelegateSaveBody({ effect_timeout: 'soon' }, saved), /above zero/);
assert.throws(() => decisionsDelegateSaveBody({ effect_timeout: '0s' }, saved), /above zero/);
`)
}

func TestDashboardParityOperatingModeShowsNoGitHub(t *testing.T) {
	html := indexHTML(t)
	block := indexSliceBetween(t, html, "<label>Operating Mode", "})()")
	start := strings.Index(block, "(() => {")
	if start < 0 {
		t.Fatal("Operating Mode select is no longer built by an inline function")
	}
	selectFn := block[start:] + "})()"
	emojis := indexSliceBetween(t, html, "const MODE_EMOJIS", "\n")
	runNodeScript(t, `
const assert = require('node:assert/strict');
`+emojis+"\n"+jsFunc(t, html, "modeEmoji")+`
global.window = { _lastStatus: { agents: [] } };
const agentName = 'scanner';
function render(mode) { const g = { mode }; return `+selectFn+`; }

const legacy = render('NO_GITHUB');
assert(legacy.includes('<option value="NO_GITHUB" selected>'), legacy);
assert(!legacy.includes('<option value="" selected>'), legacy);
assert.equal(modeEmoji('NO_GITHUB'), modeEmoji('ADVISORY'));

// The legacy alias is not offered to an agent that does not have it.
assert(!render('ADVISORY').includes('NO_GITHUB'));
`)
}
