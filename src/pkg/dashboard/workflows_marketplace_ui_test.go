package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestWorkflowsMarketplaceTabWiring pins the RFC phase-7 surface
// (hivecommons/hive#17620): the Workflows nav grows a Marketplace item,
// the governor dispatcher routes to the marketplace view, import/publish/
// refresh go through the spoke proxy routes, and library rows/detail carry
// a Publish button.
func TestWorkflowsMarketplaceTabWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`['marketplace', 'Marketplace']`,
		`'editor', 'marketplace'];`,
		`} else if (_wfLib.view === 'marketplace') {`,
		`body = renderWorkflowMarketplaceView(owner);`,
		`function renderWorkflowMarketplace(state, workflows, owner)`,
		`function renderWorkflowMarketplaceRows(entries, workflows, owner, busyKey)`,
		`function wfMktPublishButton(entry, owner)`,
		`wfMktPublishButton(e, owner)`,
		`fetch('/api/marketplace/workflows' + (force ? '?refresh=1' : '')`,
		`'/import', { method: 'POST'`,
		`fetch('/api/marketplace/workflows/publish', { method: 'POST'`,
		`fetch('/api/marketplace/refresh', { method: 'POST'`,
		`await hiveConfirm({ title: 'Publish '`,
		`yamlHint('workflow.marketplace.enabled')`,
		`.wf-mkt-stale {`,
		`.wf-prov-local-published {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing Marketplace wiring %q", want)
		}
	}
	mkt := html[strings.Index(html, "function wfMktTierBadge("):]
	mkt = mkt[:strings.Index(mkt, "function renderWorkflowMarketplaceView(")]
	for _, banned := range []string{"toLocaleString", "toLocaleDateString", "download="} {
		if strings.Contains(mkt, banned) {
			t.Errorf("marketplace JS must not use %q", banned)
		}
	}
}

// Pure helpers under node: tier badge fallback, search/tier filtering,
// installed detection, row ordering by tier, the newer-ingest flag, the
// reserved adoption/rating cells, the stale banner and the publish button
// gating.
func TestWorkflowsMarketplaceNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	html := indexHTML(t)
	script := `const assert = require('assert');
function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;'); }
function yamlHint(p) { return '<code>' + p + '</code>'; }
const WF_MKT_TIERS = ['built-in', 'curated', 'ingested-unreviewed', 'local-published'];
const WF_MKT_TIER_LABELS = { 'built-in': 'built-in', 'curated': 'curated', 'ingested-unreviewed': 'ingested · unreviewed', 'local-published': 'local-published' };
const WF_MKT_TIER_TIPS = { 'built-in': 'b', 'curated': 'c', 'ingested-unreviewed': 'i', 'local-published': 'l' };
` + jsFunc(t, html, "wfProvenanceOf") + jsFunc(t, html, "wfMktTierBadge") + jsFunc(t, html, "wfMktReservedCell") +
		jsFunc(t, html, "wfMktFilter") + jsFunc(t, html, "wfMktInstalled") + jsFunc(t, html, "renderWorkflowMarketplaceRows") +
		jsFunc(t, html, "renderWorkflowMarketplace") + jsFunc(t, html, "wfMktPublishButton") + `
assert.ok(wfMktTierBadge('curated').includes('wf-prov-curated'));
assert.ok(wfMktTierBadge('bogus').includes('wf-prov-local-published'), 'unknown tier falls back to local-published');
assert.strictEqual(wfMktReservedCell({}, 'hives'), '<span class="text-muted">—</span>');
assert.strictEqual(wfMktReservedCell(null, 'hives'), '<span class="text-muted">—</span>');
assert.strictEqual(wfMktReservedCell({ hives: 12 }, 'hives'), '12');

