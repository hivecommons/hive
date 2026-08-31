package dashboard

import (
	"net/http"
	"regexp"
	"strings"
)

// This endpoint exists because the dashboard terminal cannot deliver a copy
// (issue #5188). The terminal is ttyd/xterm.js attached to the agent's tmux
// session; tmux mouse mode owns drag selection so browser highlight-and-copy
// reaches nothing, and released ttyd (1.7.7, the version this image pins) has
// no browser-side OSC52 handling, so Claude Code's own "press c to copy" and
// tmux copy-mode both drop their clipboard escape before it can reach the
// browser. That leaves the highest-stakes flow in the hive — pasting a
// `/login` OAuth URL out of a wedged agent's terminal — with no working copy
// affordance at all.
//
// Rather than fight the terminal, the copy is done server-side: the pane is
// captured with `capture-pane -J` (already how CaptureFullLog reads it, so
// wrapped lines are rejoined) and the URLs in it are handed to the dashboard,
// which puts one on the clipboard with a single click. Joining is the part no
// client-side fix can reproduce: a URL wrapped across pane columns copies with
// embedded newlines that silently invalidate the OAuth exchange, and only the
// capture knows where the wrap was.
//
// It is read-only (GET), so any authenticated role may call it — the same rule
// as handleAgentFullLog and the /terminal proxy, both of which already expose
// the whole pane to any authenticated viewer. This endpoint exposes strictly
// less: a filtered subset of bytes those two already return, through the same
// redaction (see prepareTerminalURLs).
const maxTerminalURLs = 20

// terminalURLPattern matches http(s) URLs in captured pane text. The trailing
// class deliberately excludes whitespace, quotes, angle brackets, and
// backticks so a URL printed inside CLI prose or a shell command does not
// swallow the words after it.
var terminalURLPattern = regexp.MustCompile(`https?://[^\s"'` + "`" + `<>]+`)

// terminalURLTrailing is punctuation a terminal line commonly puts AFTER a URL
// — a sentence period, a closing paren, a trailing comma — which is not part
// of the URL. Stripped from the right only.
const terminalURLTrailing = `.,;:!?)]}'"`

