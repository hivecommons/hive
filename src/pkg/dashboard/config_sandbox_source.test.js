// Review Gate perspectives_source controls and agent Sandbox settings (#18797).
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const src = fs.readFileSync(path.join(__dirname, 'static-src', 'js', 'config-sandbox-source.js'), 'utf8');
const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
const yamlHint = k => `<code>${k}</code>`;
const names = ['reviewPerspectivesSourceHtml', 'agentSandboxSettingsHtml', 'markDirtySandboxJob', 'markDirtyPerspectivesSource',
  'sandboxSchedulingHtml', 'markSandboxScheduling', 'sandboxParseMapLines', 'sandboxParseTolerationLines', 'sandboxTolerationText'];

function load(extra) {
  const state = { dirty: {} };
  const markDirty = (section, key, value) => { (state.dirty[section] = state.dirty[section] || {})[key] = value; };
  const inputs = (extra && extra.inputs) || {};
  const document = { getElementById: id => (id in inputs ? (typeof inputs[id] === 'object' ? inputs[id] : { value: inputs[id] }) : null) };
  const body = src + '\nreturn {' + names.join(',') + '};';
  const api = new Function('esc', 'yamlHint', 'markDirty', '_configState', 'document', body)(esc, yamlHint, markDirty, { dirty: state.dirty }, document);
  return { api, state, markDirty };
}

test('unlinked Review Gate shows four fields and no Unlink button', () => {
  const { api } = load();
  const html = api.reviewPerspectivesSourceHtml({}, true);
  for (const f of ['owner', 'repo', 'path', 'ref']) assert.ok(html.includes(`id="rv-psrc-${f}"`), f);
  assert.ok(!html.includes('unlinkReviewPerspectivesSource'));
});

test('linked Review Gate shows resolver status, Unlink, and the Prompt sources link when unapproved', () => {
  const { api } = load();
  const linked = { perspectives_linked: true, perspectives_source: { owner: 'acme', repo: 'rules', path: 'p.yaml' }, perspectives_source_status: 'github:acme/rules@main', perspectives_source_approved: true };
  let html = api.reviewPerspectivesSourceHtml(linked, true);
  assert.ok(html.includes('data-action="unlinkReviewPerspectivesSource"'));
  assert.ok(html.includes('github:acme/rules@main'));
  assert.ok(!html.includes('not approved'));
  html = api.reviewPerspectivesSourceHtml(Object.assign({}, linked, { perspectives_source_approved: false, perspectives_source_status: 'unresolved' }), true);
  assert.ok(html.includes('not approved as a prompt source'));
  assert.ok(html.includes('Settings, Security, Prompt sources'));
  assert.ok(html.includes('data-tab="Security"'));
});

test('non-owners get disabled fields and no Unlink', () => {
  const { api } = load();
  const html = api.reviewPerspectivesSourceHtml({ perspectives_linked: true, perspectives_source: { owner: 'a', repo: 'b', path: 'c' } }, false);
  assert.ok(html.includes(' disabled'));
  assert.ok(!html.includes('unlinkReviewPerspectivesSource'));
});

test('editing the fields stages an object, blanking all stages null', () => {
  let t = load({ inputs: { 'rv-psrc-owner': 'acme', 'rv-psrc-repo': 'rules', 'rv-psrc-path': 'p.yaml', 'rv-psrc-ref': '' } });
  t.api.markDirtyPerspectivesSource();
  assert.deepStrictEqual(t.state.dirty.review.perspectives_source, { owner: 'acme', repo: 'rules', path: 'p.yaml', ref: '' });
  t = load({ inputs: { 'rv-psrc-owner': '', 'rv-psrc-repo': '', 'rv-psrc-path': '', 'rv-psrc-ref': '' } });
  t.api.markDirtyPerspectivesSource();
  assert.strictEqual(t.state.dirty.review.perspectives_source, null);
});

test('agent Sandbox settings: fields, warning with links, copy action', () => {
  const { api } = load();
  const g = {
    sandboxDefaultsProblem: "no sandbox image: neither the agent's sandbox.image nor agent_sandbox.image is set",
    sandboxCopySources: ['deps'],
    sandboxFields: { sandboxImage: '', sandboxRuntime: '', sandboxRuntimeEffective: 'podman', sandboxImageEffective: '', sandboxJob: { envFromSecrets: ['gh-token'] } },
  };
  const html = api.agentSandboxSettingsHtml(g);
  assert.ok(html.includes('id="cfg-agent-sandbox-image"'));
  assert.ok(html.includes('id="cfg-agent-sandbox-runtime"'));
  assert.ok(html.includes('id="cfg-agent-sandbox-job-workspaceClaim"'));
  assert.ok(html.includes('Copy sandbox settings from...'));
  assert.ok(html.includes('data-action="copySandboxSettingsFrom"'));
  assert.ok(html.includes('<option value="deps">deps</option>'));
  assert.ok(html.includes('no sandbox image'));
  assert.ok(html.includes('data-config-type="governor" data-tab="Security"'));
  assert.ok(html.includes('gh-token'));
  // No copy picker when no agent has a complete block.
  assert.ok(!api.agentSandboxSettingsHtml({ sandboxCopySources: [], sandboxFields: {} }).includes('copySandboxSettingsFrom'));
});

