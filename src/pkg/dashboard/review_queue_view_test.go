package dashboard

import (
	"net/http"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

const (
	reviewQueueViewStartMarker = "    // --- PR review queue (#9590) ---"
	reviewQueueViewEndMarker   = "    // fetchReviewQueue() - deferred to _deferredInit"
)

// reviewQueueViewJS returns the dashboard's review queue view script: the
// constants, state and functions between its two markers.
func reviewQueueViewJS(t *testing.T, html string) string {
	t.Helper()
	start := strings.Index(html, reviewQueueViewStartMarker)
	end := strings.Index(html, reviewQueueViewEndMarker)
	if start < 0 || end < start {
		t.Fatalf("index.html review queue view markers not found (start=%d end=%d)", start, end)
	}
	return html[start:end]
}

// jsonTags returns the JSON field names of a struct type.
func jsonTags(t *testing.T, v any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

// TestReviewQueueViewStaticWiring pins the view's wiring (#9590): a nav entry,
// a reorderable/collapsible section, the paged fetch of the endpoint, and a
// deferred-init poll. Without it the whole view can be deleted with the API
// tests still green.
func TestReviewQueueViewStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`data-section="review-queue-section" data-action="ocNavigate" data-arg0="review-queue-section"`,
		`id="review-queue-section" data-dashboard-section="review-queue-section"`,
		`aria-label="Move Review queue section"`,
		`id="review-queue-panel"`,
		`data-action="reviewQueuePage" data-arg0="-1"`,
		`data-action="reviewQueuePage" data-arg0="1"`,
		`'approvals-section','review-queue-section','platform-section'`,
		`'review-queue-section', 'platform-section',`,
		`fetch('/api/review/queue?limit=' + REVIEW_QUEUE_PAGE_SIZE + '&offset=' + _reviewQueueOffset)`,
		`() => fetchReviewQueue(true).then(() => { setInterval(fetchReviewQueue, REVIEW_QUEUE_POLL_MS); }),`,
		`if (section === 'review-queue-section' && level < ACMM_GOVERNOR_MIN_LEVEL) hide(item);`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("review queue view wiring missing %q", want)
		}
	}
	js := reviewQueueViewJS(t, html)
	// The page size must be one the API accepts.
	if !strings.Contains(js, "const REVIEW_QUEUE_PAGE_SIZE = 25;") {
		t.Fatal("view page size constant not found")
	}
	if rec := doOwnerGet(reviewQueueTestServer(t, filepath.Join(t.TempDir(), "missing.json")), "/api/review/queue?limit=25"); rec.Code != http.StatusOK {
		t.Fatalf("API rejects the view's page size: %d %s", rec.Code, rec.Body.String())
	}
	// Untrusted PR fields go through esc(); nothing builds markup from a raw
	// title, author or reason.
	for _, raw := range []string{"+ e.title", "+ e.author", "+ r +", "+ e.url"} {
		if strings.Contains(js, raw) {
			t.Errorf("view concatenates an unescaped field: %q", raw)
		}
	}
}

// TestReviewQueueViewDataContract ties the view to the API's JSON shape: every
// field the view reads is one the endpoint emits, so renaming a JSON tag
// breaks this test instead of silently blanking a column.
func TestReviewQueueViewDataContract(t *testing.T) {
	js := reviewQueueViewJS(t, indexHTML(t))
	entry := jsonTags(t, ghpkg.ReviewQueueEntry{})
	page := jsonTags(t, reviewQueueResponse{})
	entryFields := []string{
		"position", "repo", "number", "url", "title", "author", "hive_authored", "held",
		"tier", "review_class", "priority", "confidence_band", "confidence_score",
		"reviewed", "ci_state", "age_hours", "reasons",
	}
	for _, f := range entryFields {
		if !entry[f] {
			t.Errorf("view reads e.%s but ReviewQueueEntry has no such JSON field", f)
		}
		if !strings.Contains(js, "e."+f) {
			t.Errorf("view does not render entry field %q", f)
		}
	}
	for _, f := range []string{"items", "total", "offset", "has_more", "snapshot_at", "version"} {
		if !page[f] {
			t.Errorf("view reads d.%s but the response has no such JSON field", f)
		}
		if !strings.Contains(js, "d."+f) && !strings.Contains(js, "d && d."+f) {
			t.Errorf("view does not use response field %q", f)
		}
	}
}

