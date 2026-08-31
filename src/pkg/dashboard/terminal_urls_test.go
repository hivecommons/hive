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
