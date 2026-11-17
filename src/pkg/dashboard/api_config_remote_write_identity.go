package dashboard

import (
	"net/http"

	"github.com/hivecommons/hive/pkg/config"
)

// PUT /api/config/reference-repos/write-identity (hivecommons/hive#18516):
// the Settings → Repos → Reference repos control for
// project.remote_write_identity, which picks whose name the hive's writes to
// granted remote repos appear under: "bot" (the App bot where the repo's
// owner installed the App, else the owner's connected account), "bot_only"
// (the App bot or nothing) or "owner" (the owner's connected account). It
// never widens what the hive may write; that stays the reference-repo grant.
//
// OWNER-ONLY, like the reference repos editor: it changes whose name is on
// public posts. The change is refused before anything is stored when a
// hand-written write entry would have no credential under the new value
// (the error boot would stop on), and is audit-logged with both values.

// handleRemoteWriteIdentityPut stores project.remote_write_identity and
// re-resolves the reference repos. Body: {"write_identity": "bot"}. The
// response is the reference repos section as GET /api/config/reference-repos
// returns it, resolved under the new value.
func (s *Server) handleRemoteWriteIdentityPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		WriteIdentity *string `json:"write_identity"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.WriteIdentity == nil {
		jsonError(w, `write_identity is required: one of "bot", "bot_only", "owner"`, http.StatusBadRequest)
		return
	}
	if err := config.ValidateRemoteWriteIdentity(*body.WriteIdentity); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	next := config.NormalizeRemoteWriteIdentity(*body.WriteIdentity)
	cfg := s.deps.Config
	prevRaw := cfg.Project.RemoteWriteIdentity
	prev := cfg.Project.EffectiveRemoteWriteIdentity()
	if next == prev && prevRaw != "" {
		jsonResponse(w, s.referenceReposSectionFor(cfg))
		return
	}
	if s.deps.CheckRemoteWriteIdentity != nil {
		if err := s.deps.CheckRemoteWriteIdentity(r.Context(), next); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	cfg.Project.RemoteWriteIdentity = next
	if err := s.saveConfig(); err != nil {
		cfg.Project.RemoteWriteIdentity = prevRaw
		if s.logger != nil {
			s.logger.Error("failed to persist config after remote write identity update", "error", err)
		}
		jsonError(w, "failed to save config; write identity unchanged", http.StatusInternalServerError)
		return
	}
	if s.deps.RemoteWriteIdentityChanged != nil {
		s.deps.RemoteWriteIdentityChanged()
	}
	s.auditFromRequest(r, "config_remote_write_identity", auditDetail(
		"section", "reference_repos",
		"setting", "project.remote_write_identity",
		"previous", prev,
		"write_identity", next,
	), "")
	if s.logger != nil {
		s.logger.Info("remote write identity updated via runtime settings", "previous", prev, "write_identity", next)
	}
	jsonResponse(w, s.referenceReposSectionFor(cfg))
}
