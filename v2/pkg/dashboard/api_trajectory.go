package dashboard

import "net/http"

// handleGovernorTrajectory updates the trajectory-review lane config. The
// General tab sends only the enabled toggle; the other fields are optional and
// left untouched when omitted (they're edited via hive.yaml for now). Takes
// effect on the next hive restart, like other governor.trajectory changes.
func (s *Server) handleGovernorTrajectory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Enabled != nil {
		v := *body.Enabled
		s.deps.Config.Governor.Trajectory.Enabled = &v
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after trajectory update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_trajectory", auditDetail("section", "trajectory"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}
