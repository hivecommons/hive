package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestKnowledgeFactsPaginationUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
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
	script := `
const assert = require('node:assert/strict');
let _kbFacts = [], _kbPageSize = 50, _kbPageOffset = 0, _kbServerPaged = false,
    _kbHasMore = false, _kbRequest = 0, _kbSelectedFact = null,
    _kbSearchQuery = '', _kbSelectedLayer = 'all', _kbSelectedType = 'gotcha';
const el = {innerHTML: ''};
const document = {getElementById: () => el};
const kbIsDividerOnlyFact = f => f.divider;
const escapeHtml = s => String(s);
const kbCleanFactTitle = s => s;
const kbCleanFactPreview = s => s;
const kbConfidenceBadge = () => '';
const confColor = () => 'green';
const facts = Array.from({length: 121}, (_, i) => ({slug: 'fact-' + i}));
` + section("    function kbSearch() {", "    async function kbReadFact(") + section("    function renderKnowledgeFacts() {", "    function renderKnowledgeDetail()") + `
(async () => {
 _kbFacts = facts;
 renderKnowledgeFacts();
 assert.equal((el.innerHTML.match(/class="kb-fact-item/g) || []).length, 50);
 kbChangePage(1);
 assert.equal(_kbPageOffset, 50);
 assert.ok(el.innerHTML.includes('fact-50'));
 assert.ok(!el.innerHTML.includes('>fact-0<'));
 kbChangePage(1);
 assert.equal((el.innerHTML.match(/class="kb-fact-item/g) || []).length, 21);
 assert.match(el.innerHTML, /data-action="kbNextPage" disabled/);
 let urls = [];
 global.fetch = async url => {
  urls.push(url);
  return {ok: true, json: async () => ({facts: facts.slice(0,50), has_more: urls.length === 1})};
 };
 await kbSearch();
 assert.equal(_kbPageOffset, 0);
 assert.ok(_kbServerPaged);
 assert.match(urls[0], /limit=50&offset=0&include_states=all&type=gotcha/);
 await kbChangePage(1);
 assert.match(urls[1], /offset=50/);
 assert.equal(_kbFacts.length, 50); // replace, never append pages to the DOM
 assert.match(el.innerHTML, /data-action="kbNextPage" disabled/);
 await kbSearch();
 assert.equal(_kbPageOffset, 0);
 _kbFacts = [{divider: true}]; _kbHasMore = true;
 renderKnowledgeFacts();
 assert.match(el.innerHTML, /data-action="kbNextPage">Next/); // empty visible pages remain navigable
})().catch(e => {console.error(e); process.exit(1);});
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("knowledge pagination: %v\n%s", err, out)
	}
}
