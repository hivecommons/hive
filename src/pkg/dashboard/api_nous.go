package dashboard

import (
	"net/http"
	"regexp"
	"strings"
)

func (s *Server) handleNousStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]string{"status": "not_configured"})
		return
	}
	s.syncNousKnowledge()
	s.syncNousVerdict()
	s.deps.Nous.Mu.Lock()
	status := make(map[string]interface{}, len(s.deps.Nous.Status)+3)
	for k, v := range s.deps.Nous.Status {
		status[k] = v
	}
	// The verdict is derived state, merged at serve time so a stale copy
	// can never linger in Status after the campaign's file changes.
	if verdict := s.deps.Nous.verdictMap(); verdict != nil {
		status[nousStatusKeyVerdict] = verdict
	} else {
		delete(status, nousStatusKeyVerdict)
	}
	// The research question is derived from Config["goals"] at serve time so
	// the header text follows the goals the owner last saved.
	question := s.deps.Nous.researchQuestionLocked()
	config := make(map[string]interface{}, len(s.deps.Nous.Config))
	for k, v := range s.deps.Nous.Config {
		config[k] = v
	}
	ledger := append([]map[string]interface{}(nil), s.deps.Nous.Ledger...)
	principles := append([]NousPrinciple(nil), s.deps.Nous.Principles...)
	// Mode is the live trust-ladder knob (PUT /api/nous/mode); the Status
	// map only carries the boot-time value, so without this the lab kept
	// reporting "observe" after the operator switched to suggest (#12532).
	if s.deps.Nous.Mode != "" {
		status["mode"] = s.deps.Nous.Mode
	}
	s.deps.Nous.Mu.Unlock()
	// The Experiment Lab is repo-only: the reported scope is always "repo"
	// and any governor phase block published by the experiment loop is
	// dropped so the lab never surfaces governor data.
	status["scope"] = NousScopeRepo
	status["phases"] = nousRepoOnlyPhases(status["phases"])
	if question == "" {
		// The goals section is in-memory only; after a restart the active
		// campaign still knows the question.
		if c, ok := s.deps.Nous.ActiveCampaign(); ok {
			question = normalizeNousResearchQuestion(c.Question)
		}
	}
	status[nousStatusKeyResearchQuestion] = question
	s.addNousProgressStatus(status, config, ledger, principles)
	status["projectRepos"] = s.nousProjectRepos()
	status["availableRepos"] = status["projectRepos"]
	status["repoTargets"] = s.nousRepoTargets()
	jsonResponse(w, status)
}

// nousRepoOnlyPhases returns phases restricted to the repo scope. A missing
// or malformed phases block yields an empty object rather than nil so the
// dashboard can index it unconditionally.
func nousRepoOnlyPhases(phases interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	m, ok := phases.(map[string]interface{})
	if !ok {
		return out
	}
	if repo, ok := m[NousScopeRepo]; ok {
		out[NousScopeRepo] = repo
	}
	return out
}

// nousResearchQuestion returns the stored repo research question. The
// legacy governorGoal field, if still present in goals, is ignored.
func nousResearchQuestion(goals interface{}) string {
	m, ok := goals.(map[string]interface{})
	if !ok {
		return ""
	}
	repo, _ := m["repoGoal"].(string)
	return normalizeNousResearchQuestion(repo)
}

var (
	nousResearchQuestionKeyRe    = regexp.MustCompile(`(?i)^research[_ -]?question\s*:\s*`)
	nousResearchQuestionMarkerRe = regexp.MustCompile(`^(?:[>|][-+]?\s*)+`)
	nousResearchQuestionSpaceRe  = regexp.MustCompile(`\s+`)
)

// normalizeNousResearchQuestion strips a pasted YAML key
// ("research_question:") and folded-scalar / blockquote markers ("> ", "|")
// from a stored research question so the dashboard shows only the question.
func normalizeNousResearchQuestion(raw string) string {
	text := strings.TrimSpace(raw)
	text = nousResearchQuestionKeyRe.ReplaceAllString(text, "")
	text = nousResearchQuestionMarkerRe.ReplaceAllString(text, "")
	return strings.TrimSpace(nousResearchQuestionSpaceRe.ReplaceAllString(text, " "))
}

func (s *Server) handleNousLedger(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	s.syncNousKnowledge()
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
	s.syncNousKnowledge()
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

	// Approve only records the owner's answer. It no longer opens issues
	// or admits runs: those are promotion targets the owner picks on the
	// verdict card (POST /api/nous/promote, #11699).
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
		s.deps.Nous.SetMode(body.Mode)
		s.persistNousPanel()
	}

	s.auditFromRequest(r, "nous_set_mode", auditDetail("mode", body.Mode), "")
	okResponse(w, map[string]string{"status": "updated", "mode": body.Mode})
}

