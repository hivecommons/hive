package dashboard

import (
	"strings"
	"testing"
)

// TestPrepareTerminalURLs pins the extraction pipeline that backs the
// dashboard's click-to-copy control (#5188).
func TestPrepareTerminalURLs(t *testing.T) {
	t.Run("newest first", func(t *testing.T) {
		// The pane is chronological, so the URL an operator wants is the last
		// one printed — a stale URL from earlier in the scrollback offered
		// first is the same failure as no URL at all.
		got := prepareTerminalURLs("visit https://example.com/one\nthen https://example.com/two\n")
		if len(got) != 2 || got[0] != "https://example.com/two" {
			t.Fatalf("want newest-first, got %v", got)
		}
	})

	t.Run("deduplicates", func(t *testing.T) {
		// A CLI reprints its login URL on every redraw; the list must not fill
		// up with the same URL.
		got := prepareTerminalURLs("https://a.test/x\nhttps://a.test/x\nhttps://a.test/x\n")
		if len(got) != 1 {
			t.Fatalf("want 1 deduplicated URL, got %v", got)
		}
	})

	t.Run("strips trailing sentence punctuation", func(t *testing.T) {
		// "Open https://a.test/go." must not copy the period into the URL.
		got := prepareTerminalURLs("Open https://a.test/go.")
		if len(got) != 1 || got[0] != "https://a.test/go" {
			t.Fatalf("want punctuation trimmed, got %v", got)
		}
	})

	t.Run("stops at quotes and whitespace", func(t *testing.T) {
		got := prepareTerminalURLs(`curl "https://a.test/go" --fail`)
		if len(got) != 1 || got[0] != "https://a.test/go" {
			t.Fatalf("want the quoted URL alone, got %v", got)
		}
	})

	t.Run("no URLs is an empty list not an error", func(t *testing.T) {
		if got := prepareTerminalURLs("nothing to see here\n"); len(got) != 0 {
			t.Fatalf("want no URLs, got %v", got)
		}
	})

	t.Run("bounded", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < maxTerminalURLs*3; i++ {
			b.WriteString("https://a.test/")
			b.WriteByte(byte('a' + i%26))
			b.WriteString(strings.Repeat("z", i))
			b.WriteString("\n")
		}
		if got := prepareTerminalURLs(b.String()); len(got) > maxTerminalURLs {
			t.Fatalf("want at most %d URLs, got %d", maxTerminalURLs, len(got))
		}
	})
}

// TestPrepareTerminalURLsOAuthLogin is the flow this endpoint exists for: an
// operator whose agent is wedged on `/login` must get the OAuth URL out whole.
// tmux capture-pane -J has already rejoined the terminal wrapping by the time
// the text reaches here, so the assertion is that nothing downstream re-breaks
// or drops it.
func TestPrepareTerminalURLsOAuthLogin(t *testing.T) {
	const want = "https://claude.ai/oauth/authorize?code=true&client_id=abc123&scope=user%3Ainference"
	pane := "Please run /login\n\nOpen this URL in your browser:\n\n  " + want + "\n\nPaste code here: \n"
	got := prepareTerminalURLs(pane)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("OAuth login URL did not survive extraction: got %v", got)
	}
}

// TestPrepareTerminalURLsRedacts is the security invariant. A URL is the most
// likely place in a pane for a credential to appear — `?token=` query strings
// and device-auth codes are URL-shaped — so this endpoint must not become a
// bypass for the redaction every other pane surface applies. A redacted URL is
// dropped entirely rather than offered, because handing an operator a URL with
// a redaction marker inside it invites pasting a broken URL.
func TestPrepareTerminalURLsRedacts(t *testing.T) {
	pane := "run: https://hive.test/callback?token=ghp_" + strings.Repeat("A", 36) + "\n"
	for _, u := range prepareTerminalURLs(pane) {
		if strings.Contains(u, "REDACTED") || strings.Contains(u, "***") {
			t.Fatalf("offered a URL carrying a redaction marker: %q", u)
		}
		if strings.Contains(u, strings.Repeat("A", 36)) {
			t.Fatalf("leaked an unredacted token through the URL endpoint: %q", u)
		}
	}
}

