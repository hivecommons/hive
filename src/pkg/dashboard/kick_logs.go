package dashboard

// Kick-history endpoints (#4296, #4295): durable per-kick agent run logs.
//
// The existing /api/agents/{name}/log endpoint serves the LIVE tmux
// scrollback of the agent's current session only — a restart or a hive
// upgrade destroys every previous run's log. pkg/agent archives each kick's
// scrollback to a durable file under /data (see agent/kick_logs.go); these
// handlers list and serve those archives so an operator can read the log of
// a run several kicks ago.
//
// All three are read-only GETs, so any authenticated role may call them —
// the same rule as handleAgentFullLog and the /terminal proxy.

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"time"
)

// handleAgentKickLogList serves GET /api/agents/{name}/kicks: a JSON array of
// the agent's archived kick logs, newest first. An agent with no history yet
// gets an empty array — never an error (#4296's "less history than normal"
// compatibility requirement).
func (s *Server) handleAgentKickLogList(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.AgentMgr == nil {
		http.Error(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}
	name := s.resolveAgentParam(r.PathValue("name"))
	infos, err := s.deps.AgentMgr.ListKickLogs(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(infos)
}

// handleAgentKickLog serves GET /api/agents/{name}/kicks/{id}: one archived
// kick log as plain, selectable, searchable text. ?download=1 sets a
// Content-Disposition attachment, mirroring handleAgentFullLog.
func (s *Server) handleAgentKickLog(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.AgentMgr == nil {
		http.Error(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}
	name := s.resolveAgentParam(r.PathValue("name"))
	id := r.PathValue("id")
	log, err := s.deps.AgentMgr.ReadKickLog(name, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// SECURITY: archives hold raw pane content; like every other pane surface,
	// tokens and device-auth codes are stripped before the bytes leave the
	// server (see redactTokens).
	log = redactTokens(log)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("download") != "" {
		fname := fmt.Sprintf("hive-%s-%s", sanitizeLogFilename(name), sanitizeLogFilename(id)+".log")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fname))
	}
	_, _ = w.Write([]byte(log))
}

// handleAgentKickHistoryPage serves GET /agents/{name}/kicks: a minimal HTML
// index of the agent's run-log history — the live log of the current run
// first, then every archived kick, newest first, each with view and download
// links. It follows the dashboard's open-in-a-new-tab pattern for logs
// rather than embedding a viewer.
func (s *Server) handleAgentKickHistoryPage(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.AgentMgr == nil {
		http.Error(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}
	name := s.resolveAgentParam(r.PathValue("name"))
	infos, err := s.deps.AgentMgr.ListKickLogs(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	esc := html.EscapeString(name)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>%s — kick log history</title>
<style>
body{font-family:ui-monospace,monospace;background:#0d1117;color:#c9d1d9;margin:2rem}
h1{font-size:1.1rem}a{color:#58a6ff;text-decoration:none}a:hover{text-decoration:underline}
table{border-collapse:collapse;margin-top:1rem}td,th{padding:.3rem .9rem;text-align:left;border-bottom:1px solid #21262d}
.muted{color:#8b949e}
</style></head><body><h1>🕘 %s — kick log history</h1>
<p><a href="/api/agents/%s/log" target="_blank">📄 live log (current run)</a> <span class="muted">— the running session's scrollback; archived here when the next kick or a restart rotates it out</span></p>
`, esc, esc, esc)

	if len(infos) == 0 {
		fmt.Fprint(w, `<p class="muted">No archived kick logs yet. Archives appear after the agent's next kick, restart, or a hive shutdown.</p>`)
	} else {
		fmt.Fprint(w, `<table><tr><th>archived (UTC)</th><th>trigger</th><th>size</th><th></th></tr>`)
		for _, info := range infos {
			id := html.EscapeString(info.ID)
			fmt.Fprintf(w,
				`<tr><td>%s</td><td>%s</td><td>%d B</td><td><a href="/api/agents/%s/kicks/%s" target="_blank">view</a> · <a href="/api/agents/%s/kicks/%s?download=1">download</a></td></tr>`,
				info.Timestamp.UTC().Format(time.RFC3339), html.EscapeString(info.Reason), info.SizeBytes, esc, id, esc, id)
		}
		fmt.Fprint(w, `</table>`)
	}
	fmt.Fprint(w, `</body></html>`)
}