const entries = [
  { name: 'envoy', version: 'ingested-20260314.1', tier: 'ingested-unreviewed', description: 'Envoy release train', source: 'envoyproxy/envoy', states: 9, columns: 7, publishedAt: '2026-03-14T03:00:00Z', publishedBy: 'marketplace-refresh', sourceSHA: 'abcdef1234567890', adoption: {}, ratings: {}, benchmarks: {} },
  { name: 'techdocs', version: '2', tier: 'curated', description: 'Docs engagement', source: 'cncf/techdocs', states: 9, columns: 5, publishedAt: '2026-03-01T00:00:00Z', publishedBy: 'root', newerIngestAvailable: true, adoption: { hives: 3 }, ratings: {} },
  { name: 'hive-default', version: '1', tier: 'built-in', states: 8, columns: 7 },
  { name: 'mine', version: '1', tier: 'local-published', source: 'acme/mine', publishedBy: 'hive:acme' },
];
assert.strictEqual(wfMktFilter(entries, '', '').length, 4);
assert.deepStrictEqual(wfMktFilter(entries, 'ENVOY', '').map(e => e.name), ['envoy'], 'search is case-insensitive and matches source');
assert.deepStrictEqual(wfMktFilter(entries, '', 'curated').map(e => e.name), ['techdocs']);
assert.deepStrictEqual(wfMktFilter(entries, 'docs', 'ingested-unreviewed'), []);
assert.deepStrictEqual(wfMktFilter(null, 'x', ''), []);

const local = [{ name: 'techdocs', version: '2' }, { name: 'hive-default', version: '1', readOnly: true }];
assert.strictEqual(wfMktInstalled(entries[1], local), true);
assert.strictEqual(wfMktInstalled(entries[0], local), false);

const rows = renderWorkflowMarketplaceRows(entries, local, true, 'envoy@ingested-20260314.1');
const keys = [...rows.matchAll(/data-wf-mkt="([^"]+)"/g)].map(m => m[1]);
assert.deepStrictEqual(keys, ['hive-default@1', 'techdocs@2', 'envoy@ingested-20260314.1', 'mine@1'], 'built-in, curated, ingested, local-published');
assert.ok(rows.includes('newer ingest available'), 'curated with later ingest is flagged');
assert.ok(rows.includes('Importing…'), 'busy key disables its import button');
assert.ok(rows.includes('data-action="openWorkflowLibraryEntry" data-arg0="techdocs" data-arg1="2"'), 'installed entries open the library');
assert.ok(rows.includes('>in library<'));
assert.ok(rows.includes('data-action="wfMktImport" data-arg0="mine" data-arg1="1"'));
assert.ok(rows.includes('>3<'), 'adoption.hives renders when present');
assert.ok(rows.includes('abcdef123456</div>'), 'source SHA is abbreviated');
assert.ok(!rows.includes('toLocaleString'));
const readerRows = renderWorkflowMarketplaceRows(entries, [], false, '');
assert.ok(!readerRows.includes('wfMktImport'), 'readers get no import button');
assert.ok(renderWorkflowMarketplaceRows([], [], true, '').includes('No catalogue entries match.'));

const view = renderWorkflowMarketplace({ loaded: true, loading: false, index: { entries }, stale: true, error: 'dial tcp: refused', fetchedAt: '2026-03-14T04:00:00Z', search: 'tech', tier: '' }, local, true);
assert.ok(view.includes('wf-mkt-stale'), 'stale banner');
assert.ok(view.includes('dial tcp: refused'));
assert.ok(view.includes('1 of 4'), 'search narrows the count');
assert.ok(view.includes('data-action="wfMktRefreshCatalogue"'), 'owner sees refresh');
assert.ok(view.includes('curated (1)'), 'tier filter shows counts');
const readerView = renderWorkflowMarketplace({ loaded: true, index: { entries }, error: 'boom' }, [], false);
assert.ok(!readerView.includes('wfMktRefreshCatalogue'));
assert.ok(readerView.includes('wf-ingest-error') && readerView.includes('boom'), 'non-stale error is an alert');
assert.ok(renderWorkflowMarketplace({ loaded: false, loading: true }, [], true).includes('Loading the catalogue…'));

assert.strictEqual(wfMktPublishButton({ name: 'hive-default', version: '1', readOnly: true }, true), '', 'built-in is not publishable');
assert.strictEqual(wfMktPublishButton({ name: 'x', version: '1' }, false), '', 'readers cannot publish');
assert.strictEqual(wfMktPublishButton({ name: 'x', version: '1', compiled: false }, true), '', 'uncompilable is not publishable');
assert.ok(wfMktPublishButton({ name: 'x', version: '1', provenance: 'local' }, true).includes('data-action="wfMktPublish" data-arg0="x" data-arg1="1"'));
console.log('ok');
`
	out, err := runNodeStdin(node, script)
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("unexpected node output: %s", out)
	}
}
