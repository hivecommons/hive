package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// The review section's queue view (#9607) is JS inside index.html, invisible to
// the Go compiler. These tests pin its wiring and execute its renderer so the
// ranked list, the review-priority/* badge, the reasons list and the
// limit/offset/has_more paging stay correct.

func TestReviewSectionNavAndSubsectionOrder(t *testing.T) {
	html := indexHTML(t)
	navStart := strings.Index(html, `<div class="oc-nav-group" id="pull-requests-nav-group">
      <div class="oc-nav-label">Pull requests</div>`)
	if navStart < 0 {
		t.Fatal("Pull requests nav group not found")
	}
	navEnd := strings.Index(html[navStart+1:], `<div class="oc-nav-group"`)
	if navEnd < 0 {
		t.Fatal("Pull requests nav group end not found")
	}
	nav := html[navStart : navStart+1+navEnd]
	if !strings.Contains(nav, `<span class="oc-nav-text">Review</span>`) {
		t.Fatal("Pull requests nav must contain Review")
	}
	for _, forbidden := range []string{`<span class="oc-nav-text">Review Queue</span>`, `<span class="oc-nav-text">Review Pipeline</span>`, `data-section="review-pipeline-section"`} {
		if strings.Contains(nav, forbidden) {
			t.Fatalf("Pull requests nav still contains standalone review entry %q", forbidden)
		}
	}
	cardStart := strings.Index(html, `id="review-queue-section" data-dashboard-section="review-queue-section"`)
	if cardStart < 0 {
		t.Fatal("merged Review card not found")
	}
	cardEnd := strings.Index(html[cardStart:], `id="nous-section"`)
	if cardEnd < 0 {
		t.Fatal("could not isolate merged Review card")
	}
	card := html[cardStart : cardStart+cardEnd]
	pipeline := strings.Index(card, `id="review-pipeline-subsection"`)
	queue := strings.Index(card, `id="review-queue-subsection"`)
	if pipeline < 0 || queue < 0 {
		t.Fatalf("Review card must contain Pipeline and Queue subsections; pipeline=%d queue=%d", pipeline, queue)
	}
	if pipeline > queue {
		t.Fatal("Review card must show Pipeline before Queue")
	}
	if strings.Contains(card, `data-dashboard-section="review-pipeline-section"`) {
		t.Fatal("Review Pipeline must not remain a standalone dashboard card")
	}
	for _, want := range []string{
		`<div class="review-subsection-body dash-card-body section-body" id="review-pipeline-body">`,
		`<div class="review-subsection-body dash-card-body section-body" id="review-queue-body">`,
		`data-arg0="review-pipeline-subsection"`,
		`data-arg0="review-queue-subsection"`,
		`.review-subsection-body.dash-card-body { padding: var(--sp-4); }`,
		`.review-subsection-body > .review-queue-panel { padding: 0; }`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("Review subsections must share padded body chrome; missing %q", want)
		}
	}
}

func TestReviewQueueViewStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`data-dashboard-section="review-queue-section"`,
		`data-section="review-queue-section"`,
		`id="review-queue-panel"`,
		`data-action="reviewQueuePrevPage"`,
		`data-action="reviewQueueNextPage"`,
		`fetch('/api/review/queue?limit=' + REVIEW_QUEUE_PAGE_SIZE + '&offset=' + offset,`,
		`reviewEvidenceButtonHtml(e.repo, e.number) +`,
		`function reviewEvidenceButtonHtml(repo, number)`,
		`data-action="openReviewEvidence"`,
		`['review-queue', () => fetchReviewQueue().then(() => { dashboardSetInterval('review-queue', fetchReviewQueue, REVIEW_QUEUE_POLL_MS); })],`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing review-queue wiring %q", want)
		}
	}
	// The collapsed Review Queue card must use the shared range-control and
	// renderSparkline path, not the old count-only gauge.
	for _, want := range []string{
		`function reviewQueueCollapsedSummary(text)`,
		`function reviewQueueRangeControls()`,
		`overview-kpi-range-btn`,
		`data-action="setReviewQueueWindow"`,
		`function reviewQueueSparkline(entries)`,
		`renderSparkline(null, values`,
		`['high', high, 'High priority']`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing review-queue collapsed sparkline wiring %q", want)
		}
	}
	if strings.Contains(html, `sectionId === 'review-queue-section') {
        inner = miniGauge`) {
		t.Error("Review Queue collapsed summary still uses the gauge renderer")
	}
	// The view must never fall back to a browser-native dialog.
	for _, banned := range []string{"window.alert", "window.confirm", "window.prompt"} {
		if strings.Contains(jsFunc(t, html, "renderReviewQueue"), banned) {
			t.Errorf("renderReviewQueue uses banned native dialog %q", banned)
		}
	}
}

func TestReviewQueueEntryRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: review-queue rendering behavior was not executed")
	}
	html := indexHTML(t)
	var source strings.Builder
	// The evidence button is role-gated on window state that does not exist
	// under node; its wiring is pinned in TestReviewQueueViewStaticWiring.
	source.WriteString("function reviewEvidenceButtonHtml() { return ''; }\n")
	for _, name := range []string{"esc", "reviewQueuePriorityMeta", "reviewQueueAgeLabel", "reviewQueueSupersededHtml", "fixerQueueHint", "renderReviewQueueEntry"} {
		source.WriteString(jsFunc(t, html, name))
		source.WriteByte('\n')
	}
	source.WriteString(reviewQueueRenderingAssertions)
	if out, err := exec.Command(node, "-e", source.String()).CombinedOutput(); err != nil {
		t.Fatalf("review-queue renderer failed: %v\n%s", err, out)
	}
}

const reviewQueueRenderingAssertions = `
const assert = require('node:assert/strict');
function entry(overrides = {}) {
  return renderReviewQueueEntry({ position: 1, repo: 'hivecommons/hive', number: 42,
    title: 'fix: guard nil deref', url: 'https://github.com/hivecommons/hive/pull/42',
    author: 'octocat', hive_authored: false, priority: 'high', ci_state: 'green',
    confidence_band: 'needs-attention', age_hours: 30, held: false,
    reasons: ['triage class: fix', 'unreviewed head'], ...overrides });
}

// Rank, linked title, repo#number and the review-priority label all render.
let html = entry();
assert.match(html, /#1/);
assert.match(html, /hivecommons\/hive#42/);
assert.match(html, /href="https:\/\/github.com\/hivecommons\/hive\/pull\/42"/);
assert.match(html, /fix: guard nil deref/);
assert.match(html, /review-priority\/high/);
assert.match(html, /review-priority-high/);
assert.match(html, /@octocat/);
assert.match(html, /contributor/);
assert.match(html, /1d old/);

// The reasons list is rendered, one <li> per reason.
assert.match(html, /triage class: fix/);
assert.match(html, /unreviewed head/);
assert.equal((html.match(/<li>/g) || []).length, 2);

// Priority maps deterministically; unknown bands fall back to normal.
assert.match(entry({ priority: 'normal' }), /review-priority\/normal/);
assert.match(entry({ priority: 'low' }), /review-priority\/low/);
assert.match(entry({ priority: 'weird' }), /review-priority\/normal/);

// Agent authorship and held state surface.
assert.match(entry({ hive_authored: true }), /agent/);
assert.match(entry({ held: true }), /held/);

// Fixer status is independent of review rank and disappears after delivery.
assert.doesNotMatch(entry(), /fixer queued/);
assert.match(entry({ fixer_queue_position: 3, fixer_queue_ahead: 2 }), /fixer queued, 2 ahead/);
assert.match(entry({ fixer_queue_position: 1, fixer_queue_ahead: 0 }), /fixer queued, 0 ahead/);
assert.doesNotMatch(entry({ fixer_queue_position: 0, fixer_queue_ahead: 0 }), /fixer queued/);
assert.doesNotMatch(entry({ fixer_queue_ahead: 4 }), /fixer queued/);
assert.doesNotMatch(entry({ fixer_queue_position: '<img src=x>' }), /fixer queued/);

// No reasons still renders without throwing.
assert.match(entry({ reasons: [] }), /no ranking reasons recorded/);

// Untrusted PR titles are HTML-escaped, not injected.
const evil = entry({ title: '<img src=x onerror=alert(1)>' });
assert.doesNotMatch(evil, /<img src=x/);
assert.match(evil, /&lt;img src=x/);
`

