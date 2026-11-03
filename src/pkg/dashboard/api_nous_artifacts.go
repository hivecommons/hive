package dashboard

import (
	"net/http"
)

// nousArtifactSyncAudit is the audit action recorded for every owner-
// triggered artifact mirror pass, successful or not.
const nousArtifactSyncAudit = "nous_artifact_sync"

// handleNousArtifactSync serves POST /api/nous/artifacts/sync: the owner's
// "Sync now" button for the optional GitHub artifact repository (#11701).
// It forces one pass of the mirror (bypassing the retry backoff, never the
// API budget governor) and returns the pass result. The mirror itself lives
// in cmd/hive; the dashboard only sees the NousState.ArtifactSync seam.
func (s *Server) handleNousArtifactSync(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps.Nous == nil || s.deps.Nous.ArtifactSync == nil {
		jsonError(w, "artifact repository not configured (nous.artifact_repo)", http.StatusNotFound)
		return
	}
	result, err := s.deps.Nous.ArtifactSync(r.Context())
	if err != nil {
		s.auditFromRequest(r, nousArtifactSyncAudit, "error: "+err.Error(), "")
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.auditFromRequest(r, nousArtifactSyncAudit, "", "")
	jsonResponse(w, map[string]interface{}{"status": "synced", "result": result})
}
