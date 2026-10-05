package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// The review-queue view (#9607) is JS inside index.html, invisible to the Go
// compiler. These tests pin its wiring and execute its renderer so the ranked
// list, the review-priority/* badge, the reasons list and the limit/offset/
// has_more paging stay correct.

func TestReviewQueueViewStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`data-dashboard-section="review-queue-section"`,
		`data-section="review-queue-section"`,
		`id="review-queue-panel"`,
		`data-action="reviewQueuePrevPage"`,
		`data-action="reviewQueueNextPage"`,
		`fetch('/api/review/queue?limit=' + REVIEW_QUEUE_PAGE_SIZE + '&offset=' + _reviewQueueOffset)`,
		`() => fetchReviewQueue().then(() => { dashboardSetInterval('review-queue', fetchReviewQueue, REVIEW_QUEUE_POLL_MS); }),`,
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
	for _, name := range []string{"esc", "reviewQueuePriorityMeta", "reviewQueueAgeLabel", "renderReviewQueueEntry"} {
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

// No reasons still renders without throwing.
assert.match(entry({ reasons: [] }), /no ranking reasons recorded/);

// Untrusted PR titles are HTML-escaped, not injected.
const evil = entry({ title: '<img src=x onerror=alert(1)>' });
assert.doesNotMatch(evil, /<img src=x/);
assert.match(evil, /&lt;img src=x/);
`
