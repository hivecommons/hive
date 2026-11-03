package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestEventTriggersSettingsUI(t *testing.T) {
	script := `
const assert = require('node:assert/strict');
const html = require('node:fs').readFileSync(0, 'utf8');
const start = html.indexOf('function renderEventTriggersSection()');
const end = html.indexOf('function markReleaseSentinelList(', start);
assert(start >= 0 && end > start);
assert(html.includes('data-action="openEventTriggerSettings"'));
assert(html.includes("label: 'Event triggers', key: 'triggers'"));
assert(html.includes("if (tabId === 'Features') loadEventTriggers();"));
let state = {data: {unrelated: 'keep'}, dirty: {features: {keep: true}}};
let owner = false, requests = [], opens = [], fields = {}, response;
const panel = {innerHTML: ''};
const document = {getElementById: id => id === 'event-triggers-editor' ? panel : fields[id] || null};
const esc = s => String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const ui = new Function('_configState', 'document', 'fetch', '_inceptionAuthHeaders', 'viewerIsOwner', 'esc', 'yamlHint', 'settingHelpMark', 'configScrollBodyTo', 'openConfigDialog',
  html.slice(start, end) + ';return {loadEventTriggers,renderEventTriggersSection,newEventTrigger,editEventTrigger,eventTriggerPresetChanged,saveEventTrigger,cancelEventTriggerEdit,removeEventTrigger,openEventTriggerSettings,setState: next => {_configState = next;}};')(
  state, document, (url, opts) => {requests.push({url, opts}); return response instanceof Promise ? response : Promise.resolve(response);},
  () => ({'Content-Type':'application/json', Authorization:'******'}), () => owner, esc, () => '', () => '', () => {},
  (...args) => {opens.push(args); state = {}; ui.setState(state);});
const legacy = {name:'<legacy>',agent:'scanner',expr:'event.labels.exists(x, x.startsWith("team/"))',priority:-17};
const meta = rules => ({rules,version:'"revision-'+requests.length+'"',agents:['scanner','reviewer'],repositories:[{value:'repo1',label:'org/repo1'}],apply_mode:'next_evaluation',requires_restart:false});
const ok = data => ({ok:true,status:200,json:async()=>data});
const fail = (status,error) => ({ok:false,status,json:async()=>({error})});
const fill = d => {fields = {}; for(const [k,v] of Object.entries(d)) fields['event-trigger-'+k] = {value:String(v)};};
(async () => {
  await ui.loadEventTriggers();
  assert.equal(requests.length,0);
  assert(ui.renderEventTriggersSection().includes('Owner role required'));
  owner = true;
  response = fail(403,'owner access required');
  await ui.loadEventTriggers();
  assert(state.eventTriggers.error.includes('owner access required'));
  response = ok(meta([legacy]));
  await ui.loadEventTriggers(true);
  const model = state.eventTriggers;
  assert(model.loaded);
  const markup = ui.renderEventTriggersSection();
  assert(markup.includes('settings-subsection config-field'), 'search must resolve the Event triggers settings row');
  for(const text of ['Event triggers','ACTIONABLE','once per hive process','Existing actionable issues','restart replays','does not reset','Pause, budget, on-demand','separate setup','without restarting']) {
    assert(markup.includes(text), text);
  }
  assert(markup.includes('&lt;legacy&gt;'));
  assert(!markup.includes('<legacy>'));
  ui.newEventTrigger();
  assert.equal(model.draft.repo,'','no broad repository default');
  assert.equal(model.draft.agent,'scanner');
  fill(model.draft);
  const initialRequests = requests.length;
  await ui.saveEventTrigger();
  assert.equal(requests.length,initialRequests);
  assert(model.error.includes('one managed repository'));
  fill({...model.draft,repo:'repo1'});
  ui.eventTriggerPresetChanged();
  assert.equal(model.draft.expr,'event.kind == "issue.opened" && event.repo == "repo1"');
  fill({...model.draft,name:'first-seen',priority:'29'});
  let release;
  response = new Promise(resolve => {release=resolve;});
  const save = ui.saveEventTrigger();
  const sent = requests.at(-1);
  assert.equal(sent.url,'/api/config/triggers');
  assert.equal(sent.opts.method,'PUT');
  assert.equal(sent.opts.headers.Authorization,'******');
  assert.equal(sent.opts.headers['If-Match'],model.version);
  const body = JSON.parse(sent.opts.body);
  assert.deepEqual(body.rules[0],legacy);
  assert.equal(body.rules[1].priority,29);
  assert.deepEqual(body.preset,{index:1,repo:'repo1',kind:'issue.opened'});
  const count = requests.length;
  await ui.saveEventTrigger();
  await ui.loadEventTriggers(true);
  await ui.removeEventTrigger(0);
  assert.equal(requests.length,count,'serialize writes and do not reload while saving');
  assert.equal(model.rules.length,1,'no optimistic rule mutation');
  release(ok(meta(body.rules)));
  await save;
  assert.equal(model.rules.length,2);
  assert.equal(model.draft,null);
  assert(model.notice.includes('next event evaluation'));
  assert(model.notice.includes('no restart'));
  assert.deepEqual(state.dirty,{features:{keep:true}});
  assert.deepEqual(state.data,{unrelated:'keep'});
  ui.editEventTrigger(0);
  assert.equal(model.draft.preset,'advanced','never reverse-parse or rewrite existing CEL');
  assert.equal(model.draft.expr,legacy.expr);
  assert.equal(model.draft.priority,'-17');
  const original = JSON.stringify(model.rules), version=model.version;
  for(const [status,error] of [[400,'invalid CEL trigger: must return bool'],[403,'owner access required'],[412,'event triggers changed; reload before saving'],[500,'could not persist Event triggers; runtime rules unchanged']]) {
    fill(model.draft);
    response=fail(status,error);
    await ui.saveEventTrigger();
    assert.equal(model.error,error);
    assert(model.draft,'failed save must retain edit');
    assert.equal(JSON.stringify(model.rules),original);
    assert.equal(model.version,version);
  }
  response={ok:true,status:200,json:async()=>{throw Error('private proxy HTML');}};
  await ui.saveEventTrigger();
  assert.equal(model.error,'Invalid server response (HTTP 200)');
  response=ok({status:'updated'});
  await ui.saveEventTrigger();
  assert.equal(model.error,'Invalid Event triggers response');
  response=ok(meta([{...legacy,expr:'event.number > 0'},model.rules[1]]));
  fill({...model.draft,expr:'event.number > 0'});
  await ui.saveEventTrigger();
  assert(!('preset' in JSON.parse(requests.at(-1).opts.body)));
  ui.editEventTrigger(0);
  ui.cancelEventTriggerEdit();
  response=ok(meta([model.rules[1]]));
  await ui.removeEventTrigger(0);
  assert.equal(model.rules.length,1);
  response=new Promise(resolve=>{release=resolve;});
  const load=ui.loadEventTriggers(true);
  const other={data:{other:true}};
  ui.setState(other);
  const before=panel.innerHTML;
  release(ok(meta([])));
  await load;
  assert.equal(other.eventTriggers,undefined,'late load must not populate another dialog');
  assert.equal(panel.innerHTML,before);
  state={};
  ui.setState(state);
  response=ok(meta([legacy]));
  await ui.loadEventTriggers();
  ui.editEventTrigger(0);
  fill(state.eventTriggers.draft);
  response=new Promise(resolve=>{release=resolve;});
  const lateSave=ui.saveEventTrigger();
  ui.setState(other);
  const savedBefore=panel.innerHTML;
  release(ok(meta([legacy])));
  await lateSave;
  assert.equal(panel.innerHTML,savedBefore,'late save must not paint another dialog');
  ui.openEventTriggerSettings();
  assert.deepEqual(opens.at(-1),['governor',null,'Features']);
  assert.equal(state.focusEventTriggers,true);
})().catch(err=>{console.error(err);process.exitCode=1;});
`
	cmd := exec.Command("node", "-e", script)
	cmd.Stdin = strings.NewReader(indexHTML(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(err.Error(), "executable file not found") {
			t.Skip("node is not installed")
		}
		t.Fatalf("Event triggers settings regression: %v\n%s", err, out)
	}
}
