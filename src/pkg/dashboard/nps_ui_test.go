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
	start := strings.Index(html, "// ── NPS feedback prompt (#9610)")
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
		"e.key === 'Escape'",
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
	wrappers := []string{"npsLoadState", "npsSaveState", "npsBumpSessionCount", "npsEngagedMs", "npsAddEngagedMs"}
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

// TestNPSPromptSendsNoAnalytics: this dashboard has no GA4 wiring (the
// hive_nps_* events are deferred, see src/docs/nps.md). Pin that the prompt
// does not grow an analytics call that could carry free text off-box without
// that design being reviewed.
func TestNPSPromptSendsNoAnalytics(t *testing.T) {
	block := npsUIBlock(t)
	for _, call := range []string{"gtag(", "dataLayer", "sendBeacon", "google-analytics", "googletagmanager"} {
		if strings.Contains(block, call) {
			t.Errorf("NPS block contains analytics call %q", call)
		}
	}
}
