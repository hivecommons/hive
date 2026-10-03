package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// npsUIBlock returns the NPS prompt's section of index.html, from its banner
// comment to the next section's banner.
func npsUIBlock(t *testing.T) string {
	t.Helper()
	html := indexHTML(t)
	start := strings.Index(html, "// ── NPS feedback survey, #9610")
	end := strings.Index(html, "// ── Welcome / Getting Started dialog")
	if start < 0 || end < start {
		t.Fatalf("NPS prompt block not found in index.html (start=%d end=%d)", start, end)
	}
	return html[start:end]
}

// TestNPSEligibilityRules executes the dashboard's own eligibility helpers
// under node against the timing and backoff rules of the console's
// useNPSSurvey (first prompt no earlier than session 2 after 5 min engaged, 1
// min from session 3; 30d after a submission; 7d after a dismissal; 30d after
// 3 dismissals).
func TestNPSEligibilityRules(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH - the NPS eligibility rules were NOT executed by this run; the structure tests below still ran")
	}
	html := indexHTML(t)
	var b strings.Builder
	for _, fn := range []string{
		"npsTiming", "npsDefaultState", "npsNormalizeState", "npsDaysSince",
		"npsIsEligible", "npsRequiredEngagementMs", "npsHasEnoughUsage",
		"npsStateAfterSubmit", "npsStateAfterDismiss", "npsCategory", "npsFeedbackPrompt",
	} {
		b.WriteString(jsFunc(t, html, fn))
		b.WriteString("\n")
	}
	b.WriteString(npsEligibilityAssertions)
	path := filepath.Join(t.TempDir(), "nps_eligibility.js")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("NPS eligibility check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

const npsEligibilityAssertions = `
let fails = 0;
function check(name, cond) { if (!cond) { fails++; console.log('FAIL ' + name); } }
const T = npsTiming();
const DAY = T.MS_PER_DAY;
const MIN = 60 * 1000;
const now = Date.parse('2026-09-29T12:00:00Z');
const ago = d => new Date(now - d * DAY).toISOString();

// The constants are the console's.
check('min sessions is 2', T.MIN_SESSIONS_BEFORE_NPS === 2);
check('returning threshold is 3', T.RETURNING_USER_SESSION_THRESHOLD === 3);
check('2nd-session engagement is 5 min', T.SECOND_SESSION_MIN_ENGAGEMENT_MS === 5 * MIN);
check('returning engagement is 1 min', T.RETURNING_USER_MIN_ENGAGEMENT_MS === 1 * MIN);
check('reprompt is 30 days', T.REPROMPT_DAYS === 30);
check('dismiss retry is 7 days', T.DISMISS_RETRY_DAYS === 7);
check('max dismissals is 3', T.MAX_DISMISSALS === 3);

// Fresh and corrupt state.
check('fresh state is eligible', npsIsEligible(npsDefaultState(), now, T));
check('null state is eligible', npsIsEligible(null, now, T));
check('garbage state is eligible', npsIsEligible({ dismissCount: 'x', lastSubmittedAt: 42 }, now, T));

// Submission backoff.
check('submitted 29d ago is not eligible', !npsIsEligible({ lastSubmittedAt: ago(29) }, now, T));
check('submitted 31d ago is eligible', npsIsEligible({ lastSubmittedAt: ago(31) }, now, T));
check('unparseable submit date is not eligible', !npsIsEligible({ lastSubmittedAt: 'not a date' }, now, T));

// Dismissal backoff via the real transition.
let s = npsStateAfterDismiss(npsDefaultState(), now, T);
check('1st dismiss counts 1', s.dismissCount === 1);
check('1st dismiss snoozes 7d', Date.parse(s.snoozedUntil) === now + 7 * DAY);
check('1st dismiss does not mark max', s.maxDismissalsReachedAt === null);
check('6 days after dismiss is not eligible', !npsIsEligible(s, now + 6 * DAY, T));
check('8 days after dismiss is eligible', npsIsEligible(s, now + 8 * DAY, T));
check('legacy state with only lastDismissedAt 3d ago is not eligible', !npsIsEligible({ lastDismissedAt: ago(3) }, now, T));

s = npsStateAfterDismiss(s, now + 8 * DAY, T);
s = npsStateAfterDismiss(s, now + 16 * DAY, T);
check('3rd dismiss counts 3', s.dismissCount === 3);
check('3rd dismiss marks max', s.maxDismissalsReachedAt === new Date(now + 16 * DAY).toISOString());
check('after 3 dismissals, 8d later is still not eligible', !npsIsEligible(s, now + 24 * DAY, T));
check('after 3 dismissals, 31d later is eligible', npsIsEligible(s, now + 47 * DAY, T));

// Submission resets the dismissal ladder.
const sub = npsStateAfterSubmit(now);
check('submit records time', sub.lastSubmittedAt === new Date(now).toISOString());
check('submit resets dismissals', sub.dismissCount === 0 && sub.maxDismissalsReachedAt === null && sub.snoozedUntil === null);
check('just submitted is not eligible', !npsIsEligible(sub, now + DAY, T));

// Operator overrides (hub.nps_timing, via the status response's timing).
const O = npsTiming({ min_sessions: 1, second_session_min_engagement_ms: 2 * MIN, returning_min_engagement_ms: 30 * 1000,
  reprompt_days: 90, dismiss_retry_days: 14, max_dismissals: 5 });
check('override min sessions', O.MIN_SESSIONS_BEFORE_NPS === 1);
check('override 2nd-session engagement', O.SECOND_SESSION_MIN_ENGAGEMENT_MS === 2 * MIN);
check('override returning engagement', O.RETURNING_USER_MIN_ENGAGEMENT_MS === 30 * 1000);
check('override reprompt', O.REPROMPT_DAYS === 90);
check('override dismiss retry', O.DISMISS_RETRY_DAYS === 14);
check('override max dismissals', O.MAX_DISMISSALS === 5);
check('overrides drive eligibility: 31d after submit is NOT eligible at 90d', !npsIsEligible({ lastSubmittedAt: ago(31) }, now, O));
check('overrides drive eligibility: 91d after submit is eligible', npsIsEligible({ lastSubmittedAt: ago(91) }, now, O));
check('overrides drive dismissal snooze', Date.parse(npsStateAfterDismiss(npsDefaultState(), now, O).snoozedUntil) === now + 14 * DAY);
const bad = npsTiming({ min_sessions: 0, reprompt_days: -3, dismiss_retry_days: 2.5, max_dismissals: '9', returning_min_engagement_ms: null });
check('invalid overrides keep the constants',
  bad.MIN_SESSIONS_BEFORE_NPS === 2 && bad.REPROMPT_DAYS === 30 && bad.DISMISS_RETRY_DAYS === 7 &&
  bad.MAX_DISMISSALS === 3 && bad.RETURNING_USER_MIN_ENGAGEMENT_MS === 1 * MIN);
check('non-object overrides keep the constants', npsTiming('x').REPROMPT_DAYS === 30 && npsTiming(null).MAX_DISMISSALS === 3);

// Engagement.
check('session 1 never qualifies', !npsHasEnoughUsage(1, 60 * MIN, T));
check('session 2 needs 5 min (4 is not enough)', !npsHasEnoughUsage(2, 4 * MIN, T));
check('session 2 with 5 min qualifies', npsHasEnoughUsage(2, 5 * MIN, T));
check('session 3 with 1 min qualifies', npsHasEnoughUsage(3, 1 * MIN, T));
check('session 3 with 59s does not qualify', !npsHasEnoughUsage(3, 59 * 1000, T));

// Categories and follow-up prompts match the hub's bucketing.
check('1 is detractor', npsCategory(1) === 'detractor');
check('2 is passive', npsCategory(2) === 'passive');
check('3 is passive', npsCategory(3) === 'passive');
check('4 is promoter', npsCategory(4) === 'promoter');
check('prompts differ by category',
  new Set([npsFeedbackPrompt('detractor'), npsFeedbackPrompt('passive'), npsFeedbackPrompt('promoter')]).size === 3);

if (fails) { console.log(fails + ' NPS eligibility assertion(s) failed'); process.exit(1); }
`

// TestNPSPromptWiring pins that the prompt is started, gated on the server's
// can_submit, and posts to the spoke's own endpoint.
func TestNPSPromptWiring(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "    initNPSSurvey();\n") {
		t.Error("initNPSSurvey() is never called - the prompt is dead code")
	}
	block := npsUIBlock(t)
	for _, want := range []string{
		"fetch('/api/feedback/nps/status')",
		"status.can_submit !== true",
		"fetch('/api/feedback/nps', {",
		"if (_npsShownThisLoad || _npsCard) return;",
		"dismissNPSSurvey",
		"nps-survey-card",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("NPS block is missing %q", want)
		}
	}
}