// TestReviewQueueViewRendersAPIOutput runs the view's own script under node
// against a real /api/review/queue response: rows render in rank order,
// untrusted text is escaped, a non-http URL is never linked, and the poll
// re-renders only when the version (or page) changes, keeping expanded rows
// open across a re-render.
func TestReviewQueueViewRendersAPIOutput(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH - review queue view was NOT executed by this run")
	}
	orig := review.ReviewVerdictsPath
	review.ReviewVerdictsPath = filepath.Join(t.TempDir(), "missing.json")
	t.Cleanup(func() { review.ReviewVerdictsPath = orig })

	s := covApiServer(t)
	opened := time.Now().Add(-3 * time.Hour)
	s.deps.Scheduler = metricsSchedulerStub{actionable: &ghpkg.ActionableResult{
		GeneratedAt: opened,
		PRs: ghpkg.PRResult{Items: []ghpkg.PullRequest{
			{Repo: "repo1", Number: 10, Title: "[quality] test: cover rescan", Author: "bot", Labels: []string{"agent/quality"}, URL: "javascript:alert(1)", HeadSHA: "h10", CIStatus: "success", CreatedAt: opened},
			{Repo: "repo1", Number: 11, Title: "fix: <img src=x onerror=alert(1)>", Author: "alice", URL: "https://github.com/myorg/repo1/pull/11", HeadSHA: "h11", CIStatus: "failure", CreatedAt: opened},
		}},
	}}
	rec := doOwnerGet(s, "/api/review/queue")
	api := decodeReviewQueue(t, rec)
	if api.Total != 2 || api.Items[0].Number != 11 {
		t.Fatalf("fixture queue = %+v", api)
	}

	html := indexHTML(t)
	script := jsFunc(t, html, "esc") + `
const els = {};
function mk(id) {
  if (!els[id]) els[id] = { id, innerHTML: '', textContent: '', disabled: false, hidden: false, attrs: {}, dataset: {},
    setAttribute(k, v) { this.attrs[k] = v; }, removeAttribute(k) { delete this.attrs[k]; } };
  return els[id];
}
const document = { hidden: false, getElementById: mk };
const API = ` + rec.Body.String() + `;
let apiVersion = API.version;
const requested = [];
// Replace node's own fetch; the view calls the global.
Object.defineProperty(globalThis, 'fetch', { configurable: true, writable: true, value: async function(url) {
  requested.push(url);
  return { ok: true, status: 200, json: async () => Object.assign({}, API, { version: apiVersion }) };
} });
` + reviewQueueViewJS(t, html) + `
let renders = 0;
const origRender = renderReviewQueue;
renderReviewQueue = function(d) { renders++; return origRender(d); };
function fail(msg) { throw new Error(msg); }
(async () => {
  await fetchReviewQueue(true);
  if (renders !== 1) fail('initial load did not render');
  let out = els['review-queue-panel'].innerHTML;
  if (out.includes('<img')) fail('unescaped title rendered: ' + out);
  if (!out.includes('&lt;img')) fail('escaped title missing');
  if (out.includes('javascript:')) fail('non-http URL rendered');
  if (!out.includes('href="https://github.com/myorg/repo1/pull/11"')) fail('PR link missing');
  if (!(out.indexOf('myorg/repo1#11') < out.indexOf('myorg/repo1#10'))) fail('rows not in rank order');
  if (!out.includes('unreviewed')) fail('unreviewed band not shown');
  if (els['review-queue-count'].textContent !== '2 open PRs') fail('count: ' + els['review-queue-count'].textContent);
  if (!els['review-queue-prev'].disabled || !els['review-queue-next'].disabled) fail('pager should be disabled on a single page');

  await fetchReviewQueue();
  if (renders !== 1) fail('re-rendered although the version did not change');

  els['rq-reasons-0'] = { hidden: true };
  reviewQueueToggleReasons('myorg/repo1#11', '0');
  if (els['rq-reasons-0'].hidden) fail('toggle did not expand');
  apiVersion = 'changed';
  await fetchReviewQueue();
  if (renders !== 2) fail('did not re-render on a new version');
  out = els['review-queue-panel'].innerHTML;
  if (!out.includes('<tr id="rq-reasons-0"><td>')) fail('expanded row collapsed by re-render');
  if (!out.includes('<tr id="rq-reasons-1" hidden>')) fail('unexpanded row not hidden');

  // A page past the end (PRs merged away) steps back to the last real page.
  await reviewQueuePage(1);
  if (_reviewQueueOffset !== 0) fail('offset not clamped: ' + _reviewQueueOffset);
  if (!requested[requested.length - 1].endsWith('&offset=0')) fail('did not refetch the last page: ' + requested[requested.length - 1]);

  // A hidden tab does not poll.
  const before = requested.length;
  document.hidden = true;
  await fetchReviewQueue();
  if (requested.length !== before) fail('polled while hidden');
})().catch(e => { console.error(e.message); process.exit(1); });
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node review queue view failed: %v\n%s", err, out)
	}
}
