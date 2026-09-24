package agent

import (
	"os"
	"regexp"
	"strings"
	"time"
)

// loginPromptPatterns are substrings that indicate an agent is stuck on a
// login/authentication screen (Copilot text prompts, Claude Code OAuth flow,
// GitHub device flow). Each must be distinctive enough to never appear in
// ordinary agent output.
var loginPromptPatterns = []string{
	// A BARE "/login" is deliberately NOT here — it is handled separately by
	// lineHasLoginDirective below. "/login" alone is a substring of ordinary
	// agent output (an agent reviewing an auth route writes "POST /login"; a
	// CLI printing its slash-command list renders "/login" beside "/help"), and
	// matching it painted the 🔑 badge on agents that were authenticated and
	// mid-work.
	"sign in to use",
	"Sign in to use",
	"authenticate to use",
	"Authenticate to use",
	"log in to use",
	"Log in to use",
	// Claude Code OAuth sign-in screen
	"Use the url below to sign in",
	"Paste code here if prompted",
	"Select login method",
	"/cai/oauth/authorize",
	"claude.ai/oauth/authorize",
	// GitHub device-flow screen (Copilot CLI)
	"Enter one-time code",
	"github.com/login/device",
	// Codex browser sign-in / device-code hand-off
	"Finish signing in via your browser",
	"auth.openai.com/oauth/authorize",
	"On a remote or headless machine? Press esc and choose Sign in with Device Code.",
	// Antigravity OAuth hand-off
	"accounts.google.com/o/oauth2/auth",
	"If you aren't automatically redirected, paste the authorization code below:",
	// OMP OAuth hand-off
	"Complete login in your browser. If the browser cannot reach this machine",
	"Paste the authorization code (or full redirect URL), then press Enter:",
}

// fatalNetworkErrorPatterns are substrings that indicate a transient TLS or
// network failure killed the agent at startup. These errors leave the Copilot
// chrome visible (❯, / commands) so paneShowsCLIReady returns true, but the
// agent is dead and will never recover without a restart.
var fatalNetworkErrorPatterns = []string{
	"invalid peer certificate",
	"BadSignature",
	"fetch failed",
}

// paneShowsFatalNetworkError returns true if any line contains a fatal
// TLS/network error pattern that requires an agent restart.
func paneShowsFatalNetworkError(lines []string) bool {
	for _, line := range lines {
		for _, pat := range fatalNetworkErrorPatterns {
			if strings.Contains(line, pat) {
				return true
			}
		}
	}
	return false
}

// agentIsProducing reports whether an agent is still rendering output, and so
// vetoes the fatal-network restart in the poll loop.
//
// paneShowsFatalNetworkError matches on SCROLLBACK, so a hit may be an error
// the agent already recovered from and worked past — the same lesson
// transientAPIErrorPatterns documents below, which the fatal detector never
// learned. That detector's premise is that the agent is "visually ready but
// actually dead"; an agent whose pane changed moments ago is demonstrably
// alive, which refutes the premise. Restarting it does not recover anything,
// it destroys the turn in flight.
//
// Observed in production on the projectbluefin spoke: the reviewer emitted a
// generic "fetch failed" from one failed proxied call mid-turn, kept working,
// and was killed while streaming 22KB of output. It restarted 52 times and
// published no review for hours, because every restart landed mid-turn and
// the next turn met the same line still sitting in scrollback.
//
// Startup death is deliberately still caught: an agent that dies before
// producing anything has a static pane, so lastPaneChange does not advance
// and this returns false.
//
// A zero lastPaneChange means the pane has never been observed to change, so
// there is nothing to protect and the restart proceeds.
func agentIsProducing(lastPaneChange, now time.Time) bool {
	if lastPaneChange.IsZero() {
		return false
	}
	return now.Sub(lastPaneChange) < fatalNetworkProducingGraceSec*time.Second
}

// transientAPIErrorPatterns are substrings of API failures that a plain retry
// fixes (#4697). They are the OPPOSITE of fatalNetworkErrorPatterns above: the
// CLI survives, drops back to its idle prompt with the response truncated, and
// stays there until the next scheduled kick — which can be hours away. The
// session is alive with full context, so the remedy is a nudge, not a restart.
//
// Membership is deliberately narrow. Every pattern here must be an error where
// REPEATING THE SAME REQUEST CAN SUCCEED:
//
//   - a dropped/timed-out connection — the request never completed;
//   - 5xx and "overloaded" — the upstream failed this attempt, not this caller.
//
// Errors a retry cannot fix must NOT be listed, because nudging them loops the
// agent against a wall: 403/model-refusal (#4400) and quota exhaustion (#4583)
// are both excluded here AND re-checked at the call site via
// lineShowsUpstreamAuthorizationError / paneShowsQuotaExhausted, since Claude
// Code renders every API failure under the same "API Error:" prefix and a
// substring match alone cannot tell them apart.
var transientAPIErrorPatterns = []string{
	// The shape reported in #4697, observed repeatedly on a claude-backend
	// agent: the response is cut off mid-stream and the CLI returns to ❯.
	"connection lost mid-response",
	// Claude Code 2.1.x says "closed" where older builds said "lost"
	// (hivecommons/hive#7855). Same failure, same remedy.
	"connection closed mid-response",
	// Newer Claude Code wording for the same cut-off-mid-stream failure:
	// "API Error: Response stalled mid-stream. The response above may be
	// incomplete." Same remedy — the request never completed, so repeating
	// it can succeed.
	"stalled mid-stream",
	"connection error",
	"request timed out",
	"overloaded_error",
}

