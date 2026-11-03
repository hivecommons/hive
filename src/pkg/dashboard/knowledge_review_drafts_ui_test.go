package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestKnowledgeReviewDraftsUI: the Knowledge section shows the owner how many
// review-derived retro lessons await approval; non-owners never request the
// count or see the line (B-AC15).
func TestKnowledgeReviewDraftsUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	start := strings.Index(html, "    async function fetchKnowledgeReviewDrafts() {")
	if start < 0 {
		t.Fatal("missing fetchKnowledgeReviewDrafts")
	}
	end := strings.Index(html[start:], "    async function fetchKnowledgeStats() {")
	if end < 0 {
		t.Fatal("missing fetchKnowledgeStats")
	}
	render := html[strings.Index(html, "    function renderKnowledge() {"):]
	if !strings.Contains(render[:strings.Index(render, "\n    }\n")], "kbReviewDraftsHTML(_kbCache._reviewDrafts, viewerIsOwner())") {
		t.Fatal("renderKnowledge does not render the awaiting-approval line")
	}
	script := `
const assert = require('node:assert/strict');
let owner = false;
const viewerIsOwner = () => owner;
let urls = [];
global.fetch = async url => { urls.push(url); return {ok: true, json: async () => ({total: 3})}; };
` + html[start:start+end] + `
(async () => {
 assert.equal(kbReviewDraftsHTML(1, true).includes('>📝 1 lesson awaiting approval<'), true);
 assert.match(kbReviewDraftsHTML(2, true), /2 lessons awaiting approval/);
 assert.equal(kbReviewDraftsHTML(0, true), '');
 assert.equal(kbReviewDraftsHTML(1, false), '');
 assert.equal(await fetchKnowledgeReviewDrafts(), 0);
 assert.equal(urls.length, 0);
 owner = true;
 assert.equal(await fetchKnowledgeReviewDrafts(), 3);
 assert.deepEqual(urls, ['/api/knowledge?state=draft&tag=retro,review-feedback&limit=1']);
 global.fetch = async () => ({ok: false, status: 403});
 assert.equal(await fetchKnowledgeReviewDrafts(), 0);
})().catch(e => {console.error(e); process.exit(1);});
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("knowledge review drafts: %v\n%s", err, out)
	}
}