// TestNPSPromptRendersTextOnly is the stored-XSS guard (console #17030): the
// prompt never uses a markup sink.
func TestNPSPromptRendersTextOnly(t *testing.T) {
	block := npsUIBlock(t)
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(block, sink) {
			t.Errorf("NPS block uses %s - render text with textContent only", sink)
		}
	}
}

// TestNPSStorageAccessIsGuarded: every localStorage/sessionStorage access in
// the NPS block lives inside one of the wrapper helpers, and each wrapper
// wraps its body in try/catch (private windows and blocked storage throw).
func TestNPSStorageAccessIsGuarded(t *testing.T) {
	html := indexHTML(t)
	wrappers := []string{"npsLoadState", "npsSaveState", "npsBumpSessionCount", "npsEngagedMs", "npsAddEngagedMs",
		"npsAnalyticsOptedOut", "npsSetAnalyticsOptOut"}
	rest := npsUIBlock(t)
	for _, fn := range wrappers {
		body := jsFunc(t, html, fn)
		if !strings.Contains(body, "try {") || !strings.Contains(body, "catch (e)") {
			t.Errorf("%s() touches storage without try/catch", fn)
		}
		rest = strings.Replace(rest, body, "", 1)
	}
	if regexp.MustCompile(`\b(localStorage|sessionStorage)\.`).MatchString(rest) {
		t.Error("the NPS block accesses storage outside the guarded wrapper helpers")
	}
}

