package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestCadenceSetKindConvertsLegacyContinuousToPerMode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — cadence legacy conversion helper was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const SECONDS_PER_HOUR = 3600;",
		"const dirty = [];",
		"let renderedTab = null;",
		"const _configState = { tab: 'Cadences', data: { general: { continuous: true }, cadences: { surge: 3600, busy: 1800, quiet: 7200, idle: 900 } } };",
		"function markDirty(section, key, value) { dirty.push({ section, key, value }); }",
		"function renderConfigTab(tab) { renderedTab = tab; }",
		jsFunc(t, html, "cadenceModeValue"),
		jsFunc(t, html, "cadenceSetKind"),
	}, "\n") + `
function assertEqual(name, got, want) {
  const g = JSON.stringify(got);
  const w = JSON.stringify(want);
  if (g !== w) throw new Error(name + ': got ' + g + ', want ' + w);
}
cadenceSetKind('surge', 'interval');
assertEqual('general continuous converted off', _configState.data.general.continuous, false);
assertEqual('surge keeps stored interval', _configState.data.cadences.surge, 3600);
assertEqual('busy preserved as explicit continuous', _configState.data.cadences.busy, 'continuous');
assertEqual('idle preserved as explicit continuous', _configState.data.cadences.idle, 'continuous');
assertEqual('quiet untouched', _configState.data.cadences.quiet, 7200);
assertEqual('rerendered tab', renderedTab, 'Cadences');
assertEqual('dirty writes', dirty, [
  {section:'cadences', key:'busy', value:'continuous'},
  {section:'cadences', key:'idle', value:'continuous'},
  {section:'general', key:'continuous', value:false},
  {section:'cadences', key:'surge', value:3600},
]);

dirty.length = 0;
renderedTab = null;
_configState.data = { general: { continuous: true }, cadences: { idle: 900, quiet: 7200 } };
cadenceSetKind('idle', 'off');
assertEqual('idle edit turns legacy off', _configState.data.general.continuous, false);
assertEqual('surge idle fallback preserved continuous', _configState.data.cadences.surge, 'continuous');
assertEqual('busy idle fallback preserved continuous', _configState.data.cadences.busy, 'continuous');
assertEqual('idle requested off applied', _configState.data.cadences.idle, 0);
assertEqual('quiet still untouched after idle edit', _configState.data.cadences.quiet, 7200);
assertEqual('idle fallback dirty writes', dirty, [
  {section:'cadences', key:'surge', value:'continuous'},
  {section:'cadences', key:'busy', value:'continuous'},
  {section:'general', key:'continuous', value:false},
  {section:'cadences', key:'idle', value:0},
]);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node cadence legacy conversion test failed: %v\n%s", err, out)
	}
}

func TestCadenceButtonsPassModeAndKindThroughDispatcher(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — cadence button dispatcher test was NOT executed by this run")
	}
	html := indexHTML(t)
	for _, snippet := range []string{
		`data-arg1="${mode}" data-arg2="${kind}" data-arg-types="e,s,s"`,
		`data-action="switchToPerModeCadences"`,
		`Switch to per-mode cadences`,
		`Legacy continuous is on.`,
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("index.html is missing %q — cadence legacy controls are not explicitly actionable", snippet)
		}
	}
	if strings.Contains(html, `data-action="cadenceSetKind" data-keydown-action="cadenceKindKey" data-keys="Enter, ,ArrowLeft,ArrowRight,ArrowUp,ArrowDown" data-prevent="1" data-arg0="${mode}" data-arg1="${kind}" data-arg-types="e,s,s"`) {
		t.Fatal("cadence mode buttons still put mode/kind in arg0/arg1 while reserving arg0 for the event")
	}
	script := strings.Join([]string{
		"const SECONDS_PER_HOUR = 3600;",
		"const dirty = [];",
		"let renderedTab = null;",
		"const _configState = { tab: 'Cadences', data: { general: { continuous: true }, cadences: { surge: 'continuous', busy: 'continuous', quiet: 7200, idle: 'continuous' } } };",
		"function markDirty(section, key, value) { dirty.push({ section, key, value }); }",
		"function renderConfigTab(tab) { renderedTab = tab; }",
		jsFunc(t, html, "cadenceModeValue"),
		jsFunc(t, html, "hiveResolveActionArgs"),
		jsFunc(t, html, "cadenceSetKind"),
		jsFunc(t, html, "switchToPerModeCadences"),
	}, "\n") + `
function assertEqual(name, got, want) {
  const g = JSON.stringify(got);
  const w = JSON.stringify(want);
  if (g !== w) throw new Error(name + ': got ' + g + ', want ' + w);
}
const clickEvent = { type: 'click' };
const offButton = { dataset: { argTypes: 'e,s,s', arg1: 'surge', arg2: 'off' } };
const args = hiveResolveActionArgs(offButton, clickEvent);
assertEqual('dispatcher args', args, [clickEvent, 'surge', 'off']);
cadenceSetKind.apply(offButton, args);
assertEqual('surge changed through click args', _configState.data.cadences.surge, 0);
assertEqual('legacy flag disabled through click args', _configState.data.general.continuous, false);
assertEqual('rerendered tab after click', renderedTab, 'Cadences');

