package dashboard

import (
	"fmt"
	"net/http"

	"github.com/hivecommons/hive/pkg/beads"
)

func (s *Server) handleBeadsReset(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps.BeadStores == nil {
		jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
		return
	}

	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeBody(r, &body); err != nil {
		body.Reason = "manual reset via API"
	}
	body.Reason = sanitizeString(body.Reason)
	if body.Reason == "" {
		body.Reason = "manual reset via API"
	}

	results := make(map[string]int)
	for name, store := range s.deps.BeadStores {
		closed, err := store.CloseAll(body.Reason)
		if err != nil {
			s.deps.Logger.Error("beads reset failed", "agent", name, "error", err)
		}
		results[name] = closed
	}

	s.auditFromRequest(r, "beads_reset", "", "")
	s.refreshAndPersist()
	jsonResponse(w, map[string]any{"status": "reset", "closed": results, "reason": body.Reason})
}

func (s *Server) handleBeadsResetAgent(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	agentName := r.PathValue("agent")
	if s.deps.BeadStores == nil {
		jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
		return
	}

	store, ok := s.deps.BeadStores[agentName]
	if !ok {
		jsonError(w, fmt.Sprintf("no bead store for agent %q", agentName), http.StatusNotFound)
		return
	}

	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeBody(r, &body); err != nil {
		body.Reason = "manual reset via API"
	}
	body.Reason = sanitizeString(body.Reason)
	if body.Reason == "" {
		body.Reason = "manual reset via API"
	}

	closed, err := store.CloseAll(body.Reason)
	if err != nil {
		jsonError(w, fmt.Sprintf("reset failed: %v", err), http.StatusInternalServerError)
		return
	}

	s.auditFromRequest(r, "beads_reset_agent", "", agentName)
	s.refreshAndPersist()
	jsonResponse(w, map[string]any{"status": "reset", "agent": agentName, "closed": closed, "reason": body.Reason})
}

func (s *Server) handleBeadsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.BeadStores == nil {
		jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
		return
	}

	agentName := r.PathValue("agent")
	result := make(map[string]any)

	if agentName != "" {
		store, ok := s.deps.BeadStores[agentName]
		if !ok {
			jsonError(w, fmt.Sprintf("no bead store for agent %q", agentName), http.StatusNotFound)
			return
		}
		result[agentName] = store.List(beads.ListFilter{})
	} else {
		for name, store := range s.deps.BeadStores {
			result[name] = store.List(beads.ListFilter{})
		}
	}
	jsonResponse(w, result)
}

const maxBeadPriority = 4

func (s *Server) handleBeadsCreate(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	agentName := r.PathValue("agent")
	if s.deps.BeadStores == nil {
		jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
		return
	}

	store, ok := s.deps.BeadStores[agentName]
	if !ok {
		jsonError(w, fmt.Sprintf("no bead store for agent %q", agentName), http.StatusNotFound)
		return
	}

	var body struct {
		Title       string            `json:"title"`
		Type        string            `json:"type"`
		Priority    int               `json:"priority"`
		ExternalRef string            `json:"external_ref"`
		Metadata    map[string]string `json:"metadata"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	body.Title = sanitizeString(body.Title)
	const maxBeadTitleLen = 500
	if body.Title == "" {
		jsonError(w, "title is required", http.StatusBadRequest)
		return
	}
	if len(body.Title) > maxBeadTitleLen {
		jsonError(w, fmt.Sprintf("title too long (%d chars, max %d)", len(body.Title), maxBeadTitleLen), http.StatusBadRequest)
		return
	}
	body.Type = sanitizeString(body.Type)
	if body.Type == "" {
		body.Type = "advisory"
	}
	body.ExternalRef = sanitizeString(body.ExternalRef)
	if body.Priority < 0 || body.Priority > maxBeadPriority {
		jsonError(w, "priority must be 0-4", http.StatusBadRequest)
		return
	}

	b, err := store.Create(body.Title, beads.BeadType(body.Type), beads.Priority(body.Priority), agentName, body.ExternalRef)
	if err != nil {
		jsonError(w, fmt.Sprintf("failed to create bead: %v", err), http.StatusInternalServerError)
		return
	}

	const maxMetadataKeyLen = 100
	const maxMetadataValueLen = 1000
	const maxMetadataEntries = 50
	metaCount := 0
	for k, v := range body.Metadata {
		if metaCount >= maxMetadataEntries || len(k) > maxMetadataKeyLen || len(v) > maxMetadataValueLen {
			continue
		}
		_ = store.SetMetadata(b.ID, k, v)
		metaCount++
	}

	s.auditFromRequest(r, "bead_create", auditDetail("title", body.Title), agentName)
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, b)
}