// npsJSConst returns the full source line declaring const name in index.html,
// so the node harness runs the dashboard's own constants, not a copy.
func npsJSConst(t *testing.T, html, name string) string {
	t.Helper()
	start := strings.Index(html, "const "+name+" =")
	if start < 0 {
		t.Fatalf("index.html does not declare const %s", name)
	}
	end := strings.Index(html[start:], "\n")
	if end < 0 {
		t.Fatalf("const %s runs to end of file", name)
	}
	return html[start : start+end]
}

// npsRunNode executes script under node, or skips loudly when node is absent.
func npsRunNode(t *testing.T, what, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH - " + what + " was NOT executed by this run; the structure tests still ran")
	}
	path := filepath.Join(t.TempDir(), "nps_check.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("%s failed:\n%s", what, strings.TrimSpace(string(out)))
	}
}

// TestNPSAnalyticsGatedAndTextFree executes the dashboard's own GA4 helpers
// under node with a fake window/document/localStorage and proves:
//   - with no measurement ID nothing loads: no script element, no dataLayer,
//     no gtag, and npsTrack reports it sent nothing;
//   - a malformed ID, the per-browser opt-out, Global Privacy Control, Do Not
//     Track and unreadable storage each suppress every event the same way;
//   - with an ID, gtag.js loads once from Google's tag host with page views,
//     signals and ad personalization off, and the event payloads carry the
//     score, category and feedback LENGTH but never the free text.
func TestNPSAnalyticsGatedAndTextFree(t *testing.T) {
	html := indexHTML(t)
	var b strings.Builder
	b.WriteString(npsAnalyticsHarnessPrelude)
	for _, c := range []string{"NPS_ANALYTICS_OPT_OUT_LS_KEY", "NPS_GA4_ID_RE", "NPS_GTAG_SRC"} {
		b.WriteString(npsJSConst(t, html, c))
		b.WriteString("\n")
	}
	b.WriteString("let _npsGtagId = '';\n")
	for _, fn := range []string{
		"npsCategory", "npsGA4MeasurementID", "npsAnalyticsParams", "npsAnalyticsOptedOut",
		"npsSetAnalyticsOptOut", "npsEnsureGtag", "npsTrack",
	} {
		b.WriteString(jsFunc(t, html, fn))
		b.WriteString("\n")
	}
	b.WriteString(npsAnalyticsAssertions)
	npsRunNode(t, "the NPS GA4 gating checks", b.String())
}

const npsAnalyticsHarnessPrelude = `
const appended = [];
const store = {};
let storageThrows = false;
const localStorage = {
  getItem(k) { if (storageThrows) throw new Error('blocked'); return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
  setItem(k, v) { if (storageThrows) throw new Error('blocked'); store[k] = String(v); },
  removeItem(k) { if (storageThrows) throw new Error('blocked'); delete store[k]; },
};
const window = {};
const document = {
  createElement(tag) { return { tagName: tag }; },
  head: { appendChild(el) { appended.push(el); } },
};
// Node 21+ has a built-in read-only navigator; replace it with a plain object.
Object.defineProperty(globalThis, 'navigator', { value: {}, writable: true, configurable: true });
`

