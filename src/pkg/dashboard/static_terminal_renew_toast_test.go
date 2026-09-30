package dashboard

import (
	"os"
	"strings"
	"testing"
)

// TestTerminalRenewalDoesNotToastWhenHandoffSucceeds pins the fix for the
// false "Sign in to this hive before opening a terminal." toast: on a hosted
// spoke a user signed in only through hub SSO has no spoke-local hive_session,
// so /api/terminal/assertion/renew answers 401 while the handoff that follows
// mints the assertion and the terminal opens. The renewal must therefore
// return its verdict instead of toasting it, and openTerminal must surface
// that verdict only after the handoff has failed too.
func TestTerminalRenewalDoesNotToastWhenHandoffSucceeds(t *testing.T) {
	body, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)

	renew := jsFunctionBody(t, html, "async function renewTerminalAssertion()")
	if strings.Contains(renew, "showToast(") {
		t.Error("renewTerminalAssertion still toasts directly; a hub-SSO-only user sees an error as their terminal opens")
	}
	for _, want := range []string{
		"return { ok: true };",
		"message = 'Sign in to this hive before opening a terminal.';",
		"return { ok: false, status: resp.status, message };",
	} {
		if !strings.Contains(renew, want) {
			t.Errorf("renewTerminalAssertion missing %q — it must return the verdict for openTerminal to defer", want)
		}
	}

	open := jsFunctionBody(t, html, "async function openTerminal(agentName, href)")
	renewIdx := strings.Index(open, "const renewal = await renewTerminalAssertion();")
	handoffIdx := strings.Index(open, "const code = await createTerminalHandoff();")
	successReturn := strings.Index(open, "return;")
	toastIdx := strings.Index(open, "if (renewal.message) showToast(renewal.message, 'error');")
	switch {
	case renewIdx < 0:
		t.Fatal("openTerminal does not keep the renewal verdict")
	case handoffIdx < 0:
		t.Fatal("openTerminal does not call createTerminalHandoff")
	case toastIdx < 0:
		t.Fatal("openTerminal never surfaces the deferred renewal message when the handoff fails")
	case !(renewIdx < handoffIdx && handoffIdx < successReturn && successReturn < toastIdx):
		t.Errorf("openTerminal must renew, hand off, return on success, and only then toast the renewal verdict; got renew@%d handoff@%d return@%d toast@%d",
			renewIdx, handoffIdx, successReturn, toastIdx)
	}
}