test('Job edits stage as one sandboxJob object with typed values', () => {
  const t = load();
  t.api.markDirtySandboxJob('ttlSeconds', '600');
  t.api.markDirtySandboxJob('envFromSecrets', 'a\n b \n');
  t.api.markDirtySandboxJob('serviceAccount', 'sbx');
  assert.deepStrictEqual(t.state.dirty.general.sandboxJob, { ttlSeconds: 600, envFromSecrets: ['a', 'b'], serviceAccount: 'sbx' });
});

test('shipped index.html carries the controls', () => {
  const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
  assert.ok(html.includes('${reviewPerspectivesSourceHtml(rv, rvCanEdit)}') || html.includes('reviewPerspectivesSourceHtml(rv, rvCanEdit)'));
  assert.ok(html.includes('${agentSandboxSettingsHtml(g)}'));
});

// ── Job scheduling, resources, preflight and read relay (#19553) ──

test('map lines parse labels and quantities and report bad lines', () => {
  const { api } = load();
  assert.deepStrictEqual(api.sandboxParseMapLines('accelerator=spyre\n\ntopology.kubernetes.io/zone = z1', 'label'),
    { value: { accelerator: 'spyre', 'topology.kubernetes.io/zone': 'z1' }, errors: [] });
  assert.deepStrictEqual(api.sandboxParseMapLines('ibm.com/spyre_pf=2\nmemory=32Gi\ncpu=500m', 'quantity').value,
    { 'ibm.com/spyre_pf': '2', memory: '32Gi', cpu: '500m' });
  assert.strictEqual(api.sandboxParseMapLines('nokey', 'label').errors.length, 1);
  assert.strictEqual(api.sandboxParseMapLines('bad key=x', 'label').errors.length, 1);
  assert.strictEqual(api.sandboxParseMapLines('cpu=lots', 'quantity').errors.length, 1);
  assert.strictEqual(api.sandboxParseMapLines('cpu=-1', 'quantity').errors.length, 1);
});

test('toleration lines round-trip through taint syntax', () => {
  const { api } = load();
  const parsed = api.sandboxParseTolerationLines('accelerator:NoSchedule\nteam=ml:NoExecute\n*\nlone');
  assert.deepStrictEqual(parsed.errors, []);
  assert.deepStrictEqual(parsed.value, [
    { key: 'accelerator', operator: 'Exists', value: '', effect: 'NoSchedule' },
    { key: 'team', operator: 'Equal', value: 'ml', effect: 'NoExecute' },
    { key: '', operator: 'Exists', value: '', effect: '' },
    { key: 'lone', operator: 'Exists', value: '', effect: '' },
  ]);
  assert.strictEqual(api.sandboxTolerationText(parsed.value), 'accelerator:NoSchedule\nteam=ml:NoExecute\n*\nlone');
  assert.strictEqual(api.sandboxParseTolerationLines('a:Sometimes').errors.length, 1);
});

test('agent scheduling block shows the fields, full YAML paths and effective values', () => {
  const { api } = load();
  const f = {
    sandboxJob: { nodeSelector: {}, resources: { limits: { 'ibm.com/spyre_pf': '1' } }, tolerations: [], imagePullSecrets: [] },
    sandboxPreflight: null,
    sandboxPreflightDefaults: { timeoutS: 120, totalTimeoutS: 300, scheduleTimeoutS: 600, maxCommands: 32 },
    sandboxEffective: {
      job: { nodeSelector: { accelerator: 'spyre' }, resources: {}, tolerations: [], imagePullSecrets: ['reg'], serviceAccount: '', ttlSeconds: 0, scheduleTimeoutS: 600 },
      sources: { nodeSelector: 'agent', resources: 'agent', imagePullSecrets: 'agent_sandbox' },
      hiddenDefaults: ['node_selector'],
      preflight: { commands: ['true'], timeoutS: 120, totalTimeoutS: 300 },
      preflightSource: 'agent_sandbox',
    },
  };
  const html = api.sandboxSchedulingHtml('agent', f);
  for (const id of ['nodeSelector', 'limits', 'requests', 'tolerations', 'imagePullSecrets', 'scheduleTimeoutS', 'preflightOverride', 'preflightCommands']) {
    assert.ok(html.includes(`id="sbx-agent-${id}"`), id);
  }
  assert.ok(html.includes('agents.<key>.sandbox.job.node_selector.<key>'));
  assert.ok(html.includes('agents.<key>.sandbox.preflight.commands[]'));
  assert.ok(html.includes('ibm.com/spyre_pf=1'));
  assert.ok(html.includes('from agent_sandbox defaults'));
  assert.ok(html.includes('cfg-agent-sandbox-hidden-defaults'));
  assert.ok(html.includes('agent_sandbox.job.node_selector'));
  // Without an override the preflight inputs show the hive-wide list, locked.
  assert.match(html, /id="sbx-agent-preflightCommands"[^>]*disabled/);
  assert.ok(!html.includes('github_read_proxy'), 'the read relay is hive-wide only');
  assert.ok(api.agentSandboxSettingsHtml({ sandboxFields: f }).includes('id="sbx-agent-nodeSelector"'));
});