// 500/502/503/529 are retryable upstream failures; match them as whole tokens
// only, so unrelated request IDs or token counts under the same API-error chrome
// do not trip the watchdog.
var transientAPIErrorStatusRe = regexp.MustCompile(`\b(?:500|502|503|529)\b`)

// paneShowsTransientAPIError reports whether any line carries a retryable API
// failure. Pure over its input so the decision is table-testable without tmux;
// the call site supplies the VISIBLE pane tail rather than scrollback, so an
// error the agent already recovered from does not read as current.
//
// Two CLI shapes are recognized, because the two backends render failures
// differently and matching only one silently excluded the other: Claude Code
// prefixes with "API Error:", while the Copilot CLI reports
// copilotModelFailureMarker with a transport cause appended.
func paneShowsTransientAPIError(lines []string) bool {
	for _, line := range lines {
		lower := strings.ToLower(line)

		if strings.Contains(lower, copilotModelFailureMarker) {
			for _, pat := range copilotTransportErrorPatterns {
				if strings.Contains(lower, pat) {
					return true
				}
			}
			// A copilot model failure with no transport cause is not
			// assumed retryable; fall through so the Claude-shaped
			// checks below still get a chance on the same line.
		}

		if !strings.Contains(lower, "api error:") {
			continue
		}
		for _, pat := range transientAPIErrorPatterns {
			if strings.Contains(lower, pat) {
				return true
			}
		}
		if transientAPIErrorStatusRe.MatchString(line) {
			return true
		}
	}
	return false
}

type providerErrorMatch struct {
	Class string
	Line  string
}

var (
	providerAPIErrorStatusRe = regexp.MustCompile(`(?i)\bAPI Error:\s*(\d{3})\b`)
	providerRetryingRe       = regexp.MustCompile(`(?i)\bRetrying in \d+s\s+·\s+attempt \d+/\d+\b`)
	providerHTTPStatusRe     = regexp.MustCompile(`\b(401|403|429|500|502|503|529)\b`)
)

