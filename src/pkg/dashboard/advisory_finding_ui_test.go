package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// Execute the shipped renderer, including its per-finding helpers, so these
// fixtures test visible behavior rather than the presence of source strings.
func TestAdvisoryFindingRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: advisory rendering behavior was not executed")
	}
	html := indexHTML(t)
	var source strings.Builder
	for _, name := range []string{"escapeHtml", "advisoryFindingTime", "advisoryFindingStateHTML", "renderAdvisoryDigest"} {
		source.WriteString(jsFunc(t, html, name))
		source.WriteByte('\n')
	}
	source.WriteString(advisoryFindingRenderingAssertions)
	if out, err := exec.Command(node, "-e", source.String()).CombinedOutput(); err != nil {
		t.Fatalf("advisory renderer failed: %v\n%s", err, out)
	}
}

func TestAdvisoryDigestUsesSharedSubsectionShell(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: advisory subsection shell behavior was not executed")
	}
	html := indexHTML(t)
	var source strings.Builder
	source.WriteString(`const assert = require('node:assert/strict');
function textOrDash(value) { return value || '—'; }
function visualSectionSummary(sectionId, text) { return text; }
function dashboardSectionHelpMark(sectionId) { return '<a class="config-info section-help-mark" data-section="' + sectionId + '">?</a>'; }
`)
	for _, name := range []string{"esc", "sectionCardHeader", "sectionCardShell"} {
		source.WriteString(jsFunc(t, html, name))
		source.WriteByte('\n')
	}
	source.WriteString(advisorySubsectionShellAssertions)
	if out, err := exec.Command(node, "-e", source.String()).CombinedOutput(); err != nil {
		t.Fatalf("advisory subsection shell check failed: %v\n%s", err, out)
	}
}

const advisorySubsectionShellAssertions = `
const sections = [
  ['advisory-digest-section', '📋 Advisory Digest', '<div id="advisory-digest" class="advisory-subsection-card"></div>'],
  ['hive-advice-section', '🧭 Hive Advice', '<div id="hive-advice-card" class="advisory-subsection-card"></div>'],
  ['acmm-reco-section', '⬆️ Ready to level up?', '<div id="acmm-reco-card" class="advisory-subsection-card"></div>'],
  ['lifecycle-section', '🔀 Lifecycle Timeline', '<div id="lifecycle-card" class="advisory-subsection-card"></div>'],
];
for (const [id, title, body] of sections) {
  const out = sectionCardShell({ id, title, summary: '0', bodyId: id + '-body' }, body);
  assert.match(out, /class="dash-card"/, id + ' missing shared card');
  assert.match(out, /class="dash-card-header section-header-toggle"/, id + ' missing shared header class');
  assert.match(out, /data-section-card-header="1"/, id + ' missing shared header marker');
  assert.match(out, /class="dashboard-grip"/, id + ' missing shared drag handle');
  assert.match(out, /class="section-chevron /, id + ' missing shared caret');
  assert.match(out, /class="config-info section-help-mark"/, id + ' missing help mark');
  assert.ok(out.indexOf('data-section-card-shell="1"') < out.indexOf('data-section-card-header="1"'), id + ' header is not inside card shell');
  assert.ok(out.indexOf('data-section-card-header="1"') < out.indexOf('id="' + id + '-body"'), id + ' header is not before body inside card');
}
`