// handleNousScope keeps the legacy PUT /api/nous/scope endpoint for
// compatibility, but the Experiment Lab is repo-only: "repo" is the only
// accepted value and anything else (including the former "governor" and
// "both") is rejected with 400.
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

	body.Scope = strings.ToLower(sanitizeString(body.Scope))
	if body.Scope != NousScopeRepo {
		jsonError(w, `scope must be "repo": the Experiment Lab is repo-only`, http.StatusBadRequest)
		return
	}

	if s.deps.Nous != nil {
		s.deps.Nous.SetScope(NousScopeRepo)
		s.persistNousPanel()
	}

	s.auditFromRequest(r, "nous_set_scope", auditDetail("scope", NousScopeRepo), "")
	okResponse(w, map[string]string{"status": "updated", "scope": NousScopeRepo})
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

	// `note` is accepted as an alias of `reason` (the issue's wording);
	// `gate_id` lets a caller refuse to answer a gate it has not seen.
	var body struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
		Note     string `json:"note"`
		GateID   string `json:"gate_id"`
	}
	if err := decodeBody(r, &body); err != nil || body.Decision == "" {
		jsonError(w, "decision is required", http.StatusBadRequest)
		return
	}

	body.Decision = sanitizeString(body.Decision)
	body.Reason = sanitizeString(body.Reason)
	body.Note = sanitizeString(body.Note)
	body.GateID = sanitizeString(body.GateID)
	if body.Decision == "" {
		jsonError(w, "decision is required", http.StatusBadRequest)
		return
	}
	if body.Reason == "" {
		body.Reason = body.Note
	}

	d, err := s.deps.Nous.RecordGateDecision(body.GateID, body.Decision, body.Reason, requestUser(r))
	if err != nil {
		s.auditFromRequest(r, "nous_gate_decision_failed", auditDetail("decision", body.Decision, "gate_id", body.GateID, "error", err.Error()), "")
		jsonError(w, err.Error(), nousGateStatus(err))
		return
	}

	s.auditFromRequest(r, "nous_gate_decision", auditDetail("decision", d.Decision, "gate_id", d.GateID, "reason", d.Reason), "")
	okResponse(w, map[string]string{"status": "decided", "decision": d.Decision, "gate_id": d.GateID})
}

func (s *Server) handleNousGatePending(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	// Re-read the agent's pending gate on demand so the panel never waits
	// a full poll interval after the agent publishes (#11696).
	if err := s.deps.Nous.SyncGate(); err != nil && s.logger != nil {
		s.logger.Warn("nous: pending gate unreadable", "error", err)
	}
	s.deps.Nous.Mu.Lock()
	pending := s.deps.Nous.GatePending
	s.deps.Nous.Mu.Unlock()
	jsonResponse(w, pending)
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
	cfg := make(map[string]interface{}, len(s.deps.Nous.Config)+6)
	for k, v := range s.deps.Nous.Config {
		cfg[k] = v
	}
	mode := s.deps.Nous.Mode
	principles := append([]NousPrinciple(nil), s.deps.Nous.Principles...)
	s.deps.Nous.Mu.Unlock()

	projectRepos := s.nousProjectRepos()
	repoTargets := s.nousRepoTargetsFrom(cfg, projectRepos)
	cfg["projectRepos"] = projectRepos
	cfg["availableRepos"] = projectRepos
	cfg["repoTargets"] = repoTargets
	cfg["principles"] = principles

	campaign := map[string]interface{}{"mode": mode}
	if ff, ok := cfg["fast_fail"]; ok {
		campaign["fast_fail"] = ff
	}
	if sched, ok := cfg["schedule"]; ok {
		campaign["schedule"] = sched
	}
	cfg["campaign"] = campaign

	// Per-campaign baseline (hivecommons/hive#12501): expose the active
	// experiment and fall back to its question when goals are unset
	// (for example after a restart, when Config is in-memory only).
	if c, ok := s.deps.Nous.ActiveCampaign(); ok {
		cfg["activeCampaign"] = nousCampaignToView(s.deps.Nous, c)
		if nousResearchQuestion(cfg["goals"]) == "" && c.Question != "" {
			cfg["goals"] = map[string]interface{}{"repoGoal": c.Question}
		}
	}

	repoCampaign := map[string]interface{}{}
	if goals, ok := cfg["goals"].(map[string]interface{}); ok {
		if rq, ok := goals["repoGoal"].(string); ok {
			repoCampaign["research_question"] = strings.TrimSpace(rq)
		}
	}
	cfg["repoCampaign"] = repoCampaign
	jsonResponse(w, cfg)
}

func (s *Server) nousProjectRepos() []string {
	if s == nil || s.deps.Config == nil {
		return nil
	}
	cfg := s.deps.Config
	seen := map[string]bool{}
	var repos []string
	add := func(repo string) {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			return
		}
		key := strings.ToLower(repo)
		if !seen[key] {
			seen[key] = true
			repos = append(repos, repo)
		}
	}
	for _, repo := range cfg.Project.Repos {
		add(repo)
	}
	add(cfg.Project.PrimaryRepo)
	return repos
}

func (s *Server) nousRepoTargets() []string {
	if s == nil || s.deps.Nous == nil {
		return nil
	}
	s.deps.Nous.Mu.Lock()
	cfg := make(map[string]interface{}, len(s.deps.Nous.Config))
	for k, v := range s.deps.Nous.Config {
		cfg[k] = v
	}
	s.deps.Nous.Mu.Unlock()
	return s.nousRepoTargetsFrom(cfg, s.nousProjectRepos())
}

func (s *Server) nousRepoTargetsFrom(cfg map[string]interface{}, projectRepos []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(repo string) {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			return
		}
		key := strings.ToLower(repo)
		if !seen[key] {
			seen[key] = true
			out = append(out, repo)
		}
	}
	if reposCfg, ok := cfg["repos"].(map[string]interface{}); ok {
		if raw, ok := reposCfg["repos"].([]interface{}); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					add(s)
				}
			}
		}
		if raw, ok := reposCfg["repos"].([]string); ok {
			for _, v := range raw {
				add(v)
			}
		}
	}
	if len(out) == 0 {
		for _, repo := range projectRepos {
			add(repo)
		}
	}
	return out
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
	s.persistNousPanel()

	s.auditFromRequest(r, "nous_config_update", auditDetail("section", section), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "section": section})
}