test('hive scheduling block carries the read relay controls', () => {
  const { api } = load();
  const html = api.sandboxSchedulingHtml('hive', {
    agentSandboxJobNodeSelector: { accelerator: 'spyre' },
    agentSandboxJobGithubReadProxy: { enabled: true, listenAddr: ':8443', endpoint: 'https://relay:8443', noProxy: [] },
  });
  for (const id of ['rpEnabled', 'rpListenAddr', 'rpEndpoint', 'rpNoProxy', 'nodeSelector', 'preflightCommands']) assert.ok(html.includes(`id="sbx-hive-${id}"`), id);
  assert.ok(html.includes('agent_sandbox.job.github_read_proxy.listen_addr'));
  assert.ok(html.includes('agent_sandbox.job.resources.limits.<key>'));
  assert.ok(!html.includes('preflightOverride'));
});

test('agent scheduling edits stage typed sandboxJob and sandboxPreflight values', () => {
  const t = load({ inputs: {
    'sbx-agent-nodeSelector': 'accelerator=spyre',
    'sbx-agent-limits': 'ibm.com/spyre_pf=2',
    'sbx-agent-requests': '',
    'sbx-agent-tolerations': 'accelerator:NoSchedule',
    'sbx-agent-imagePullSecrets': 'reg',
    'sbx-agent-scheduleTimeoutS': '900',
    'sbx-agent-preflightOverride': { type: 'checkbox', checked: true },
    'sbx-agent-preflightCommands': 'nvidia-smi\n',
    'sbx-agent-preflightTimeoutS': '60',
    'sbx-agent-preflightTotalTimeoutS': '',
    'sbx-agent-preflightRetryDelayS': '',
  } });
  t.api.markSandboxScheduling('agent');
  assert.deepStrictEqual(t.state.dirty.general.sandboxJob, {
    nodeSelector: { accelerator: 'spyre' },
    resources: { limits: { 'ibm.com/spyre_pf': '2' }, requests: {} },
    tolerations: [{ key: 'accelerator', operator: 'Exists', value: '', effect: 'NoSchedule' }],
    imagePullSecrets: ['reg'],
    scheduleTimeoutS: 900,
  });
  assert.deepStrictEqual(t.state.dirty.general.sandboxPreflight, { commands: ['nvidia-smi'], timeoutS: 60, totalTimeoutS: 0, retryDelayS: 0 });
});

test('a field with an error is left out of the save; no override stages null', () => {
  const t = load({ inputs: {
    'sbx-agent-nodeSelector': 'bad key=x',
    'sbx-agent-limits': 'cpu=2',
    'sbx-agent-preflightOverride': { type: 'checkbox', checked: false },
    'sbx-agent-preflightCommands': 'true',
  } });
  t.api.markSandboxScheduling('agent');
  assert.ok(!('nodeSelector' in t.state.dirty.general.sandboxJob));
  assert.deepStrictEqual(t.state.dirty.general.sandboxJob.resources, { limits: { cpu: '2' } });
  assert.strictEqual(t.state.dirty.general.sandboxPreflight, null);
});

test('hive scheduling edits stage agentSandbox keys and refuse an incomplete relay', () => {
  let t = load({ inputs: {
    'sbx-hive-nodeSelector': 'accelerator=spyre',
    'sbx-hive-preflightCommands': '',
    'sbx-hive-rpEnabled': { type: 'checkbox', checked: true },
    'sbx-hive-rpListenAddr': ':8443',
    'sbx-hive-rpEndpoint': 'https://relay:8443',
    'sbx-hive-rpNoProxy': 'registry.internal',
  } });
  t.api.markSandboxScheduling('hive');
  assert.deepStrictEqual(t.state.dirty.security.agentSandboxJobNodeSelector, { accelerator: 'spyre' });
  assert.deepStrictEqual(t.state.dirty.security.agentSandboxPreflight, { commands: [] });
  assert.deepStrictEqual(t.state.dirty.security.agentSandboxJobGithubReadProxy, { enabled: true, listenAddr: ':8443', endpoint: 'https://relay:8443', noProxy: ['registry.internal'] });
  t = load({ inputs: {
    'sbx-hive-rpEnabled': { type: 'checkbox', checked: true },
    'sbx-hive-rpListenAddr': '',
    'sbx-hive-rpEndpoint': 'http://relay:8443',
    'sbx-hive-rpNoProxy': '',
  } });
  t.api.markSandboxScheduling('hive');
  assert.ok(!t.state.dirty.security || !('agentSandboxJobGithubReadProxy' in t.state.dirty.security));
});

test('shipped index.html carries the scheduling controls in both places', () => {
  const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
  assert.ok(html.includes("${sandboxSchedulingHtml('agent', f)}"));
  assert.ok(html.includes("${sandboxSchedulingHtml('hive', sec)}"));
});
