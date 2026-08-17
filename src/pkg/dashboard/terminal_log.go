package dashboard

import (
	"fmt"
	"net/http"
	"strings"
	"time"
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
func (s *Server) handleAgentFullLog(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.AgentMgr == nil {
		http.Error(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}

	name := s.resolveAgentParam(r.PathValue("name"))
	log, err := s.deps.AgentMgr.CaptureFullLog(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// SECURITY: every other pane surface (/api/pane, the status summaries) strips
	// tokens and device-auth codes before the output leaves the server; the full
	// log must too, or it becomes a redaction bypass (see redactTokens).
	log = redactTokens(log)

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
