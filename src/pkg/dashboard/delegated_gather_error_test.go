package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/github"
)

// projectbluefin, 2026-11-06: the App was out of quota for half an hour and
// every "Let hive decide" preview read "(hive unavailable)": the plan
// answered 502, which the hosted Ingress swaps for its canned body. The
// plan now answers 429 with Retry-After and when to retry, for GitHub's
// primary and secondary limits alike.
func TestDelegatedDecisionPlanRateLimitedAnswers429WithReset(t *testing.T) {
	// A rate-limit 403 engages the shared pacer; release it so the rest of
	// the package is not paced. X-RateLimit-Limit 60 keeps the shared
	// core-bucket snapshot unmoved (an unauthenticated bucket).
	t.Cleanup(github.ResetRateLimitPacingForTest)
	t.Cleanup(github.ResetRateLimitBudgetForTest)
	reset := time.Now().Add(21 * time.Minute).Truncate(time.Second)
	for _, tc := range []struct {
		name      string
		refuse    func(w http.ResponseWriter)
		minS      int
		maxS      int
		wantError string
	}{
		{"primary", func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Limit", "60")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for installation ID 1."}`)
		}, 19 * 60, 22 * 60, "GitHub API rate limit for this hive is exhausted; resets at " + reset.UTC().Format("15:04Z")},
		{"secondary", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "90")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`)
		}, 85, 90, "GitHub secondary rate limit reached for this hive; retry after "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github.ResetRateLimitPacingForTest()
			ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/collaborators/writer/permission") {
					_, _ = io.WriteString(w, `{"permission":"write"}`)
					return
				}
				tc.refuse(w)
			}))
			t.Cleanup(ghSrv.Close)
			srv := newDelegatedServer(t, &repoItemDecisionMock{})
			srv.deps.GHClient = github.NewClient("token", "testorg", []string{"testrepo"}, slog.Default(), ghSrv.URL)
			srv.deps.GHClient.SetAppBotLogin("hive[bot]")
			srv.deps.GHClient.SetPRDetailTTLFunc(func() time.Duration { return 0 })

			w := postDelegatedPlan(t, srv, 8, "writer", `{"type":"pr"}`, nil)
			if w.Code == http.StatusBadGateway || w.Code == http.StatusServiceUnavailable {
				t.Fatalf("code=%d body=%s: the Ingress rewrites this to \"hive unavailable\"", w.Code, w.Body.String())
			}
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("code=%d body=%s, want 429", w.Code, w.Body.String())
			}
			secs, err := strconv.Atoi(w.Header().Get("Retry-After"))
			if err != nil || secs < tc.minS || secs > tc.maxS {
				t.Fatalf("Retry-After = %q, want %d..%d s", w.Header().Get("Retry-After"), tc.minS, tc.maxS)
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(body.Error, tc.wantError) {
				t.Fatalf("error = %q, want it to start %q", body.Error, tc.wantError)
			}
		})
	}
}

