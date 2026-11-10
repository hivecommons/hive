package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// The Event triggers editor offers the Default-branch CI failed preset,
// explains ci.failed in its help, defaults the target to ci-maintainer when
// that agent exists, and saves a one-repository ci.failed preset.
func TestEventTriggersCIFailedPresetUI(t *testing.T) {
	script := `
const assert = require('node:assert/strict');
const html = require('node:fs').readFileSync(0, 'utf8');
const start = html.indexOf('function renderEventTriggersSection()');
const end = html.indexOf('function markReleaseSentinelList(', start);
assert(start >= 0 && end > start);
let state = {};
let requests = [], fields = {}, response;
const panel = {innerHTML: ''};
const document = {getElementById: id => id === 'event-triggers-editor' ? panel : fields[id] || null};
const esc = s => String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const ui = new Function('_configState', 'document', 'fetch', '_inceptionAuthHeaders', 'viewerIsOwner', 'esc', 'yamlHint', 'settingHelpMark', 'configScrollBodyTo', 'openConfigDialog',
  html.slice(start, end) + ';return {loadEventTriggers,renderEventTriggersSection,newEventTrigger,eventTriggerPresetChanged,saveEventTrigger};')(
  state, document, (url, opts) => {requests.push({url, opts}); return Promise.resolve(response);},
  () => ({'Content-Type':'application/json'}), () => true, esc, () => '', () => '', () => {}, () => {});
const meta = rules => ({rules,version:'"rev-'+requests.length+'"',agents:['scanner','ci-maintainer'],repositories:[{value:'repo1',label:'org/repo1'}],apply_mode:'next_evaluation',requires_restart:false});
const ok = data => ({ok:true,status:200,json:async()=>data});
const fill = d => {fields = {}; for (const [k,v] of Object.entries(d)) fields['event-trigger-'+k] = {value:String(v)};};
(async () => {
  response = ok(meta([]));
  await ui.loadEventTriggers();
  const model = state.eventTriggers;
  assert(model.loaded);
  const markup = ui.renderEventTriggersSection();
  for (const text of ['Default-branch CI', '<code>ci.failed</code>', 'red-main sweep', 'not cancelled', 'once per repository, check and commit']) {
    assert(markup.includes(text), text);
  }
  ui.newEventTrigger();
  assert(panel.innerHTML.includes('<option value="ci.failed"'), 'preset picker must offer ci.failed');
  assert(panel.innerHTML.includes('Default-branch CI failed'));
  fill({...model.draft, preset:'ci.failed', repo:'repo1'});
  ui.eventTriggerPresetChanged();
  assert.equal(model.draft.agent, 'ci-maintainer');
  assert.equal(model.draft.expr, 'event.kind == "ci.failed" && event.repo == "repo1"');
  fill({...model.draft, name:'main-red'});
  response = ok(meta([{name:'main-red',agent:'ci-maintainer',expr:model.draft.expr,priority:0}]));
  await ui.saveEventTrigger();
  const body = JSON.parse(requests.at(-1).opts.body);
  assert.deepEqual(body.preset, {index:0, repo:'repo1', kind:'ci.failed'});
  assert.deepEqual(body.rules, [{name:'main-red', agent:'ci-maintainer', expr:'event.kind == "ci.failed" && event.repo == "repo1"', priority:0}]);
})().catch(err=>{console.error(err);process.exitCode=1;});
`
	cmd := exec.Command("node", "-e", script)
	cmd.Stdin = strings.NewReader(indexHTML(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(err.Error(), "executable file not found") {
			t.Skip("node is not installed")
		}
		t.Fatalf("Event triggers ci.failed preset regression: %v\n%s", err, out)
	}
}
