// Agent card renders the per-agent sandbox_warnings preflight findings.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const src = fs.readFileSync(path.join(__dirname, 'static-src', 'js', 'agent-cards.js'), 'utf8');
const m = src.match(/function agentSandboxWarningsHtml\(a\) \{[\s\S]*?\n    \}\n/);
assert.ok(m, 'agentSandboxWarningsHtml not found');
const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
const fix = src.match(/const NO_SANDBOX_IMAGE_WARNING = [^\n]*\n    function sandboxImageFixLinksHtml\(w, a\) \{[\s\S]*?\n    \}\n/);
assert.ok(fix, 'sandboxImageFixLinksHtml not found');
const agentSandboxWarningsHtml = new Function('esc', fix[0] + m[0] + '; return agentSandboxWarningsHtml;')(esc);

test('renders one agent-error line per warning', () => {
  const html = agentSandboxWarningsHtml({ sandbox_warnings: ['operator missing', 'no <gvisor>'] });
  assert.strictEqual((html.match(/class="agent-error"/g) || []).length, 2);
  assert.ok(html.includes('⚠ operator missing'));
  assert.ok(html.includes('no &lt;gvisor>'));
});

test('renders nothing without warnings', () => {
  assert.strictEqual(agentSandboxWarningsHtml({}), '');
  assert.strictEqual(agentSandboxWarningsHtml({ sandbox_warnings: [] }), '');
  assert.strictEqual(agentSandboxWarningsHtml(null), '');
});

test('shipped index.html carries the card hook', () => {
  const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
  assert.ok(html.includes('${agentSandboxWarningsHtml(a)}'));
});

test('the no-sandbox-image warning links to the agent and the default controls (#18797)', () => {
  const html = agentSandboxWarningsHtml({ name: 'reviewer', sandbox_warnings: ['no sandbox image: neither the agent\'s sandbox.image nor agent_sandbox.image is set'] });
  assert.ok(html.includes('data-agent="reviewer"'));
  assert.ok(html.includes('data-config-type="agent" data-tab="General"'));
  assert.ok(html.includes('data-config-type="governor" data-tab="Security"'));
  assert.ok(!agentSandboxWarningsHtml({ name: 'x', sandbox_warnings: ['operator missing'] }).includes('data-action'));
});
