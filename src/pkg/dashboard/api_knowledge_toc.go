package dashboard

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// knowledgeTOCScope parses the scope query shared by the TOC and full-read
// endpoints: layers, repos, types and tags (comma-separated allow-lists) plus
// include_states for operators who need draft/deprecated/superseded entries.
// The default is approved knowledge only (#11102).
func knowledgeTOCScope(r *http.Request) (knowledge.TOCScope, error) {
	q := r.URL.Query()
	include, err := knowledge.ParseLifecycleStates(q.Get("include_states"))
	if err != nil {
		return knowledge.TOCScope{}, err
	}
	scope := knowledge.TOCScope{
		Repos:         splitCSV(q.Get("repos")),
		Types:         splitCSV(q.Get("types")),
		Tags:          splitCSV(q.Get("tags")),
		IncludeStates: include,
	}
	for _, l := range splitCSV(q.Get("layers")) {
		scope.Layers = append(scope.Layers, knowledge.LayerType(strings.ToLower(l)))
	}
	return scope, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// handleKnowledgeTOC serves GET /api/knowledge/toc: a capped, body-free table
// of contents of the knowledge visible under the requested scope. With
// format=prompt it also returns the markdown rendering kicks inject, bounded
// by max_chars.
// A request naming an agent with ?agent= is further restricted to that
// agent's knowledge.agent_scopes entry; the request scope cannot widen it.
func (s *Server) handleKnowledgeTOC(w http.ResponseWriter, r *http.Request) {
	scope, err := knowledgeTOCScope(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	maxChars, _ := strconv.Atoi(r.URL.Query().Get("max_chars"))

	if !s.ensureKnowledge() {
		jsonResponse(w, map[string]interface{}{"enabled": false, "entries": []interface{}{}, "total": 0, "returned": 0, "truncated": false})
		return
	}
	facts := s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, "", "", 0)
	if agentScope, ok := s.knowledgeAgentScope(r); ok {
		facts = agentScope.Filter(facts)
	}
	toc := knowledge.BuildTOC(facts, scope, limit)
	resp := map[string]interface{}{
		"enabled":   true,
		"entries":   toc.Entries,
		"total":     toc.Total,
		"returned":  toc.Returned,
		"truncated": toc.Truncated,
	}
	if r.URL.Query().Get("format") == "prompt" {
		resp["prompt"] = knowledge.FormatTOCForPrompt(toc, maxChars)
	}
	jsonResponse(w, resp)
}

// handleKnowledgeEntry serves GET /api/knowledge/entry/{id}: one entry's
// complete markdown and front-matter. The same scope and lifecycle filter as
// the TOC applies, including the ?agent= scope, so an entry the TOC would hide
// cannot be read by guessing its id.
func (s *Server) handleKnowledgeEntry(w http.ResponseWriter, r *http.Request) {
	scope, err := knowledgeTOCScope(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not enabled", http.StatusNotFound)
		return
	}
	fact, err := s.deps.Knowledge.ReadEntry(s.deps.Ctx, r.PathValue("id"))
	if err != nil || fact == nil || !scope.InScope(*fact) {
		jsonError(w, "knowledge entry not found", http.StatusNotFound)
		return
	}
	if agentScope, ok := s.knowledgeAgentScope(r); ok && !agentScope.InScope(*fact) {
		jsonError(w, "knowledge entry not found", http.StatusNotFound)
		return
	}
	state := fact.EffectiveState()
	fact.State = state
	jsonResponse(w, map[string]interface{}{
		"id":       fact.Slug,
		"title":    fact.Title,
		"type":     fact.Type,
		"layer":    fact.Layer,
		"repo":     knowledge.FactRepo(*fact),
		"tags":     fact.Tags,
		"status":   state,
		"markdown": knowledge.RenderFactMarkdown(*fact),
		"fact":     fact,
	})
}

// maxKnowledgeStateReason caps the operator-supplied reason recorded in the
// audit log for a lifecycle change.
const maxKnowledgeStateReason = 500

// handleKnowledgeEntryState serves PUT /api/knowledge/entry/{id}/state: an
// owner-only lifecycle change (draft, approved, deprecated, superseded) of an
// entry in a local channel. Every change is audited with the actor, the
// previous and new state and the operator's reason (#11200).
func (s *Server) handleKnowledgeEntryState(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var req struct {
		State        string `json:"state"`
		SupersededBy string `json:"superseded_by"`
		Reason       string `json:"reason"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	supersededBy := strings.TrimSpace(req.SupersededBy)
	state, err := knowledge.ParseLifecycleState(req.State)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch {
	case state == knowledge.StateSuperseded && supersededBy == "":
		jsonError(w, "state superseded requires superseded_by", http.StatusBadRequest)
		return
	case state != knowledge.StateSuperseded && supersededBy != "":
		jsonError(w, "superseded_by requires state superseded", http.StatusBadRequest)
		return
	case supersededBy == id:
		jsonError(w, "an entry cannot supersede itself", http.StatusBadRequest)
		return
	}
	reason := strings.Join(strings.Fields(req.Reason), " ")
	if len(reason) > maxKnowledgeStateReason {
		reason = reason[:maxKnowledgeStateReason]
	}
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	change, err := s.deps.Knowledge.SetEntryState(id, state, supersededBy)
	switch {
	case errors.Is(err, knowledge.ErrEntryNotWritable):
		jsonError(w, "knowledge entry not found in a writable channel", http.StatusNotFound)
		return
	case errors.Is(err, knowledge.ErrReplacementNotInChannel):
		jsonError(w, "superseded_by entry not found in the same channel", http.StatusBadRequest)
		return
	case err != nil:
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	action, detail := "knowledge_set_state", []string{"id", id, "channel", change.Channel, "from", string(change.Previous), "to", string(state)}
	if supersededBy != "" {
		action = "knowledge_supersede"
		detail = append(detail, "superseded_by", supersededBy)
	}
	s.auditFromRequest(r, action, auditDetail(append(detail, "reason", reason)...), "")
	jsonResponse(w, map[string]interface{}{
		"ok":             true,
		"id":             id,
		"channel":        change.Channel,
		"previous_state": change.Previous,
		"state":          change.Fact.EffectiveState(),
		"superseded_by":  change.Fact.SupersededBy,
		"fact":           change.Fact,
	})
}
