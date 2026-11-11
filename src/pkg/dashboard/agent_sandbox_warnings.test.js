// Agent card renders the per-agent sandbox_warnings preflight findings.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const src = fs.readFileSync(path.join(__dirname, 'static-src', 'js', 'agent-cards.js'), 'utf8');
const m = src.match(/function agentSandboxWarningsHtml\(a\) \{[\s\S]*?\n    \}\n/);
assert.ok(m, 'agentSandboxWarningsHtml not found');
const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
const agentSandboxWarningsHtml = new Function('esc', m[0] + '; return agentSandboxWarningsHtml;')(esc);

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
