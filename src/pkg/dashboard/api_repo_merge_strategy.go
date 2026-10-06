package dashboard

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

type repoMergeStrategyRequest struct {
	Repo          string `json:"repo"`
	MergeStrategy string `json:"merge_strategy"`
}

func (s *Server) repoMergeStrategyResponse(w http.ResponseWriter, repo string, extra map[string]any) {
	cfg := s.deps.Config
	if normalized, ok := config.NormalizeRepoForOrg(cfg.Project.Org, repo); ok {
		repo = normalized
	}
	out := map[string]any{
		"ok":             true,
		"repo":           repo,
		"merge_strategy": cfg.RepoMergeStrategy(repo),
	}
	for k, v := range extra {
		out[k] = v
	}
	jsonResponse(w, out)
}

// handleRepoMergeStrategyGet reports one repo's merge strategy (#10886).
// Read-only, so any authenticated role may call it.
func (s *Server) handleRepoMergeStrategyGet(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if repo == "" {
		jsonError(w, "repo is required", http.StatusBadRequest)
		return
	}
	if !s.watchesRepo(repo) {
		jsonError(w, "repo "+repo+" is not in project.repos", http.StatusBadRequest)
		return
	}
	s.repoMergeStrategyResponse(w, repo, nil)
}

// handleRepoMergeStrategySet changes one repo's merge strategy. The gate is
// asymmetric, mirroring auto-merge (#9070): switching to hive-serialized needs
// the pause/hold tier (owner or repo write); switching back to direct is
// owner-only. The strategy decides how, never whether, so no autonomy-level
// check applies here.
func (s *Server) handleRepoMergeStrategySet(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body repoMergeStrategyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.Repo = strings.TrimSpace(body.Repo)
	body.MergeStrategy = strings.TrimSpace(body.MergeStrategy)
	if body.Repo == "" {
		jsonError(w, "repo is required", http.StatusBadRequest)
		return
	}
	if body.MergeStrategy == "" {
		jsonError(w, "merge_strategy is required", http.StatusBadRequest)
		return
	}
	if err := config.ValidateRepoMergeStrategy(body.Repo, body.MergeStrategy); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.MergeStrategy == config.MergeStrategyDirect {
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
	previous := s.deps.Config.RepoMergeStrategy(body.Repo)
	changed, err := s.deps.Config.SetRepoMergeStrategyForRepoAndSave(body.Repo, body.MergeStrategy)
	if err != nil && !changed {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		s.logger.Error("failed to persist repo merge strategy", "repo", body.Repo, "error", err)
	}
	s.auditFromRequest(r, "repo_merge_strategy", auditDetail("repo", body.Repo, "from", previous, "to", body.MergeStrategy, "changed", strconv.FormatBool(changed)), "")
	s.refreshAndPersist()
	s.repoMergeStrategyResponse(w, body.Repo, map[string]any{"changed": changed, "persisted": err == nil})
}
