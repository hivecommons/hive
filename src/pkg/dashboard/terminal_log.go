package dashboard

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// handleAgentFullLog serves the full retained tmux scrollback of an agent's
// latest run as plain, selectable, searchable text (issue #3693). The browser
// "Terminal" (ttyd) only shows the last screenful; this endpoint pulls the whole
// retained session buffer server-side via Manager.CaptureFullLog, which uses the
// same per-agent socket + su-exec path as every other capture so it works under
// per-UID isolation.
//
// It is read-only (GET), so any authenticated role may call it — the same rule
// as handleAgentState and the /terminal proxy, both of which already expose the
// pane to any authenticated viewer.
//
// Query parameters:
//
//	?download=1  sets Content-Disposition: attachment so the browser saves a
//	             .log text file (the "download full log" control). Omitted, the log
//	             renders inline in a new tab as selectable/searchable text (the
//	             "view full log" control).
//	?explain=only  returns just the agent's EXPLAIN reasoning lines (#3887).
//	?explain=hide  returns the log with those lines removed.
//	               Any other value (including absent) returns the log unfiltered.
func (s *Server) handleAgentFullLog(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.AgentMgr == nil {
		http.Error(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}

	name := s.resolveAgentParam(r.PathValue("name"))
	captureFullLog := s.deps.AgentMgr.CaptureFullLog
	if s.captureFullLogFn != nil {
		captureFullLog = s.captureFullLogFn
	}
	log, err := captureFullLog(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	log = prepareFullLog(log, r.URL.Query().Get("explain"))

	// text/plain so browsers render it as selectable, searchable text (Cmd/Ctrl-F
	// works, Select-All copies cleanly) rather than trying to interpret it.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// The pane is a live capture; never let an intermediary or the browser serve
	// a stale copy of a previous run.
	w.Header().Set("Cache-Control", "no-store")

	if r.URL.Query().Get("download") != "" {
		// Timestamped, filesystem-safe filename: hive-<agent>-<UTC>.log.
		safe := sanitizeLogFilename(name)
		fname := fmt.Sprintf("hive-%s-%s.log", safe, time.Now().UTC().Format("20060102-150405"))
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fname))
	}

	_, _ = w.Write([]byte(log))
}

// prepareFullLog is everything handleAgentFullLog does to a captured pane
// before it leaves the server. It exists as its own function so a test can pin
// the whole pipeline rather than a reconstruction of it.
//
// SECURITY: every other pane surface (/api/pane, the status summaries) strips
// tokens and device-auth codes before the output leaves the server; the full
// log must too, or it becomes a redaction bypass (see redactTokens).
//
// The invariant is that redaction applies to EVERY response, explain views
// included — not that it happens first. Agents in explain mode narrate what
// they were doing, which is exactly when a command containing a token gets
// quoted into prose, so the explain-only view is if anything more likely to
// carry one than the raw pane is. Adding a view must never add a path that
// skips redactTokens, and the explain filter must never be mistaken for one:
// it selects lines, it does not sanitize them.
func prepareFullLog(log, explainMode string) string {
	return filterExplainLines(redactTokens(log), explainMode)
}

// filterExplainLines splits an agent's explanation out of its ordinary output.
//
// Agent logs are tmux pane scrapes, so explanation cannot be written to a real
// second channel — there is only the one pane. Explain mode instead has the
// agent tag its reasoning with config.ExplainLinePrefix (see the explain kick
// suffixes in pkg/agent), which makes the split a read-time concern: "only"
// yields the reasoning stream on its own, "hide" yields the log an operator
// would have seen with explanation off. Neither changes what the agent did.
//
// Any other mode — including "" — returns the log untouched, so the default
// response is byte-identical to before this filter existed.
//
// Matching is on the trimmed line so the prefix is still recognized under the
// CLI's own indentation, and it is deliberately NOT anchored to column 0 for
// the same reason. Lines are rejoined with "\n"; a trailing newline is
// preserved when the input had one, so a filtered download still ends cleanly.
func filterExplainLines(log, mode string) string {
	var keepExplain bool
	switch mode {
	case "only":
		keepExplain = true
	case "hide":
		keepExplain = false
	default:
		return log
	}

	hadTrailingNewline := strings.HasSuffix(log, "\n")
	lines := strings.Split(log, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), config.ExplainLinePrefix) == keepExplain {
			kept = append(kept, line)
		}
	}
	out := strings.Join(kept, "\n")
	if hadTrailingNewline && out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

// sanitizeLogFilename reduces an agent name to characters safe in a download
// filename, so a display name or id never injects a path separator or quote into
// the Content-Disposition header.
func sanitizeLogFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		return "agent"
	}
	return out
}