dirty.length = 0;
renderedTab = null;
_configState.data = { general: { continuous: true }, cadences: { surge: 3600, busy: 1800, quiet: 7200, idle: 900 } };
switchToPerModeCadences();
assertEqual('switch action disables legacy flag', _configState.data.general.continuous, false);
assertEqual('switch action preserves active rows as continuous', _configState.data.cadences, { surge: 'continuous', busy: 'continuous', quiet: 7200, idle: 'continuous' });
assertEqual('switch action dirty writes', dirty, [
  {section:'cadences', key:'surge', value:'continuous'},
  {section:'cadences', key:'busy', value:'continuous'},
  {section:'cadences', key:'idle', value:'continuous'},
  {section:'general', key:'continuous', value:false},
]);
assertEqual('rerendered tab after switch action', renderedTab, 'Cadences');

dirty.length = 0;
renderedTab = null;
_configState.data = { general: { continuous: false }, cadences: { surge: 3600 } };
const intervalButton = { dataset: { argTypes: 'e,s,s', arg1: 'surge', arg2: 'continuous' } };
cadenceSetKind.apply(intervalButton, hiveResolveActionArgs(intervalButton, clickEvent));
assertEqual('non-continuous agent button changes row', _configState.data.cadences.surge, 'continuous');
assertEqual('non-continuous agent does not dirty legacy flag', dirty, [
  {section:'cadences', key:'surge', value:'continuous'},
]);
assertEqual('non-continuous agent rerendered tab', renderedTab, 'Cadences');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node cadence dispatcher test failed: %v\n%s", err, out)
	}
}

func TestCadenceLegacyToggleAndHintsShareConfigFlag(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — cadence render source-of-truth test was NOT executed by this run")
	}
	html := indexHTML(t)
	for _, snippet := range []string{
		`const _dirtyGeneral = (_configState.dirty && _configState.dirty.general) || {};`,
		`: _general.enabled !== false;`,
		`: _general.continuous === true;`,
		`renderCadenceBuilder('quiet', c.quiet, _continuous)`,
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("index.html is missing %q — cadence render state is not tied to config source of truth", snippet)
		}
	}
	script := strings.Join([]string{
		"const SECONDS_PER_MIN = 60, SECONDS_PER_HOUR = 3600, SECONDS_PER_DAY = 86400;",
		"const window = { _lastAgents: [{ name: 'scanner', enabled: false }] };",
		"function esc(v) { return String(v == null ? '' : v).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/\"/g, '&quot;'); }",
		"function browserTimeZone() { return 'UTC'; }",
		"let _configState = { agent: 'scanner', dirty: {}, data: { general: { enabled: true, continuous: false }, cadences: {} } };",
		jsFunc(t, html, "pwrSwitchHtml"),
		jsFunc(t, html, "cadenceToHuman"),
		jsFunc(t, html, "cadenceModeValue"),
		jsFunc(t, html, "cadenceDetailKind"),
		jsFunc(t, html, "cadenceSummary"),
		jsFunc(t, html, "cadenceKindButton"),
		jsFunc(t, html, "renderCadenceBuilder"),
		jsFunc(t, html, "renderAgentCadences"),
	}, "\n") + `
function assertIncludes(name, text, needle) {
  if (!text.includes(needle)) throw new Error(name + ': missing ' + needle);
}
function assertNotIncludes(name, text, needle) {
  if (text.includes(needle)) throw new Error(name + ': unexpectedly found ' + needle);
}
let out = renderAgentCadences({ surge: 3600, busy: 1800, quiet: 7200, idle: 900 });
assertIncludes('enabled uses config not stale live agent', out, 'aria-label="Agent enabled"');
assertIncludes('enabled switch uses config on', out, 'class="pwr-switch on agent-power-switch"');
assertIncludes('enabled switch aria on', out, 'aria-label="Agent enabled"');
assertIncludes('continuous switch off', out, 'id="cfg-agent-continuous" data-action="toggleContinuousAllActiveModes" data-arg-types="t" role="switch" aria-checked="false"');
assertNotIncludes('hint hidden when flag off', out, 'Legacy continuous is on.');
assertNotIncludes('row legacy hint hidden when flag off', out, 'Legacy all-modes continuous is on;');

_configState = { agent: 'scanner', dirty: {}, data: { general: { enabled: true, continuous: true }, cadences: {} } };
out = renderAgentCadences({ surge: 3600, busy: 1800, quiet: 7200, idle: 900 });
assertIncludes('continuous switch on', out, 'id="cfg-agent-continuous" data-action="toggleContinuousAllActiveModes" data-arg-types="t" role="switch" aria-checked="true"');
assertIncludes('legacy notice shown when flag on', out, 'Legacy continuous is on.');
assertIncludes('active row legacy hint shown when flag on', out, 'Legacy all-modes continuous is on;');
assertIncludes('quiet stays interval under legacy flag', out, 'cad-quiet-kind" value="interval"');

_configState.dirty.general = { continuous: false };
out = renderAgentCadences({ surge: 3600, busy: 1800, quiet: 7200, idle: 900 });
assertIncludes('dirty continuous false controls toggle', out, 'id="cfg-agent-continuous" data-action="toggleContinuousAllActiveModes" data-arg-types="t" role="switch" aria-checked="false"');
assertNotIncludes('dirty continuous false hides hint', out, 'Legacy continuous is on.');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node cadence render source-of-truth test failed: %v\n%s", err, out)
	}
}
