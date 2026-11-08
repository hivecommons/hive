package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// knowledgeAutoMemoryUIScript drives the knowledge view's auto-memory review
// (#17058) against the generated index.html: the filter lists drained drafts
// only, rows show the agent: tags, an owner gets approve/discard backed by
// PUT /api/knowledge/entry/{id}/state, and a non-owner gets no buttons.
func knowledgeAutoMemoryUIScript(t *testing.T) string {
	t.Helper()
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	section := func(start, end string) string {
		t.Helper()
		i := strings.Index(html, start)
		if i < 0 {
			t.Fatalf("missing %s", start)
		}
		j := strings.Index(html[i:], end)
		if j < 0 {
			t.Fatalf("missing %s", end)
		}
		return html[i : i+j]
	}
	return `
const assert = require('node:assert/strict');
let _kbFacts = [], _kbPageSize = 50, _kbPageOffset = 0, _kbServerPaged = false,
    _kbHasMore = false, _kbRequest = 0, _kbSelectedFact = null,
    _kbSearchQuery = '', _kbSelectedLayer = 'auto-memory', _kbSelectedType = '';
const el = {innerHTML: ''};
const document = {getElementById: () => el};
const kbIsDividerOnlyFact = () => false;
const escapeHtml = s => String(s);
const kbCleanFactTitle = s => s;
const kbCleanFactPreview = s => s;
const kbConfidenceBadge = () => '';
const confColor = () => 'green';
let owner = true, confirmed = true, alerts = [], statsFetched = 0;
const viewerIsOwner = () => owner;
const hiveConfirm = async () => confirmed;
const hiveAlert = async msg => { alerts.push(msg); };
const _inceptionAuthHeaders = () => ({'Content-Type': 'application/json'});
const fetchKnowledgeStats = () => { statsFetched++; };
` + section("    function kbSearch() {", "    async function kbReadFact(") +
		section("    function renderKnowledgeFacts() {", "    function renderKnowledgeDetail()") + `
(async () => {
 const drained = {slug: 'cli-memory-aaaa', title: 'Use go test -run', state: 'draft',
   tags: ['cli-memory', 'cli:claude', 'memhash:aaaa', 'agent:scanner', 'agent:developer']};
 const approved = {slug: 'cli-memory-bbbb', title: 'Approved memory', state: 'approved', tags: ['cli-memory', 'agent:scanner']};
 const calls = [];
 global.fetch = async (url, opts) => {
  calls.push({url, opts});
  if (opts && opts.method === 'PUT') return {ok: true, json: async () => ({ok: true})};
  return {ok: true, json: async () => ({results: [drained, approved]})};
 };
 await kbSearch();
 assert.match(calls[0].url, /^\/api\/knowledge\/search\?tag=cli-memory&include_states=draft/);
 assert.deepEqual(_kbFacts.map(f => f.slug), ['cli-memory-aaaa'], 'only drafts are listed for review');
 assert.ok(el.innerHTML.includes('agent scanner') && el.innerHTML.includes('agent developer'), el.innerHTML);
 assert.match(el.innerHTML, /data-action="kbAutoMemoryReview" data-arg0="cli-memory-aaaa" data-arg1="approved"/);
 assert.match(el.innerHTML, /data-action="kbAutoMemoryReview" data-arg0="cli-memory-aaaa" data-arg1="deprecated"/);

 await kbAutoMemoryReview('cli-memory-aaaa', 'approved');
 const put = calls.find(c => c.opts && c.opts.method === 'PUT');
 assert.equal(put.url, '/api/knowledge/entry/cli-memory-aaaa/state');
 assert.equal(JSON.parse(put.opts.body).state, 'approved');
 assert.equal(statsFetched, 1);
 assert.deepEqual(alerts, []);

 await kbAutoMemoryReview('cli-memory-aaaa', 'deprecated');
 const discard = calls.filter(c => c.opts && c.opts.method === 'PUT')[1];
 assert.equal(JSON.parse(discard.opts.body).state, 'deprecated', 'discard deprecates so the drain never re-drafts it');

 confirmed = false;
 const before = calls.length;
 await kbAutoMemoryReview('cli-memory-aaaa', 'approved');
 assert.equal(calls.length, before, 'a cancelled confirm sends nothing');

 owner = false;
 renderKnowledgeFacts();
 assert.ok(el.innerHTML.includes('agent scanner'));
 assert.ok(!el.innerHTML.includes('kbAutoMemoryReview'), 'non-owners get no review buttons');

 _kbFacts = [{slug: 'plain', title: 'Plain fact', state: 'draft', tags: ['agent:scanner']}];
 owner = true;
 renderKnowledgeFacts();
 assert.ok(!el.innerHTML.includes('kbAutoMemoryReview') && !el.innerHTML.includes('agent scanner'), 'only drained facts get review controls');
})().catch(e => {console.error(e); process.exit(1);});
`
}

func TestKnowledgeAutoMemoryReviewUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	if out, err := exec.Command(node, "-e", knowledgeAutoMemoryUIScript(t)).CombinedOutput(); err != nil {
		t.Fatalf("auto-memory review UI: %v\n%s", err, out)
	}
}