// TestFilterAuthURLs pins the #5327 fix: the dashboard's "Copy login URL"
// control must be offered a sign-in link or nothing at all. The bug it fixes
// is an operator with no login in flight clicking the control and receiving a
// repository URL scraped from the agent's own output — a URL they never asked
// for, under a label promising something else.
func TestFilterAuthURLs(t *testing.T) {
	t.Run("keeps an OAuth authorize URL", func(t *testing.T) {
		// The flow the control exists for.
		const login = "https://claude.ai/oauth/authorize?code=true&client_id=abc123"
		got := filterAuthURLs([]string{login})
		if len(got) != 1 || got[0] != login {
			t.Fatalf("the OAuth login URL must survive filtering, got %v", got)
		}
	})

	t.Run("drops URLs an agent merely printed", func(t *testing.T) {
		// Exactly what the issue observed: with no auth URL on the pane the
		// control offered a repo URL from the agent's own output.
		for _, u := range []string{
			"https://github.com/hivecommons/hive",
			"https://github.com/hivecommons/hive/pull/5315",
			"https://pkg.go.dev/net/http",
			"https://example.com/docs/getting-started",
		} {
			if got := filterAuthURLs([]string{u}); len(got) != 0 {
				t.Errorf("non-auth URL %q was offered as a login URL", u)
			}
		}
	})

	t.Run("a hostname that merely reads like auth does not qualify", func(t *testing.T) {
		// Markers are matched on path+query only, so prose about an OAuth
		// demo site cannot dress an ordinary link up as a sign-in link.
		if got := filterAuthURLs([]string{"https://oauth-authorize-demo.example.com/blog"}); len(got) != 0 {
			t.Fatalf("host-only match promoted a non-auth URL: %v", got)
		}
	})

	t.Run("preserves newest-first order", func(t *testing.T) {
		got := filterAuthURLs([]string{
			"https://a.test/oauth/authorize?n=1",
			"https://github.com/hivecommons/hive",
			"https://b.test/login/oauth?n=2",
		})
		if len(got) != 2 || got[0] != "https://a.test/oauth/authorize?n=1" {
			t.Fatalf("filtering must not reorder, got %v", got)
		}
	})

	t.Run("empty in empty out", func(t *testing.T) {
		if got := filterAuthURLs(nil); len(got) != 0 {
			t.Fatalf("want empty, got %v", got)
		}
	})
}

// TestAuthURLsStayRedacted keeps the #5315 security invariant attached to the
// new list too: filtering runs on the ALREADY-redacted output of
// prepareTerminalURLs, so a credential-bearing URL can never reach the
// dashboard through authUrls either. A `?token=` string is URL-shaped and is
// the single most likely credential in a pane, so this is the shape most worth
// pinning.
func TestAuthURLsStayRedacted(t *testing.T) {
	pane := "sign in: https://hive.test/login/oauth?token=ghp_" + strings.Repeat("A", 36) + "\n"
	for _, u := range filterAuthURLs(prepareTerminalURLs(pane)) {
		if strings.Contains(u, "REDACTED") || strings.Contains(u, "***") {
			t.Fatalf("offered an auth URL carrying a redaction marker: %q", u)
		}
		if strings.Contains(u, strings.Repeat("A", 36)) {
			t.Fatalf("leaked an unredacted token through the auth URL list: %q", u)
		}
	}
}

// TestDeviceFlowStaysExcluded restates the deliberate #5315 trade in the new
// filter's terms. deviceCodeLineRedactor blanks any `login/device` line
// wholesale because it carries a one-time code beside the URL, so a GitHub
// device-flow URL never reaches either list — even though `/login/device` is
// an authURLMarker. The device flow has its own first-class dashboard control
// that copies the code, so nothing is lost.
func TestDeviceFlowStaysExcluded(t *testing.T) {
	pane := "! First copy your one-time code: ABCD-1234\nOpen https://github.com/login/device and paste it\n"
	if got := filterAuthURLs(prepareTerminalURLs(pane)); len(got) != 0 {
		t.Fatalf("device-flow URL must not be offered (its line carries the code): %v", got)
	}
}
