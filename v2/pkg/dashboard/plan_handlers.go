package dashboard

import (
	"net/http"

	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/planning"
)

// Plan-review dashboard endpoints (Phase 2 planning intelligence). They mirror
// the /api/inception/* shape: thin HTTP handlers that delegate all logic to
// pkg/planning. Beads live in per-agent stores, so each handler locates the
// store that actually holds the target epic (findEpicStore) before acting.

// findEpicStore returns the bead store that contains an epic bead with epicID,
// or (nil, "") if no store holds it. Planning children live in the same store
// as their epic (Decompose creates them there), so the located store is where
// the whole plan lives.
func (s *Server) findEpicStore(epicID string) (*beads.Store, string) {
	if s.deps == nil || s.deps.BeadStores == nil {
		return nil, ""
	}
	for name, store := range s.deps.BeadStores {
		if b, err := store.Get(epicID); err == nil && b.Type == beads.TypeEpic {
			return store, name
		}
	}
	return nil, ""
}

// handlePlanTree serves GET /api/plan/{epicID}: the review view of a decomposed
// epic (the epic + its children with execution tags and dependency edges).
func (s *Server) handlePlanTree(w http.ResponseWriter, r *http.Request) {
	epicID := r.PathValue("epicID")
	store, _ := s.findEpicStore(epicID)
	if store == nil {
		jsonError(w, "epic not found in any bead store", http.StatusNotFound)
		return
	}
	tree, err := planning.GetPlanTree(store, epicID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "plan": tree})
}

// handlePlanApprove serves POST /api/plan/{epicID}/approve: approve the plan,
// releasing its children through Ready().
func (s *Server) handlePlanApprove(w http.ResponseWriter, r *http.Request) {
	epicID := r.PathValue("epicID")
	store, agentName := s.findEpicStore(epicID)
	if store == nil {
		jsonError(w, "epic not found in any bead store", http.StatusNotFound)
		return
	}
	if err := planning.ApprovePlan(store, epicID); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.auditFromRequest(r, "plan_approve", auditDetail("epic", epicID), agentName)
	s.refreshAndPersist()
	tree, _ := planning.GetPlanTree(store, epicID)
	jsonResponse(w, map[string]interface{}{"ok": true, "status": "approved", "plan": tree})
}

// handlePlanReject serves POST /api/plan/{epicID}/reject: return an approved (or
// draft) plan to draft state, re-gating its children.
func (s *Server) handlePlanReject(w http.ResponseWriter, r *http.Request) {
	epicID := r.PathValue("epicID")
	store, agentName := s.findEpicStore(epicID)
	if store == nil {
		jsonError(w, "epic not found in any bead store", http.StatusNotFound)
		return
	}
	if err := planning.RejectPlan(store, epicID); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.auditFromRequest(r, "plan_reject", auditDetail("epic", epicID), agentName)
	s.refreshAndPersist()
	tree, _ := planning.GetPlanTree(store, epicID)
	jsonResponse(w, map[string]interface{}{"ok": true, "status": "draft", "plan": tree})
}

// handlePlanChild serves POST /api/plan/{epicID}/child/{childID}: edit a child
// before approval. Body {"action":"retag","execution":"agent_suitable"} retags;
// {"action":"remove"} removes (closes) the child.
func (s *Server) handlePlanChild(w http.ResponseWriter, r *http.Request) {
	epicID := r.PathValue("epicID")
	childID := r.PathValue("childID")
	store, agentName := s.findEpicStore(epicID)
	if store == nil {
		jsonError(w, "epic not found in any bead store", http.StatusNotFound)
		return
	}

	var body struct {
		Action    string `json:"action"`
		Execution string `json:"execution"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	switch body.Action {
	case "retag":
		if err := planning.RetagChild(store, epicID, childID, sanitizeString(body.Execution)); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.auditFromRequest(r, "plan_child_retag", auditDetail("epic", epicID, "child", childID, "execution", body.Execution), agentName)
	case "remove":
		if err := planning.RemoveChild(store, epicID, childID); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.auditFromRequest(r, "plan_child_remove", auditDetail("epic", epicID, "child", childID), agentName)
	default:
		jsonError(w, "action must be 'retag' or 'remove'", http.StatusBadRequest)
		return
	}

	s.refreshAndPersist()
	tree, _ := planning.GetPlanTree(store, epicID)
	jsonResponse(w, map[string]interface{}{"ok": true, "plan": tree})
}