func TestReviewQueuePagingKeepsSelectedPage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	html := indexHTML(t)
	start := strings.Index(html, "    function renderReviewQueue(data)")
	end := strings.Index(html, "    // fetchReviewQueue() — deferred to _deferredInit")
	if start < 0 || end <= start {
		t.Fatal("could not find review-queue paging source")
	}
	script := html[start:end] + reviewQueuePagingFixture + fetchWrapperSource(t, html) + reviewQueuePagingAssertions
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("review-queue paging failed: %v\n%s", err, out)
	}
}

const reviewQueuePagingFixture = `
const assert = require('node:assert/strict');
const REVIEW_QUEUE_PAGE_SIZE = 25;
const DASHBOARD_SERVER_HISTORY_CAP = 50;
let _reviewQueueTotal = 0, _reviewQueueLimit = 25, _reviewQueueOffset = 0, _reviewQueueHasMore = false;
const window = { _reviewQueueHistory: [], location: new URL('http://localhost/'), fetch: fetchFixture };
let storedToken = 'fixture-old-token';
const localStorage = {
  getItem: () => storedToken,
  setItem: (_, value) => { storedToken = value; },
  removeItem: () => { storedToken = null; }
};
let finishAuthPrompt;
function hivePrompt() {
  return new Promise(resolve => { finishAuthPrompt = resolve; });
}
const panel = { innerHTML: '', querySelector: () => panel.innerHTML.includes('review-queue-entry') };
function pagingButton() {
  return { setAttribute: function (name, value) { this[name] = value; } };
}
const range = {}, prev = pagingButton(), next = pagingButton();
const elements = {
  'review-queue-panel': panel, 'review-queue-paging': { style: {} },
  'review-queue-range': range, 'review-queue-prev': prev, 'review-queue-next': next,
};
const document = { getElementById: id => elements[id] };
function refreshSectionCardShell() {}
function setSectionSummary() {}
function updateReviewSectionSummary() {}
function capDashboardArray(value) { return value; }
function renderReviewQueueEntry() { return '<div class="review-queue-entry">PR</div>'; }

const requests = [];
const deadlines = new Set();
function setTimeout(callback) { deadlines.add(callback); return callback; }
function clearTimeout(callback) { deadlines.delete(callback); }
function fetch(url, opts) { return window.fetch(url, opts); }
function fetchFixture(url, opts) {
  const target = new URL(url, window.location);
  if (target.pathname === '/api/auth/token') return Promise.resolve({ ok: true, json: async () => ({ configured: true }) });
  if (target.pathname === '/api/gh-user-auth/status') return Promise.resolve({ ok: true, json: async () => ({ logged_in: false }) });
  assert.equal(target.pathname, '/api/review/queue');
  const offset = Number(target.searchParams.get('offset'));
  return new Promise(resolve => requests.push({offset, resolve, signal: opts.signal, authorization: opts.headers?.Authorization}));
}
function page(offset) {
  return { offset, limit: 25, total: 75, has_more: offset < 50, items: Array(25).fill({}) };
}
function reply(offset) {
  const request = requests.shift();
  assert.equal(request.offset, offset);
  request.resolve({ ok: true, json: async () => page(offset) });
}
async function expireDeadline(request) {
  assert.equal(deadlines.size, 1);
  const [expire] = deadlines;
  expire();
  await Promise.race([request, new Promise((_, reject) => {
    setImmediate(() => reject(new Error('paging stayed locked after timeout')));
  })]);
}
`

