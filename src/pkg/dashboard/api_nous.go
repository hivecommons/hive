package dashboard

import (
	"net/http"
)

func (s *Server) handleNousStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]string{"status": "not_configured"})
		return
	}
	s.deps.Nous.Mu.Lock()
	status := make(map[string]interface{}, len(s.deps.Nous.Status))
	for k, v := range s.deps.Nous.Status {
		status[k] = v
	}
	s.deps.Nous.Mu.Unlock()
	jsonResponse(w, status)
}

func (s *Server) handleNousLedger(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	s.deps.Nous.Mu.Lock()
	ledger := s.deps.Nous.Ledger
	s.deps.Nous.Mu.Unlock()
	if ledger == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, ledger)
}

func (s *Server) handleNousPrinciples(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	s.deps.Nous.Mu.Lock()
	principles := s.deps.Nous.Principles
	s.deps.Nous.Mu.Unlock()
	if principles == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, principles)
}

func (s *Server) handleNousApprove(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	s.auditFromRequest(r, "nous_approve", "", "")
	okResponse(w, map[string]string{"status": "approved"})
}

func (s *Server) handleNousAbort(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	s.auditFromRequest(r, "nous_abort", "", "")
	okResponse(w, map[string]string{"status": "aborted"})
}

func (s *Server) handleNousMode(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Mode string `json:"mode"`
	}
	if err := decodeBody(r, &body); err != nil || body.Mode == "" {
		jsonError(w, "mode is required", http.StatusBadRequest)
		return
	}

	body.Mode = sanitizeString(body.Mode)
	if body.Mode == "" {
		jsonError(w, "mode is required", http.StatusBadRequest)
		return
	}

	if s.deps.Nous != nil {
		s.deps.Nous.Mode = body.Mode
	}

	s.auditFromRequest(r, "nous_set_mode", auditDetail("mode", body.Mode), "")
	okResponse(w, map[string]string{"status": "updated", "mode": body.Mode})
}

func (s *Server) handleNousScope(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Scope string `json:"scope"`
	}
	if err := decodeBody(r, &body); err != nil || body.Scope == "" {
		jsonError(w, "scope is required", http.StatusBadRequest)
		return
	}

	body.Scope = sanitizeString(body.Scope)
	if body.Scope == "" {
		jsonError(w, "scope is required", http.StatusBadRequest)
		return
	}

	if s.deps.Nous != nil {
		s.deps.Nous.Scope = body.Scope
	}

	s.auditFromRequest(r, "nous_set_scope", auditDetail("scope", body.Scope), "")
	okResponse(w, map[string]string{"status": "updated", "scope": body.Scope})
}

func (s *Server) handleNousPhase(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]string{"phase": "inactive"})
		return
	}
	jsonResponse(w, map[string]string{"phase": s.deps.Nous.Phase})
}

func (s *Server) handleNousGateDecision(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Nous == nil {
		jsonError(w, "nous not configured", http.StatusNotFound)
		return
	}

	var body struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := decodeBody(r, &body); err != nil || body.Decision == "" {
		jsonError(w, "decision is required", http.StatusBadRequest)
		return
	}

	body.Decision = sanitizeString(body.Decision)
	body.Reason = sanitizeString(body.Reason)
	if body.Decision == "" {
		jsonError(w, "decision is required", http.StatusBadRequest)
		return
	}

	if s.deps.Nous.GatePending == nil {
		s.deps.Nous.GatePending = make(map[string]interface{})
	}
	s.deps.Nous.GateResponse = map[string]interface{}{
		"decision": body.Decision,
		"reason":   body.Reason,
	}

	s.auditFromRequest(r, "nous_gate_decision", auditDetail("decision", body.Decision), "")
	okResponse(w, map[string]string{"status": "decided", "decision": body.Decision})
}

func (s *Server) handleNousGatePending(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	jsonResponse(w, s.deps.Nous.GatePending)
}

func (s *Server) handleNousGateRespond(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body map[string]interface{}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if s.deps.Nous != nil {
		s.deps.Nous.GateResponse = body
	}

	s.auditFromRequest(r, "nous_gate_respond", "", "")
	okResponse(w, map[string]string{"status": "responded"})
}

func (s *Server) handleNousGateResponse(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	jsonResponse(w, s.deps.Nous.GateResponse)
}

func (s *Server) handleNousConfigGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	s.deps.Nous.Mu.Lock()
	cfg := s.deps.Nous.Config
	s.deps.Nous.Mu.Unlock()
	jsonResponse(w, cfg)
}

func (s *Server) handleNousConfigGoals(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "goals")
}

func (s *Server) handleNousConfigRepos(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "repos")
}

func (s *Server) handleNousConfigOutput(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "output")
}

func (s *Server) handleNousConfigFastFail(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "fast_fail")
}

func (s *Server) handleNousConfigSchedule(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "schedule")
}

func (s *Server) handleNousConfigControllables(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "controllables")
}

func (s *Server) handleNousConfigPrinciples(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "principles")
}

func (s *Server) handleNousDeletePrinciple(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	id := r.PathValue("id")
	if s.deps.Nous == nil {
		jsonError(w, "nous not configured", http.StatusNotFound)
		return
	}

	s.deps.Nous.Mu.Lock()
	filtered := make([]NousPrinciple, 0, len(s.deps.Nous.Principles))
	for _, p := range s.deps.Nous.Principles {
		if p.ID != id {
			filtered = append(filtered, p)
		}
	}
	s.deps.Nous.Principles = filtered
	s.deps.Nous.Mu.Unlock()

	s.auditFromRequest(r, "nous_delete_principle", auditDetail("id", id), "")
	okResponse(w, map[string]string{"status": "deleted", "id": id})
}

func (s *Server) handleNousConfigSection(w http.ResponseWriter, r *http.Request, section string) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Nous == nil {
		jsonError(w, "nous not configured", http.StatusNotFound)
		return
	}

	var body interface{}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.deps.Nous.Mu.Lock()
	if s.deps.Nous.Config == nil {
		s.deps.Nous.Config = make(map[string]interface{})
	}
	s.deps.Nous.Config[section] = body
	s.deps.Nous.Mu.Unlock()

	s.auditFromRequest(r, "nous_config_update", auditDetail("section", section), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "section": section})
}