const advisoryFindingRenderingAssertions = `
const assert = require('node:assert/strict');
Date.now = () => Date.parse('2026-09-08T12:00:00Z');
let rendered = '';
const card = { classList: { toggle() {} } };
const document = { getElementById(id) { assert.equal(id, 'advisory-digest'); return card; } };
function isAdvisoryCollapsed() { return false; }
function refreshSectionCardShell(sectionId) { assert.equal(sectionId, 'advisory-digest-section'); }
function setSectionSummary(sectionId, value) { assert.equal(sectionId, 'advisory-digest-section'); assert.match(value, /finding/); }
function applySectionCollapse(sectionId) { assert.equal(sectionId, 'advisory-digest-section'); }
function setIfChanged(el, value) { assert.equal(el, card); rendered = value; }
function renderFinding(overrides = {}) {
  const finding = { agent: 'quality', type: 'coverage-gap', severity: 'high',
    title: 'cleanup paths lack coverage', timestamp: '2026-09-05T12:00:00Z',
    last_seen_at: '2026-09-08T11:42:00Z', provenance_sha: 'f613508123456789', ...overrides };
  renderAdvisoryDigest({ mode: 'advisory', total_count: 1, generated_at: '2026-09-08T12:00:00Z', by_agent: { quality: [finding] } });
  return rendered;
}
let html = renderFinding();
assert.match(html, /Created <time[^>]+>3d ago<\/time> · last seen <time[^>]+>18m ago<\/time>/);
assert.match(html, /f61350812345/);
assert.match(html, /no known freshness warning/);
assert.match(html, /No linked remediation/);
assert.doesNotMatch(html, /last verified|No PR exists|⚠/);

html = renderFinding({ provenance_stale: true });
assert.match(html, /⚠ Evidence/);
assert.match(html, /provenance stale — not re-verified at the analyzed snapshot/);
assert.doesNotMatch(html, /no known freshness warning/);

html = renderFinding({ provenance_sha: '', cached_replays: 3 });
assert.match(html, /cached replay — not re-verified/);
assert.doesNotMatch(html, /no known freshness warning/);
assert.doesNotMatch(renderFinding({ cached_replays: 3 }), /cached replay/);

html = renderFinding({ path_stale: true, provenance_stale: true });
assert.match(html, /referenced file\/path stale/);
assert.match(html, /provenance stale/);

const reference = { owner: 'org', repo: 'repo', number: 224, kind: 'pr',
  state: 'OPEN', url: 'https://github.com/org/repo/pull/224', checked_at: '2026-09-08T11:59:00Z' };
html = renderFinding({ linked_work: [reference] });
assert.match(html, /href="https:\/\/github.com\/org\/repo\/pull\/224"/);
assert.match(html, /PR #224 \(org\/repo\)/);
assert.match(html, /OPEN · in flight/);
assert.match(html, /checked <time[^>]+>1m ago/);
assert.doesNotMatch(html, /No linked remediation/);
for (const state of ['MERGED', 'CLOSED']) {
  html = renderFinding({ linked_work: [{ ...reference, state }] });
  assert.match(html, new RegExp(' · ' + state));
  assert.match(html, /cleanup paths lack coverage/);
  assert.doesNotMatch(html, /in flight/);
}
for (const state of ['OPEN', 'CLOSED']) {
  html = renderFinding({ linked_work: [{ ...reference, kind: 'issue', state }] });
  assert.match(html, /Issue #224/);
  assert.match(html, new RegExp(' · ' + state));
  assert.doesNotMatch(html, /in flight/);
}
html = renderFinding({ linked_work: [{ ...reference, kind: '', state: 'UNKNOWN', checked_at: '' }] });
assert.match(html, /UNKNOWN \(state unavailable\)/);
assert.match(html, /href=/);
assert.doesNotMatch(html, /No linked remediation|checked <time/);

html = renderFinding({ timestamp: '0001-01-01T00:00:00Z', last_seen_at: undefined, provenance_sha: '' });
assert.match(html, /Created unknown · last seen unknown/);
assert.doesNotMatch(html, /Invalid Date|NaN/);
assert.equal(advisoryFindingTime('invalid'), 'unknown');
assert.match(advisoryFindingTime('2026-09-09T00:00:00Z'), /just now/);

html = renderFinding({ provenance_sha: '<img src=x onerror=alert(1)>',
  linked_work: [{ ...reference, owner: '<script>', url: 'javascript:alert(1)' }] });
assert.doesNotMatch(html, /<img|<script>|href="javascript:/);
assert.match(html, /&lt;script&gt;/);

renderAdvisoryDigest(null);
assert.match(rendered, /class="advisory-agent-body muted"/);
assert.match(rendered, /text-align:left/);
assert.doesNotMatch(rendered, /text-align:center|padding:20px/);
`