// 502 and 503 are the Ingress's "hive unavailable": no GitHub refusal may
// use them, and each answer carries the real reason.
func TestDelegatedGatherErrorNeverAnswers502ForGitHubRefusals(t *testing.T) {
	now := time.Date(2026, 11, 6, 17, 10, 7, 0, time.UTC)
	reset := time.Date(2026, 11, 6, 17, 32, 6, 0, time.UTC)
	retry := 75 * time.Second
	resp := func(code int) *http.Response {
		return &http.Response{StatusCode: code, Header: http.Header{}, Request: &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/repos/o/r/issues/1"}}}
	}
	for _, tc := range []struct {
		name     string
		err      error
		status   int
		contains string
		retry    time.Duration
	}{
		{"primary rate limit", fmt.Errorf("reading o/r#1: %w", &gh.RateLimitError{Rate: gh.Rate{Limit: 7000, Reset: gh.Timestamp{Time: reset}}, Response: resp(http.StatusForbidden), Message: "API rate limit of 7000 still exceeded"}),
			http.StatusTooManyRequests, "resets at 17:32Z", reset.Sub(now)},
		{"secondary rate limit", fmt.Errorf("reading PR o/r#1: %w", &gh.AbuseRateLimitError{Response: resp(http.StatusForbidden), Message: "You have exceeded a secondary rate limit", RetryAfter: &retry}),
			http.StatusTooManyRequests, "secondary rate limit reached for this hive; retry after 17:12Z", retry},
		{"secondary without a time", &gh.AbuseRateLimitError{Response: resp(http.StatusForbidden)},
			http.StatusTooManyRequests, "retry after 17:12Z", delegatedGatherRetryFallback},
		{"pacer shed", fmt.Errorf("reading o/r#1: %w", &url.Error{Op: "Get", URL: "https://api.github.com/repos/o/r/issues/1", Err: github.ErrPacingShed}),
			http.StatusTooManyRequests, "pacing its GitHub requests", delegatedGatherRetryFallback},
		{"not found", fmt.Errorf("reading o/r#1: %w", &gh.ErrorResponse{Response: resp(http.StatusNotFound), Message: "Not Found"}),
			http.StatusFailedDependency, "404 Not Found", 0},
		{"permission", fmt.Errorf("reading o/r#1: %w", &gh.ErrorResponse{Response: resp(http.StatusForbidden), Message: "Resource not accessible by integration"}),
			http.StatusFailedDependency, "Resource not accessible by integration", 0},
		{"no snapshot", errDelegatedNoSnapshot, http.StatusFailedDependency, "no dashboard snapshot yet", 0},
		{"transport", fmt.Errorf("reading o/r#1: %w", &url.Error{Op: "Get", URL: "https://api.github.com/repos/o/r/issues/1", Err: errors.New("connection refused")}),
			http.StatusBadGateway, "connection refused", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, msg, d := delegatedGatherErrorStatus(tc.err, now)
			if status != tc.status || !strings.Contains(msg, tc.contains) || d != tc.retry {
				t.Fatalf("got (%d, %q, %v), want (%d, …%q…, %v)", status, msg, d, tc.status, tc.contains, tc.retry)
			}
			if tc.status != http.StatusBadGateway && (status == http.StatusBadGateway || status == http.StatusServiceUnavailable) {
				t.Fatalf("%s answered %d, which the Ingress rewrites to \"hive unavailable\"", tc.name, status)
			}
			w := httptest.NewRecorder()
			delegatedGatherError(w, tc.err, now)
			if got := w.Header().Get("Retry-After"); (tc.retry > 0) != (got != "") {
				t.Fatalf("Retry-After = %q for wait %v", got, tc.retry)
			}
		})
	}
}

// The preview shows the server's reason; a 429 says when to retry, from
// the message or else Retry-After; the Ingress's own 502 page (HTML, not
// JSON) still reads "hive unavailable".
func TestStageTrackerDelegatePlanRateLimitMessage(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+stageTrackerConfirmOptOutJS(t, html)+`
(async () => {
  const item = { repo: 'projectbluefin/testsuite', kind: 'pr', number: 1026, url: 'u', stage: 85, modifiers: ['needs-decision'] };
  const req = stageTrackerActionRequest(item, 'decide:delegate', {});
  let reply;
  global.fetch = async () => reply;
  const hdr = h => ({ get: k => (k in h ? h[k] : null) });

  reply = { ok: false, status: 429, headers: hdr({ 'Retry-After': '1319' }), json: async () => ({ ok: false, error: 'GitHub API rate limit for this hive is exhausted; resets at 17:32Z' }) };
  let pl = await stageTrackerPostDelegatePlan(item, req, false);
  assert.strictEqual(pl.error, 'GitHub API rate limit for this hive is exhausted; resets at 17:32Z');
  const err = stageTrackerDelegatePlanErrorContent('projectbluefin/testsuite#1026', [pl]);
  assert.ok(err.includes('Could not preview projectbluefin/testsuite#1026 (GitHub API rate limit for this hive is exhausted; resets at 17:32Z)'), err);
  assert.ok(!err.includes('hive unavailable'), err);

  // A 429 whose message names no time: Retry-After does.
  const now = Date.parse('2026-11-06T17:10:07Z');
  assert.strictEqual(stageTrackerDelegatePlanErrorText(429, { error: 'slow down' }, '90', now), 'slow down; retry after 17:12Z');
  assert.strictEqual(stageTrackerDelegatePlanErrorText(429, {}, null, now), 'GitHub rate limit reached; retry in a minute');
  // The Ingress's canned 502: JSON or its HTML page.
  assert.strictEqual(stageTrackerDelegatePlanErrorText(502, { ok: false, error: 'hive unavailable' }, null, now), 'hive unavailable');
  assert.strictEqual(stageTrackerDelegatePlanErrorText(502, {}, null, now), 'hive unavailable');
  reply = { ok: false, status: 502, headers: hdr({}), json: async () => { throw new SyntaxError('Unexpected token <'); } };
  pl = await stageTrackerPostDelegatePlan(item, req, false);
  assert.strictEqual(pl.error, 'hive unavailable');
  // A 424 carries GitHub's own words.
  assert.strictEqual(stageTrackerDelegatePlanErrorText(424, { error: 'reading o/r#1: 404 Not Found' }, null, now), 'reading o/r#1: 404 Not Found');
})().catch(e => { console.error(e); process.exit(1); });
`)
}
