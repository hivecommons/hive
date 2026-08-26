package dashboard

import (
	"strings"
	"testing"
)

// A dashboard token that the server REJECTS must not be a permanent, silent
// dead end.
//
// The SPA's fetch wrapper stores the token in localStorage and attaches it to
// every mutation. It used to prompt only when it had NO token, and it never
// looked at the response — so once the operator rotated HIVE_DASHBOARD_TOKEN,
// a browser holding the old value 401'd on every write forever, with no
// prompt and no error. Each feature surfaced that differently (a "Failed:
// Unauthorized" toast, a Re-check button that did nothing, a confirm dialog
// that closed as if it had worked), so it read as several unrelated broken
// features rather than one bad credential. Measured on a live hive: writes
// failed for a full day across four features after a rotation.
//
// These tests pin the structure of the recovery path in the embedded
// static/index.html. The browser behavior itself (localStorage, window.prompt,
// a real 401) cannot execute here; what is honestly guaranteed is that the
// recovery branch, its single-retry guard, and the stored-token precondition
// survive future edits — matching the convention of the other index.html
// structure tests in this package.

// TestStaleTokenTriggersRecovery asserts the wrapper reacts to a rejected
// token at all: it inspects mutation responses for 401/403, drops the stored
// value, and re-prompts. Losing any piece restores the silent-failure bug.
func TestStaleTokenTriggersRecovery(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"res.status === 401 || res.status === 403",
		"localStorage.removeItem('hive-token')",
		"_authRejected = true;",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — a rotated dashboard token silently fails every write again", snippet)
		}
	}
}

// TestStaleTokenRetriesExactlyOnce asserts the retry is guarded. A token that
// is simply WRONG (operator pastes garbage) must fail and stop, not loop
// prompting forever.
func TestStaleTokenRetriesExactlyOnce(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"!opts.__hiveAuthRetried",
		"__hiveAuthRetried: true",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — the auth retry is unguarded and can loop", snippet)
		}
	}
}

// TestStaleTokenRecoveryRequiresAStoredToken asserts the recovery only runs
// when a token was actually SENT. On a hosted/hub-proxied hive identity comes
// from the hub login and no token exists: a 401/403 there is a genuine
// authorization denial, and clearing storage or prompting for a token the
// operator was never given would be wrong.
func TestStaleTokenRecoveryRequiresAStoredToken(t *testing.T) {
	html := indexHTML(t)
	const guard = "if (isMutation && _authToken && !opts.__hiveAuthRetried &&"
	if !strings.Contains(html, guard) {
		t.Errorf("index.html is missing the %q guard — hosted hives would clear/prompt on a real authorization denial", guard)
	}
}

// TestStaleTokenPromptExplainsItself asserts the re-prompt says WHY it is
// asking. The first-run prompt and the rejected-token prompt are the same
// dialog otherwise, and an operator who sees the generic text again has no
// way to know their saved token was the problem.
func TestStaleTokenPromptExplainsItself(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "was rejected") {
		t.Error("index.html: the re-prompt does not tell the operator the stored token was rejected")
	}
	if !strings.Contains(html, "_authRejected\n") && !strings.Contains(html, "_authRejected ?") {
		t.Error("index.html: the prompt text is not conditioned on _authRejected")
	}
}