const npsAnalyticsAssertions = `
let fails = 0;
function check(name, cond) { if (!cond) { fails++; console.log('FAIL ' + name); } }
const SECRET = 'my secret complaint about @octocat and https://example.com/private';
const ID = 'G-TESTABC123';
function nothingLoaded(label) {
  check(label + ': no script appended', appended.length === 0);
  check(label + ': no dataLayer', window.dataLayer === undefined);
  check(label + ': no gtag', window.gtag === undefined);
}
function everyEventRefused(status, label) {
  check(label + ': shown refused', npsTrack(status, 'hive_nps_survey_shown', {}) === false);
  check(label + ': response refused', npsTrack(status, 'hive_nps_response', npsAnalyticsParams(1, SECRET)) === false);
  check(label + ': dismissed refused', npsTrack(status, 'hive_nps_dismissed', {}) === false);
  nothingLoaded(label);
}

// The payload builder never carries text.
const p = npsAnalyticsParams(1, '  ' + SECRET + '  ');
check('params carry score', p.score === 1);
check('params carry category', p.category === 'detractor');
check('params carry trimmed length', p.feedback_length === SECRET.length);
check('params carry nothing else', Object.keys(p).sort().join(',') === 'category,feedback_length,score');
check('params never contain the text', !JSON.stringify(p).includes('secret'));
check('no text is length 0', npsAnalyticsParams(4, undefined).feedback_length === 0);

// Off by default and under every opt-out.
everyEventRefused(null, 'no status');
everyEventRefused({ can_submit: true }, 'no ID');
everyEventRefused({ ga4_measurement_id: '' }, 'empty ID');
everyEventRefused({ ga4_measurement_id: 'UA-1234-1' }, 'malformed ID');
everyEventRefused({ ga4_measurement_id: 'G-ABC"><script>' }, 'hostile ID');

npsSetAnalyticsOptOut(true);
check('opt-out recorded', npsAnalyticsOptedOut() === true);
everyEventRefused({ ga4_measurement_id: ID }, 'opted out');
npsSetAnalyticsOptOut(false);
check('opt-out cleared', npsAnalyticsOptedOut() === false);

navigator.globalPrivacyControl = true;
everyEventRefused({ ga4_measurement_id: ID }, 'GPC');
delete navigator.globalPrivacyControl;
navigator.doNotTrack = '1';
everyEventRefused({ ga4_measurement_id: ID }, 'DNT');
delete navigator.doNotTrack;
storageThrows = true;
everyEventRefused({ ga4_measurement_id: ID }, 'storage blocked');
storageThrows = false;

// Enabled: loads once, sends the three events, never the text.
const status = { ga4_measurement_id: ID };
check('shown sent', npsTrack(status, 'hive_nps_survey_shown', {}) === true);
check('response sent', npsTrack(status, 'hive_nps_response', npsAnalyticsParams(1, SECRET)) === true);
check('dismissed sent', npsTrack(status, 'hive_nps_dismissed', {}) === true);
check('gtag.js loaded exactly once', appended.length === 1);
check('gtag.js from Google tag host with the ID', appended[0] && appended[0].src === 'https://www.googletagmanager.com/gtag/js?id=' + ID);
check('gtag.js async', appended[0] && appended[0].async === true);
const calls = (window.dataLayer || []).map(a => Array.from(a));
const cfg = calls.find(c => c[0] === 'config');
check('config targets the ID', cfg && cfg[1] === ID);
check('no automatic page views', cfg && cfg[2].send_page_view === false);
check('no google signals', cfg && cfg[2].allow_google_signals === false);
check('no ad personalization', cfg && cfg[2].allow_ad_personalization_signals === false);
const events = calls.filter(c => c[0] === 'event').map(c => c[1]);
check('exactly the three events', events.join(',') === 'hive_nps_survey_shown,hive_nps_response,hive_nps_dismissed');
const wire = JSON.stringify(calls);
check('no free text in any payload', !wire.includes('secret') && !wire.includes('octocat') && !wire.includes('example.com'));
check('length did go out', wire.includes('"feedback_length":' + SECRET.length));

if (fails) { console.log(fails + ' NPS analytics assertion(s) failed'); process.exit(1); }
`