func providerErrorBackoffBase() time.Duration {
	if v := os.Getenv(ProviderErrorBackoffBaseEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return providerErrorBackoffBaseDefault
}

func providerErrorBackoffMax() time.Duration {
	if v := os.Getenv(ProviderErrorBackoffMaxEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return providerErrorBackoffMaxDefault
}

func providerErrorBackoffDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := providerErrorBackoffBase()
	maxDelay := providerErrorBackoffMax()
	delay := base
	for i := 1; i < attempt && delay < maxDelay; i++ {
		delay *= 2
		if delay > maxDelay {
			return maxDelay
		}
	}
	return delay
}

func classifyProviderError(pane string) (providerErrorMatch, bool) {
	for _, line := range strings.Split(stripExplainLines(pane), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		switch {
		case strings.Contains(lower, "insufficient_quota"):
			return providerErrorMatch{Class: "quota", Line: trimmed}, true
		case strings.Contains(lower, "not licensed to use copilot"):
			return providerErrorMatch{Class: "auth", Line: trimmed}, true
		case strings.Contains(lower, "rate_limit") ||
			((strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests")) && providerLineHasAPIContext(lower)):
			return providerErrorMatch{Class: "rate_limit", Line: trimmed}, true
		case strings.Contains(lower, "overloaded_error") ||
			(strings.Contains(lower, "overloaded") && providerLineHasAPIContext(lower)):
			return providerErrorMatch{Class: "overloaded", Line: trimmed}, true
		case strings.Contains(lower, `"type":"api_error"`) || strings.Contains(lower, `"type": "api_error"`) ||
			strings.Contains(lower, "inference backend unreachable"):
			return providerErrorMatch{Class: "api_error", Line: trimmed}, true
		case providerRetryingRe.MatchString(trimmed):
			return providerErrorMatch{Class: "retrying", Line: trimmed}, true
		}
		if m := providerAPIErrorStatusRe.FindStringSubmatch(trimmed); len(m) == 2 {
			return providerErrorMatch{Class: providerErrorStatusClass(m[1]), Line: trimmed}, true
		}
		if providerLineHasAPIContext(lower) {
			if m := providerHTTPStatusRe.FindStringSubmatch(trimmed); len(m) == 2 {
				return providerErrorMatch{Class: providerErrorStatusClass(m[1]), Line: trimmed}, true
			}
		}
	}
	return providerErrorMatch{}, false
}

func providerLineHasAPIContext(lower string) bool {
	return strings.Contains(lower, "api error") ||
		strings.Contains(lower, "api_error") ||
		strings.Contains(lower, "inference") ||
		strings.Contains(lower, "backend") ||
		strings.Contains(lower, "quota") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "forbidden")
}

func providerErrorStatusClass(status string) string {
	switch status {
	case "401", "403":
		return "auth"
	case "429":
		return "rate_limit"
	case "529":
		return "overloaded"
	default:
		return "api_error"
	}
}

// cliReadyIndicators prove copilot finished startup.
var cliReadyIndicators = []string{
	"❯",
	"/ commands",
	"? help",
	"/login",
	"sign in",
	"Sign in",
	"Copilot v",
	"Tip: /init",
	"Loading:",
	"● Loading",
}

// paneShowsCLIReady returns true if the pane shows any indicator that
// copilot finished initializing (prompt, help text, or login request).
func paneShowsCLIReady(lines []string) bool {
	for _, line := range lines {
		for _, ind := range cliReadyIndicators {
			if strings.Contains(line, ind) {
				return true
			}
		}
	}
	return false
}

// paneShowsLoginPrompt returns true if any line in the pane output matches a
// known login/authentication prompt pattern.
// loginDirectiveVerbs are the imperative words a CLI uses when it is TELLING
// the operator to authenticate ("Please /login to continue", "Run /login",
// "Type /login to sign in"). A line containing "/login" counts as a login
// prompt only when one of these also appears, which is what separates a real
// login screen from an agent discussing an auth route ("POST /login returns
// 302") or a CLI listing its slash commands ("/help  /login  /model").
var loginDirectiveVerbs = []string{
	"please", "run", "type", "use", "enter", "try", "must", "need",
}

// lineHasLoginDirective reports whether a line both mentions "/login" AND
// carries an imperative that makes it a directive to the operator.
func lineHasLoginDirective(line string) bool {
	if !strings.Contains(line, "/login") {
		return false
	}
	lower := strings.ToLower(line)
	for _, verb := range loginDirectiveVerbs {
		if strings.Contains(lower, verb) {
			return true
		}
	}
	return false
}

// modelRefusalPatterns are upstream MODEL-ENTITLEMENT refusals: the caller is
// authenticated, and the model it asked for is not one this account may use.
// Observed verbatim from a LiteLLM gateway in #4400:
//
//	team not allowed to access model. This team can only access
//	models=['gemini-2.5-pro', ..., 'aws/claude-sonnet-4-6', ...]
//
// Kept as text as well as the status check below because not every gateway
// surfaces an HTTP status through the CLI's error line.
var modelRefusalPatterns = []string{
	"not allowed to access model",
	"team not allowed to access",
}

// lineShowsUpstreamAuthorizationError reports whether a line carries an
// upstream failure that LOGGING IN CANNOT FIX (#4400).
//
// The distinction is the HTTP status, and it is not a nicety:
//
//	401  authentication — the caller is not identified. /login is the fix.
//	403  authorization  — the caller IS identified and is not permitted.
//	                      /login changes nothing; the request itself is the
//	                      problem.
//
// Keying on the status rather than on one gateway's wording keeps this working
// for gateways that phrase the refusal differently, and keeps 401 — a genuine
// logged-out signal — detected exactly as before.
func lineShowsUpstreamAuthorizationError(line string) bool {
	if strings.Contains(line, "API Error: 403") {
		return true
	}
	lower := strings.ToLower(line)
	for _, pat := range modelRefusalPatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

var quotaExhaustionPatterns = []string{
	"exceeded your monthly quota",
	"used all your copilot free chat requests",
	"individual quota reached",
	"budget_exceeded",
	"budget has been exceeded",
	"provider spending limit reached",
	"refused the request on a spending limit",
	"gone over your budget allowance",
	"bobcoins",
}

var quotaExhaustionStatusPattern = regexp.MustCompile(`\b\d+(?:\.\d+)?/\d+(?:\.\d+)?\s*\(0%\)\s*\|`)

func paneShowsQuotaExhausted(lines []string) bool {
	for _, line := range lines {
		lower := strings.ToLower(line)
		for _, pat := range quotaExhaustionPatterns {
			if strings.Contains(lower, pat) {
				return true
			}
		}
		if quotaExhaustionStatusPattern.MatchString(line) {
			return true
		}
	}
	return false
}

func paneShowsBobAPIKeyRejected(lines []string) bool {
	for _, line := range lines {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "api key") || !strings.Contains(lower, "401") {
			continue
		}
		if strings.Contains(lower, "verification failed") ||
			strings.Contains(lower, "invalid or expired api key") ||
			strings.Contains(lower, `"error":"unauthorized"`) ||
			strings.Contains(lower, `"error": "unauthorized"`) {
			return true
		}
	}
	return false
}

func paneShowsLoginPrompt(lines []string) bool {
	for _, line := range lines {
		// An upstream authorization failure is not a login prompt, whatever
		// the CLI decorated it with.
		if lineShowsUpstreamAuthorizationError(line) || paneShowsQuotaExhausted([]string{line}) {
			continue
		}
		if lineHasLoginDirective(line) {
			return true
		}
		for _, pat := range loginPromptPatterns {
			if strings.Contains(line, pat) {
				return true
			}
		}
	}
	return false
}
