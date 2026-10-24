package dashboard

import (
	"net/http"
)

// handleAgentLoginCode types an operator-supplied authorization code into an
// agent's pane, completing an OAuth hand-off the dashboard terminal cannot.
//
// It is the write-side twin of handleAgentTerminalURLs. That endpoint exists
// because the terminal cannot deliver a COPY (#5188); this one exists because
// it cannot reliably accept a PASTE either. An operator who has opened the
// sign-in URL is then asked to paste a code back into a ttyd/xterm.js pane over
// tmux, where it arrives mangled — leaving a host shell and `tmux send-keys` as
// the only route, which is no answer for an operator whose interface is a web
// dashboard.
//
// OWNER-ONLY, unlike its read-side twin. handleAgentTerminalURLs is a GET that
// any authenticated role may call because it exposes strictly less than the
// terminal proxy already does. This one WRITES to an interactive pane, so it
// takes the strictest gate the dashboard has rather than inheriting the read
// endpoint's reasoning. It grants no capability the writable ttyd terminal does
// not already give the same operator; the point of the gate is that a narrower
// door should not be easier to open than the wide one beside it.
//
// The code is validated in crux.Runtime.SubmitLoginCode (printable, whitespace-free,
// length-bounded) so it cannot carry a newline and become a second command. It
// is never logged, and never echoed back in a response — the audit entry
// records that a code was submitted for an agent, not what it was.
func (s *Server) handleAgentLoginCode(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.AgentMgr == nil {
		jsonError(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}

	name := s.resolveAgentParam(r.PathValue("name"))
	var body struct {
		Code string `json:"code"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := s.deps.AgentMgr.SubmitLoginCode(name, body.Code); err != nil {
		// SubmitLoginCode's errors describe the SHAPE of the problem and never
		// quote the value, so they are safe to return to the operator who typed
		// it — and they are the only way that operator learns why a paste was
		// refused.
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if s.deps.Logger != nil {
		s.deps.Logger.Info("audit: login code submitted to agent", "agent", name, "trigger", "dashboard-api")
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "agent": name})
}
