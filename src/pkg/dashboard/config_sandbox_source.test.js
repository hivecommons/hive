// Review Gate perspectives_source controls and agent Sandbox settings (#18797).
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const src = fs.readFileSync(path.join(__dirname, 'static-src', 'js', 'config-sandbox-source.js'), 'utf8');
const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
const yamlHint = k => `<code>${k}</code>`;
const names = ['reviewPerspectivesSourceHtml', 'agentSandboxSettingsHtml', 'markDirtySandboxJob', 'markDirtyPerspectivesSource'];

function load(extra) {
  const state = { dirty: {} };
  const markDirty = (section, key, value) => { (state.dirty[section] = state.dirty[section] || {})[key] = value; };
  const inputs = (extra && extra.inputs) || {};
  const document = { getElementById: id => (id in inputs ? { value: inputs[id] } : null) };
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
