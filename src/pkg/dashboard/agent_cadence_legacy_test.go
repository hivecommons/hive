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