// handleAgentTerminalURLs returns the distinct URLs visible in an agent's
// retained tmux scrollback, most recent first, so the dashboard can offer
// click-to-copy for a URL the operator cannot select out of the terminal.
//
// Response: {"urls": [...], "authUrls": [...]}. `authUrls` is the subset that
// looks like a sign-in link (see isAuthURL) and is the ONLY list the dashboard
// offers to copy — issue #5327: an operator with no login in flight was handed
// a repository URL scraped from the agent's own output, which they had never
// asked for. `urls` is retained for callers that want the unfiltered view.
//
// An agent with no session, or with no URL on screen, is not an error — it
// returns empty lists, because "there is nothing to copy right now" is a
// normal state the dashboard renders by hiding the control rather than as a
// failure.
func (s *Server) handleAgentTerminalURLs(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.AgentMgr == nil {
		jsonError(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}

	name := s.resolveAgentParam(r.PathValue("name"))
	log, err := s.deps.AgentMgr.CaptureFullLog(name)
	if err != nil {
		// A stopped or never-started agent has no pane to read. That is not a
		// server fault and must not render as an error in the dashboard.
		jsonResponse(w, map[string]interface{}{"urls": []string{}, "authUrls": []string{}})
		return
	}

	// The pane is a live capture; never let an intermediary serve a stale copy.
	w.Header().Set("Cache-Control", "no-store")
	urls := prepareTerminalURLs(log)
	jsonResponse(w, map[string]interface{}{"urls": urls, "authUrls": filterAuthURLs(urls)})
}

// prepareTerminalURLs is everything handleAgentTerminalURLs does to a captured
// pane before it leaves the server. It is its own function so a test can pin
// the whole pipeline rather than a reconstruction of it.
//
// SECURITY: redactTokens runs FIRST, on the whole capture, exactly as
// prepareFullLog does. A URL is the single most likely place in a pane for a
// credential to appear — device-auth codes and `?token=` query strings are
// literally URL-shaped — so extracting before redacting would turn this
// endpoint into a redaction bypass for the one shape most worth redacting. A
// redacted URL is then dropped rather than offered: handing an operator a URL
// with `***REDACTED***` in the middle of it invites pasting a broken URL and
// wondering why the login failed.
//
// One consequence is deliberate and worth stating: redactTokens also blanks
// any line matching deviceCodeLineRedactor (`login/device`, "Waiting for
// authorization", …), so a GitHub *device-flow* URL never reaches this list.
// That is the correct trade — those lines carry the one-time code beside the
// URL — and it costs nothing here, because the device flow already has a
// first-class dashboard control that copies the code. The flow this endpoint
// exists for, a Claude Code `/login` OAuth URL, is not device-flow shaped and
// comes through whole.
func prepareTerminalURLs(log string) []string {
	redacted := redactTokens(log)
	matches := terminalURLPattern.FindAllString(redacted, -1)

	seen := make(map[string]bool, len(matches))
	// Collected newest-first: the pane is chronological, so the URL an
	// operator wants is almost always the last one printed.
	urls := make([]string, 0, len(matches))
	for i := len(matches) - 1; i >= 0; i-- {
		u := strings.TrimRight(matches[i], terminalURLTrailing)
		// Bare "https://" left after trimming carries nothing to copy.
		if u == "" || strings.HasSuffix(u, "//") {
			continue
		}
		if strings.Contains(u, "REDACTED") || strings.Contains(u, "***") {
			continue
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		urls = append(urls, u)
		if len(urls) >= maxTerminalURLs {
			break
		}
	}
	return urls
}

// authURLMarkers are substrings that identify a URL as a sign-in link rather
// than an incidental URL the agent happened to print. Issue #5327: the copy
// control was offered unconditionally, so a pane with no login in flight
// returned a repository URL from the agent's own output — a URL the operator
// never asked for, attached to a button whose whole reason to exist is the
// login case.
//
// Kept deliberately narrow. A false negative hides a control the operator can
// still work around (open the terminal, or use the full log); a false positive
// reintroduces exactly the bug being fixed, by promising "Copy login URL" and
// delivering whatever else was on screen. Matching is case-insensitive because
// CLIs are inconsistent about it, and is done on the path+query only so a host
// named e.g. "oauth-demo.example.com" in ordinary output cannot qualify.
var authURLMarkers = []string{
	"/oauth/authorize",
	"/oauth2/authorize",
	"/authorize",
	"/cai/oauth",
	"/login/oauth",
	"/login/device",
	"/device/code",
	"/activate",
	"/signin",
	"/sign-in",
	"/auth/callback",
}

// authURLHosts are hosts whose *sole* dashboard-relevant purpose is
// authentication, so any URL on them is a sign-in link regardless of path.
var authURLHosts = []string{
	"claude.ai/oauth",
	"console.anthropic.com/oauth",
	"accounts.google.com",
}

// isAuthURL reports whether u is a sign-in link an operator is expected to
// open in a browser to unwedge an agent. See authURLMarkers for why this is
// intentionally conservative.
func isAuthURL(u string) bool {
	lower := strings.ToLower(u)
	for _, h := range authURLHosts {
		if strings.Contains(lower, h) {
			return true
		}
	}
	// Strip the scheme and host so a marker can only match in the path or
	// query, never in a hostname that merely reads like one.
	rest := lower
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	slash := strings.IndexAny(rest, "/?#")
	if slash < 0 {
		return false
	}
	rest = rest[slash:]
	for _, m := range authURLMarkers {
		if strings.Contains(rest, m) {
			return true
		}
	}
	return false
}

// filterAuthURLs keeps only the sign-in links, preserving the newest-first
// order prepareTerminalURLs established.
func filterAuthURLs(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if isAuthURL(u) {
			out = append(out, u)
		}
	}
	return out
}
