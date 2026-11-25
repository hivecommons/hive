package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// repoOpenPRsRequestMaxBytes bounds the POST /api/repos/open-prs body.
const repoOpenPRsRequestMaxBytes = 8 << 10

// repoOpenPRsRequest is the body of POST /api/repos/open-prs: the repo and
// its new open-PRs mode (on, off or workflow_only).
type repoOpenPRsRequest struct {
	Repo string `json:"repo"`
	Mode string `json:"mode"`
}

// handleRepoOpenPRs sets one repo's open-PRs mode
// (project.repo_policies[].open_prs), the sibling of the per-repo auto-merge
// switch. It works at every ACMM level.
//
// Asymmetric gate, as for auto-merge (#9070): narrowing (on to
// workflow_only or off, workflow_only to off) only reduces what agents do,
// so the pause tier (owner or repo write) may do it; widening gives agents
// back PR rights on the repo, which is the owner's decision.
func (s *Server) handleRepoOpenPRs(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body repoOpenPRsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, repoOpenPRsRequestMaxBytes)).Decode(&body); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.Repo = strings.TrimSpace(body.Repo)
	if body.Repo == "" {
		jsonError(w, "repo is required", http.StatusBadRequest)
		return
	}
	mode, ok := config.ParseRepoOpenPRsMode(body.Mode)
	if !ok || strings.TrimSpace(body.Mode) == "" {
		jsonError(w, "mode must be on, off or workflow_only", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	prev := cfg.RepoOpenPRsSetting(body.Repo)
	if mode.Widens(prev) {
		if !requireOwnerRole(w, r) {
			return
		}
	} else if !s.requireRepoPausePermission(w, r, body.Repo) {
		return
	}
	if !s.watchesRepo(body.Repo) {
		jsonError(w, "repo "+body.Repo+" is not in project.repos — nothing would be updated", http.StatusBadRequest)
		return
	}
	changed, err := cfg.SetRepoOpenPRsForRepoAndSave(body.Repo, mode)
	if err != nil && s.logger != nil {
		s.logger.Error("failed to persist repo open-PRs mode", "repo", body.Repo, "mode", string(mode), "error", err)
	}
	s.auditFromRequest(r, "repo_open_prs", auditDetail("repo", body.Repo, "mode", string(mode), "previous", string(prev)), "")
	floor := s.refreshAndPersistSeq()
	repo := body.Repo
	if normalized, ok := config.NormalizeRepoForOrg(cfg.Project.Org, body.Repo); ok {
		repo = normalized
	}
	jsonResponse(w, map[string]any{
		"ok":           true,
		"repo":         repo,
		"mode":         string(mode),
		"effective":    string(cfg.RepoOpenPRsMode(body.Repo)),
		"changed":      changed,
		"persisted":    err == nil,
		"minStatusSeq": floor,
	})
}

// repoOpenPRsPayload is one repo's open-PRs entry on GET /api/config/governor
// (Settings → Repos): the repo's own mode and the effective one after the
// hive-wide project.open_prs switch.
func repoOpenPRsPayload(cfg *config.Config, repo string) map[string]any {
	return map[string]any{
		"value":     string(cfg.RepoOpenPRsSetting(repo)),
		"effective": string(cfg.RepoOpenPRsMode(repo)),
	}
}
