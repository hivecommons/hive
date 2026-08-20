package dashboard

import "net/http"

// handleProvidersHeadroom serves GET /api/providers/headroom: the last known
// per-provider headroom snapshot from the rotation manager (RFC #3958).
// Owner-only — headroom exposes subscription usage for the operator's own
// accounts. When rotation is disabled (nil manager) it reports enabled=false
// with an empty provider list rather than erroring.
func (s *Server) handleProvidersHeadroom(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.RotationMgr == nil {
		jsonResponse(w, map[string]interface{}{"providers": []interface{}{}, "enabled": false})
		return
	}
	jsonResponse(w, s.deps.RotationMgr.HeadroomResponse())
}
