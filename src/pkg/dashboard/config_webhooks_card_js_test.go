package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// Settings → Forge App → GitHub webhooks (hivecommons/hive#18029): the card
// is rendered from GET /api/config/github/webhooks alone. These tests lift the
// render functions out of index.html and execute them under node against the
// three verification modes the endpoint reports, so the copy an owner reads
// for each mode is pinned, not only pattern-matched in the source.

func webhooksCardHarness(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		"const assert = require('assert');",
		"let _webhookSetup = null; let _webhookSecretOnce = ''; let _webhookConfirmRegenerate = false;",
		"const host = { innerHTML: '' };",
		"global.document = { getElementById(id) { return id === 'webhook-setup-host' ? host : null; } };",
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "forgeAppRow"),
		jsFunc(t, html, "webhookStatusChip"),
		jsFunc(t, html, "webhookAgo"),
		jsFunc(t, html, "webhookCopyBtn"),
		jsFunc(t, html, "renderWebhookSecretBlock"),
		jsFunc(t, html, "renderWebhookCreateBlock"),
		jsFunc(t, html, "renderWebhookSetup"),
		"function render(d) { _webhookSetup = d; renderWebhookSetup(); return host.innerHTML; }",
		`const base = { payload_url: 'https://spoke.example/api/webhook/github', payload_url_source: 'config',
  path: '/api/webhook/github', content_type: 'application/json', events: ['check_run', 'pull_request', 'push'],
  optional_events: {}, org: 'o', repos: ['r'], app_org_hooks: false, app_repo_hooks: false };`,
	}, "\n")
}

func runWebhooksCardScript(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the webhooks card render was NOT executed by this run")
	}
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
}

func TestWebhooksCardRendersRelayAndSetupCopy(t *testing.T) {
	html := indexHTML(t)
	runWebhooksCardScript(t, webhooksCardHarness(t, html)+`
// relay: a hosted spoke receives hub-relayed deliveries; nothing to set up, live status shown.
const relay = render(Object.assign({}, base, { verification: 'relay (hub-signed)', hosted: true,
  secret_source: '', secret_configured: false,
  health: { events_1h: 4, last_event_at: new Date(Date.now() - 120000).toISOString(), mode: 'webhook' } }));
assert.ok(relay.includes('hub relay; the GitHub App installation delivers through the hub, nothing to set up'), relay);
assert.ok(relay.includes('Direct webhook (optional)'), relay);
assert.ok(relay.includes('receiving · 4/1h'), relay);
assert.ok(!relay.includes('Set up a webhook on GitHub'), relay);
assert.ok(!relay.includes('No events yet'), relay);

// none: the set-up steps — generate a secret, payload URL, content type, events to tick.
const none = render(Object.assign({}, base, { verification: 'none', hosted: false,
  secret_source: '', secret_configured: false, health: {} }));
assert.ok(none.includes('Set up a webhook on GitHub'), none);
assert.ok(none.includes('not configured'), none);
assert.ok(none.includes('No secret yet — generate one'), none);
assert.ok(none.includes('data-action="webhookGenerateSecret"') && none.includes('>Generate secret<'), none);
assert.ok(none.includes('https://spoke.example/api/webhook/github'), none);
assert.ok(none.includes('application/json'), none);
for (const e of base.events) assert.ok(none.includes('<code class="wh-code">' + e + '</code>'), e);
assert.ok(!none.includes('hub relay'), none);
// Without the App's hook permission there is no create button, only a hint naming the permission.
assert.ok(!none.includes('Create webhook for me'), none);
assert.ok(none.includes('<code>organization_hooks</code>') && none.includes('<code>repository_hooks</code>'), none);

// direct, env secret: no generate button, the env var is named; no events yet → test-delivery hint.
const env = render(Object.assign({}, base, { verification: 'github-signed', hosted: false,
  secret_source: 'env', secret_configured: true, health: {} }));
assert.ok(env.includes('GITHUB_WEBHOOK_SECRET'), env);
assert.ok(!env.includes('webhookGenerateSecret'), env);
assert.ok(env.includes('No events yet'), env);

// With the App's hook permission the create button replaces the hint.
const app = render(Object.assign({}, base, { verification: 'github-signed', hosted: false,
  secret_source: 'file', secret_configured: true, app_org_hooks: true, create_available: true, health: {} }));
assert.ok(app.includes('Create webhook for me'), app);
assert.ok(!app.includes('<code>organization_hooks</code>'), app);
`)
}
