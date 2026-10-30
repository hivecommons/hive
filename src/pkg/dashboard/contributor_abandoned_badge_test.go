package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestContributorAbandonedBadge(t *testing.T) {
	html := indexHTML(t)
	render := jsFunctionBody(t, html, "function renderContributors(contributors)")
	if !strings.Contains(render, "${contributorAbandonedBadge(c)}") {
		t.Fatal("contributor cards do not render the abandoned badge")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — abandoned badge rendering was NOT executed")
	}
	js := jsFunctionBody(t, html, "function esc(s)") + "\n" + jsFunctionBody(t, html, "function contributorAbandonedBadge(c)") + `
function assert(ok, message) { if (!ok) throw new Error(message); }
for (const c of [{}, {abandoned: 0}, {abandoned: -1}]) {
  assert(contributorAbandonedBadge(c) === '', 'zero/legacy counters should be hidden');
}
const records = Array.from({length: 7}, (_, i) => ({repo: 'org/repo', pr: i + 1,
  adopted_into_pr: i === 6 ? 90 : 0, reason: 'offline', at: '2026-10-0' + (i + 1) + 'T12:00:00Z'}));
records[6].reason = 'operator_takeover';
const before = JSON.stringify(records);
const badge = contributorAbandonedBadge({abandoned: 7, abandoned_prs: records});
assert(badge.includes('Abandoned 7'), badge);
assert(badge.includes('class="contrib-abandoned"'), 'badge class missing');
assert(badge.includes('org/repo#7 → #90 · operator_takeover · 2026-10-07T12:00:00Z'), badge);
assert(badge.includes('org/repo#6 → in-place · offline'), badge);
assert(badge.indexOf('org/repo#7') < badge.indexOf('org/repo#6'), 'not newest first');
assert(!badge.includes('org/repo#2') && badge.includes('org/repo#3'), 'tooltip must cap at five recent PRs');
assert(JSON.stringify(records) === before, 'render mutated source records');
const unsafe = contributorAbandonedBadge({abandoned: 1, abandoned_prs: [{repo: '\"<img>', pr: 2,
  reason: '\" onmouseover=\"evil', at: '<script>'}]});
assert(!unsafe.includes('<img>') && !unsafe.includes('<script>') && !unsafe.includes('" onmouseover="'), unsafe);
assert(contributorAbandonedBadge({abandoned: 1}).includes('PRs adopted by the hive'), 'fallback tooltip missing');
`
	if out, err := exec.Command(node, "-e", js).CombinedOutput(); err != nil {
		t.Fatalf("abandoned badge rendering failed: %v\n%s", err, out)
	}
}
