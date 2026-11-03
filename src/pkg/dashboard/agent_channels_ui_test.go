package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestAgentChannelsSaveUI(t *testing.T) {
	script := `
const assert = require('node:assert/strict');
const html = require('node:fs').readFileSync(0, 'utf8');
const start = html.indexOf('// --- Channels tab ---');
const end = html.indexOf('// --- Tools tab ---', start);
assert(start >= 0 && end > start);
const section = html.slice(start, end);
const state = {agent: 'scanner', tab: 'Channels', data: {channels: [{type: 'kick', enabled: false}], backend: 'copilot'}};
let type = 'kick', requests = [], toasts = [], renders = [];
let response;
const ui = new Function('_configState', 'document', 'fetch', '_inceptionAuthHeaders',
  'showToast', 'renderConfigTab', 'esc', 'setTimeout',
  section + '; return {addChannel, removeChannel, toggleChannel, saveChannels, renderAgentChannels, types: CHANNEL_TYPES, setState: next => {_configState = next;}};')(
  state, {getElementById: () => ({value: type})},
  (url, opts) => { requests.push({url, opts}); return Promise.resolve(response); },
  () => ({'Content-Type': 'application/json', Authorization: 'Bearer fixture'}),
  (msg, kind) => toasts.push({msg, kind}), tab => renders.push(tab), s => s, () => {});

(async () => {
  assert.deepEqual(ui.types, ['kick']);
  const markup = ui.renderAgentChannels([{type: 'kick'}, {type: 'kick'}]);
  assert(markup.includes('data-arg0="1"'));
  assert(!markup.includes('data-arg0="+i+"'));
  assert(!markup.includes('<option value="webhook">'));
  assert(markup.includes('event.kind == "issue.opened"'));
  assert(markup.includes('does not register a GitHub webhook'));
  type = 'webhook';
  ui.addChannel();
  assert.equal(requests.length, 0);
  assert(toasts.at(-1).msg.includes('Only kick'));
  type = 'kick';
  const before = JSON.stringify(state.data);
  for (const [status, error] of [[400, 'channel type has no trigger runtime'], [403, 'owner access required'], [500, 'config could not be saved']]) {
    response = {ok: false, status, json: async () => ({error})};
    await ui.addChannel();
    assert.equal(JSON.stringify(state.data), before);
    assert.equal(toasts.at(-1).msg, 'Save error: ' + error);
    assert.equal(toasts.at(-1).kind, 'error');
  }
  response = {ok: false, status: 401, json: async () => { throw Error('HTML sign-in page with private diagnostics'); }};
  await ui.toggleChannel(0, true);
  assert.equal(JSON.stringify(state.data), before);
  assert.equal(toasts.at(-1).msg, 'Save error: HTTP 401');
  await ui.removeChannel(0);
  assert.equal(JSON.stringify(state.data), before);
  response = {ok: true, status: 200, json: async () => { throw Error('private proxy response'); }};
  await ui.addChannel();
  assert.equal(JSON.stringify(state.data), before);
  assert.equal(toasts.at(-1).msg, 'Save error: Invalid server response (HTTP 200)');
  response = {ok: true, status: 200, json: async () => ({status: 'updated'})};
  const saving = ui.addChannel();
  assert.equal(JSON.stringify(state.data), before, 'must not mutate before server acceptance');
  assert(ui.renderAgentChannels(state.data.channels).includes('disabled'));
  const pendingRequests = requests.length;
  await ui.toggleChannel(0, true);
  assert.equal(requests.length, pendingRequests, 'must not send overlapping full-list writes');
  await saving;
  assert.deepEqual(state.data.channels, [{type: 'kick', enabled: false}, {type: 'kick'}]);
  assert.equal(state.data.backend, 'copilot');
  const request = requests.at(-1);
  assert.equal(request.url, '/api/config/agent/scanner/channels');
  assert.equal(request.opts.method, 'PUT');
  assert.equal(request.opts.headers.Authorization, 'Bearer fixture');
  assert.deepEqual(JSON.parse(request.opts.body), {channels: state.data.channels});
  await ui.toggleChannel(1, false);
  assert.equal(state.data.channels[1].enabled, false);
  await ui.removeChannel(1);
  assert.deepEqual(state.data.channels, [{type: 'kick', enabled: false}]);
  assert.equal(toasts.at(-1).msg, 'Channels saved');
  assert.equal(renders.at(-1), 'Channels');
  const toastCount = toasts.length;
  const pending = ui.addChannel();
  const otherState = {agent: 'reviewer', tab: 'Channels', data: {channels: []}};
  ui.setState(otherState);
  const renderCount = renders.length;
  await pending;
  assert.deepEqual(otherState.data.channels, [], 'late save must not overwrite another dialog');
  assert.equal(renders.length, renderCount);
  assert.equal(toasts.length, toastCount);
  ui.setState(state);
  response = {ok: false, status: 403, json: async () => ({error: 'owner access required'})};
  const failed = ui.removeChannel(0);
  ui.setState(otherState);
  const failedRenderCount = renders.length;
  await failed;
  assert.equal(renders.length, failedRenderCount);
  assert.equal(toasts.length, toastCount);
})().catch(err => { console.error(err); process.exitCode = 1; });
`
	cmd := exec.Command("node", "-e", script)
	cmd.Stdin = strings.NewReader(indexHTML(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(err.Error(), "executable file not found") {
			t.Skip("node is not installed")
		}
		t.Fatalf("channel save UI regression: %v\n%s", err, out)
	}
}