const reviewQueuePagingAssertions = `
// An unresolved promise does not keep Node alive.
process.exitCode = 1;
(async function () {
  const initial = fetchReviewQueue();
  reply(0);
  await initial;
  assert.equal(range.textContent, '1–25 of 75');

  // Let the old poll reach JSON decoding before the operator clicks Next.
  const poll = fetchReviewQueue();
  const oldRequest = requests.shift();
  assert.equal(oldRequest.offset, 0);
  let finishOldPage;
  oldRequest.resolve({ ok: true, json: () => new Promise(resolve => { finishOldPage = resolve; }) });
  await new Promise(setImmediate);
  assert.equal(typeof finishOldPage, 'function');
  const advance = reviewQueueNextPage();
  reviewQueueNextPage();
  fetchReviewQueue();
  assert.deepEqual(requests.map(request => request.offset), [25]);
  assert.equal(next.disabled, false);
  assert.equal(next['aria-disabled'], 'true');
  reply(25);
  await advance;
  assert.equal(range.textContent, '26–50 of 75');
  finishOldPage(page(0));
  await poll;
  assert.equal(range.textContent, '26–50 of 75');

  const failed = reviewQueueNextPage();
  const failedRequest = requests.shift();
  assert.equal(failedRequest.offset, 50);
  failedRequest.resolve({ ok: false, status: 500 });
  await failed;
  assert.equal(range.textContent, '26–50 of 75');
  assert.equal(prev.disabled, false);
  assert.equal(next.disabled, false);
  assert.equal(prev['aria-disabled'], 'false');
  assert.equal(next['aria-disabled'], 'false');
  const retry = reviewQueueNextPage();
  reply(50);
  await retry;
  assert.equal(range.textContent, '51–75 of 75');

  // A fetch wrapper can stay pending even after the network is aborted.
  const stalled = reviewQueuePrevPage();
  const stalledRequest = requests.shift();
  assert.equal(stalledRequest.offset, 25);
  assert.equal(prev['aria-disabled'], 'true');
  await expireDeadline(stalled);
  assert.equal(stalledRequest.signal.aborted, true);
  assert.equal(range.textContent, '51–75 of 75');
  assert.equal(prev['aria-disabled'], 'false');
  const back = reviewQueuePrevPage();
  const authRequest = requests.shift();
  assert.equal(authRequest.offset, 25);
  assert.equal(authRequest.authorization, 'Bearer fixture-old-token');
  authRequest.resolve({ ok: false, status: 401 });
  await new Promise(setImmediate);
  assert.equal(typeof finishAuthPrompt, 'function');
  assert.equal(deadlines.size, 0);
  assert.equal(prev['aria-disabled'], 'true');
  finishAuthPrompt('fixture-new-token');
  await new Promise(setImmediate);
  assert.equal(deadlines.size, 1);
  assert.equal(requests[0].authorization, 'Bearer fixture-new-token');
  reply(25);
  await back;
  assert.equal(range.textContent, '26–50 of 75');

  const stalledBody = reviewQueueNextPage();
  const bodyRequest = requests.shift();
  assert.equal(bodyRequest.offset, 50);
  let finishStalledBody;
  bodyRequest.resolve({ ok: true, json: () => new Promise(resolve => { finishStalledBody = resolve; }) });
  await new Promise(setImmediate);
  assert.equal(typeof finishStalledBody, 'function');
  await expireDeadline(stalledBody);
  assert.equal(bodyRequest.signal.aborted, true);
  assert.equal(next['aria-disabled'], 'false');
  finishStalledBody(page(50));
  await new Promise(setImmediate);
  assert.equal(range.textContent, '26–50 of 75');
  assert.equal(deadlines.size, 0);
})().then(() => { process.exitCode = 0; }, err => console.error(err));
`

func TestReviewQueueSupersededActionsWithoutAutoClose(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	html := indexHTML(t)
	source := jsFunc(t, html, "esc") + "\n" + jsFunc(t, html, "reviewQueueSupersededHtml") + `
const assert = require('node:assert/strict');
global.window = { _hiveRole: 'owner' };
function dashboardRoleAtLeast(role, required) { return role === required; }
function formatDashboardDateTime(at) { return at.toISOString(); }
const entry = {repo: 'myorg/repo1', number: 2, superseded: {
  closer_repo: 'myorg/repo1', closer_pr: 9, closer_url: 'https://github.com/myorg/repo1/pull/9'
}};
let html = reviewQueueSupersededHtml(entry);
assert.match(html, /myorg\/repo1#9/);
assert.match(html, /data-action="reviewQueueSupersededClose"/);
assert.match(html, /data-action="reviewQueueSupersededKeepOpen"/);
assert.match(html, /close as superseded or keep open/);
assert.doesNotMatch(html, /auto-closes|closes on the next sweep|grace window/);
entry.superseded.grace_ends_at = new Date(Date.now() + 3600000).toISOString();
assert.match(reviewQueueSupersededHtml(entry), /auto-closes/);
window._hiveRole = 'read';
assert.doesNotMatch(reviewQueueSupersededHtml(entry), /data-action=/);
`
	if out, err := exec.Command(node, "-e", source).CombinedOutput(); err != nil {
		t.Fatalf("superseded renderer failed: %v\n%s", err, out)
	}
}