// TestNPSAnalyticsCallSites is the structural half: gtag is only ever called
// from the two gated helpers, gtag.js is only ever referenced through
// NPS_GTAG_SRC, every event goes through npsTrack with either an empty
// payload or npsAnalyticsParams (which cannot carry text), and nothing uses
// sendBeacon.
func TestNPSAnalyticsCallSites(t *testing.T) {
	html := indexHTML(t)
	block := npsUIBlock(t)
	rest := block
	for _, fn := range []string{"npsEnsureGtag", "npsTrack"} {
		rest = strings.Replace(rest, jsFunc(t, html, fn), "", 1)
	}
	if strings.Contains(rest, "gtag(") || strings.Contains(rest, "dataLayer") {
		t.Error("the NPS block calls gtag/dataLayer outside npsEnsureGtag/npsTrack")
	}
	if strings.Contains(block, "sendBeacon") {
		t.Error("the NPS block uses sendBeacon")
	}
	if n := strings.Count(block, "googletagmanager"); n != 1 {
		t.Errorf("googletagmanager appears %d times in the NPS block, want once (NPS_GTAG_SRC)", n)
	}
	if strings.Count(html, "googletagmanager") != 1 {
		t.Error("googletagmanager is referenced outside the NPS block")
	}

	callRe := regexp.MustCompile(`npsTrack\(([^,]+), '([a-z_]+)', ([^;]*)\);`)
	calls := callRe.FindAllStringSubmatch(block, -1)
	if total := strings.Count(block, "npsTrack(") - 1; len(calls) != total {
		t.Fatalf("parsed %d npsTrack call sites but found %d; keep each call on one line", len(calls), total)
	}
	events := map[string]bool{}
	for _, c := range calls {
		events[c[2]] = true
		if payload := strings.TrimSpace(c[3]); payload != "{}" && !strings.HasPrefix(payload, "npsAnalyticsParams(") {
			t.Errorf("event %s sends payload %q; use {} or npsAnalyticsParams(...)", c[2], payload)
		}
	}
	for _, want := range []string{"hive_nps_survey_shown", "hive_nps_response", "hive_nps_dismissed"} {
		if !events[want] {
			t.Errorf("event %s is never sent", want)
		}
	}
	if len(events) != 3 {
		t.Errorf("unexpected event set %v", events)
	}
}

// TestNPSDetractorIssueRequestRules executes the dashboard's own helpers for
// the opt-in detractor issue: nothing is requested unless the hive opted in,
// the score is the detractor score, consent is exactly true, and the text
// reaches the minimum length.
func TestNPSDetractorIssueRequestRules(t *testing.T) {
	html := indexHTML(t)
	var b strings.Builder
	for _, fn := range []string{"npsDetractorIssueAllowed", "npsIssueRequestBody"} {
		b.WriteString(jsFunc(t, html, fn))
		b.WriteString("\n")
	}
	b.WriteString(npsDetractorAssertions)
	npsRunNode(t, "the NPS detractor-issue rules", b.String())

	block := npsUIBlock(t)
	for _, want := range []string{
		"fetch('/api/feedback/nps/issue', {",
		"consent.type = 'checkbox';",
		"npsIssueRequestBody(status, selected, feedbackText, true)",
		"if (issueBody) npsOpenIssue(issueBody);",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("NPS block is missing %q", want)
		}
	}
	if strings.Contains(block, "consent.checked = true") {
		t.Error("the consent box must never be ticked by code")
	}
}

const npsDetractorAssertions = `
let fails = 0;
function check(name, cond) { if (!cond) { fails++; console.log('FAIL ' + name); } }
const on = { detractor_issue: { enabled: true, score: 1, min_feedback_chars: 20, repo: 'acme/widgets' } };
const long = 'The queue view never loads for our org.';
check('off when status lacks it', npsIssueRequestBody({}, 1, long, true) === null);
check('off when disabled', npsIssueRequestBody({ detractor_issue: { enabled: false, score: 1, min_feedback_chars: 20 } }, 1, long, true) === null);
check('off for null status', npsIssueRequestBody(null, 1, long, true) === null);
check('only the detractor score', npsIssueRequestBody(on, 2, long, true) === null && npsIssueRequestBody(on, 4, long, true) === null);
check('consent must be exactly true', npsIssueRequestBody(on, 1, long, false) === null && npsIssueRequestBody(on, 1, long, 'true') === null && npsIssueRequestBody(on, 1, long, undefined) === null);
check('too short', npsIssueRequestBody(on, 1, 'x'.repeat(19), true) === null);
check('short once trimmed', npsIssueRequestBody(on, 1, '   ' + 'x'.repeat(19) + '   ', true) === null);
const ok = npsIssueRequestBody(on, 1, '  ' + 'x'.repeat(20) + '  ', true);
check('exactly the minimum is accepted', ok !== null && ok.feedback === 'x'.repeat(20) && ok.score === 1 && ok.consent === true);
check('allowed only for detractor', npsDetractorIssueAllowed(on, 1) && !npsDetractorIssueAllowed(on, 2) && !npsDetractorIssueAllowed(null, 1));
if (fails) { console.log(fails + ' NPS detractor assertion(s) failed'); process.exit(1); }
`
