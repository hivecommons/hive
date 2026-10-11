package dashboard

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// upstreamWatchRepoPatch is one repo's entry in a PUT /api/config/upstream-watch
// body. POINTER-typed so an absent key leaves that field unchanged.
type upstreamWatchRepoPatch struct {
	Upstream        *string   `json:"upstream,omitempty"`
	Sources         *[]string `json:"sources,omitempty"`
	PRLabels        *[]string `json:"pr_labels,omitempty"`
	MaxIssuesPerRun *int      `json:"max_issues_per_run,omitempty"`
	Label           *string   `json:"label,omitempty"`
	StartFrom       *string   `json:"start_from,omitempty"`
}

// handleUpstreamWatchConfigPut edits the upstream_watch block from the
// Features tab (hivecommons/hive#11211): the global switch, the poll interval,
// and per-repo add / update / remove. A repo mapped to null is removed. The
// candidate block is validated with the loader's own rules BEFORE the live
// config is touched, so a rejected edit leaves nothing half-applied. The poll
// loop reads the live config each tick, so no restart is needed.
func (s *Server) handleUpstreamWatchConfigPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Enabled  *bool                              `json:"enabled,omitempty"`
		Interval *string                            `json:"interval,omitempty"`
		Repos    map[string]*upstreamWatchRepoPatch `json:"repos,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Enabled == nil && body.Interval == nil && len(body.Repos) == 0 {
		jsonError(w, "nothing to update", http.StatusBadRequest)
		return
	}

	cfg := s.deps.Config
	prev := cfg.UpstreamWatch
	next := prev
	next.Repos = make(map[string]config.UpstreamWatchRepo, len(prev.Repos))
	for k, v := range prev.Repos {
		v.Sources = append([]string(nil), v.Sources...)
		v.PRLabels = append([]string(nil), v.PRLabels...)
		next.Repos[k] = v
	}

	if body.Enabled != nil {
		next.Enabled = *body.Enabled
	}
	if body.Interval != nil {
		raw := strings.TrimSpace(*body.Interval)
		if raw == "" {
			next.Interval = 0
		} else {
			d, err := time.ParseDuration(raw)
			if err != nil {
				jsonError(w, "invalid interval "+strconv.Quote(raw)+" (use a duration such as 6h or 30m)", http.StatusBadRequest)
				return
			}
			next.Interval = d
		}
	}

	keys := make([]string, 0, len(body.Repos))
	for k := range body.Repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		patch := body.Repos[key]
		// Store the bare name the Repos tab writes to project.repos, so YAML
		// and the dashboard never hold two spellings of one key.
		bare, _ := config.NormalizeRepoForOrg(cfg.Project.Org, sanitizeString(key))
		if bare == "" {
			jsonError(w, "repo name is required", http.StatusBadRequest)
			return
		}
		existingKey, entry, found := upstreamWatchFindKey(next.Repos, cfg.Project.Org, bare)
		if patch == nil {
			if found {
				delete(next.Repos, existingKey)
			}
			continue
		}
		if found && existingKey != bare {
			delete(next.Repos, existingKey)
		}
		if patch.Upstream != nil {
			entry.Upstream = sanitizeString(*patch.Upstream)
		}
		if patch.Sources != nil {
			entry.Sources = upstreamWatchCleanList(*patch.Sources)
		}
		if patch.PRLabels != nil {
			entry.PRLabels = upstreamWatchCleanList(*patch.PRLabels)
		}
		if patch.MaxIssuesPerRun != nil {
			entry.MaxIssuesPerRun = *patch.MaxIssuesPerRun
		}
		if patch.Label != nil {
			entry.Label = sanitizeString(*patch.Label)
		}
		if patch.StartFrom != nil {
			entry.StartFrom = sanitizeString(*patch.StartFrom)
		}
		next.Repos[bare] = entry
	}
	if len(next.Repos) == 0 {
		next.Repos = nil
	}

	candidate := config.Config{Project: cfg.Project, UpstreamWatch: next}
	if err := candidate.ValidateUpstreamWatch(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	candidate.ApplyUpstreamWatchDefaults()

	cfg.UpstreamWatch = candidate.UpstreamWatch
	if err := s.saveConfig(); err != nil {
		cfg.UpstreamWatch = prev
		s.logger.Error("failed to persist config", "error", err)
		jsonError(w, "failed to persist config", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "config_upstream_watch", auditDetail(
		"section", "upstream_watch",
		"enabled", strconv.FormatBool(cfg.UpstreamWatch.Enabled),
		"repos", strconv.Itoa(len(cfg.UpstreamWatch.Repos)),
	), "")
	okResponse(w, map[string]string{"status": "updated"})
}

// upstreamWatchFindKey finds the existing entry for bare, matching a bare or
// org-qualified key case-insensitively the way validateUpstreamWatch does.
func upstreamWatchFindKey(repos map[string]config.UpstreamWatchRepo, org, bare string) (string, config.UpstreamWatchRepo, bool) {
	if v, ok := repos[bare]; ok {
		return bare, v, true
	}
	for k, v := range repos {
		kb, _ := config.NormalizeRepoForOrg(org, k)
		if strings.EqualFold(kb, bare) {
			return k, v, true
		}
	}
	return "", config.UpstreamWatchRepo{}, false
}

// upstreamWatchCleanList trims and drops blank entries; an empty result is nil
// so the loader's defaults (both sources, all PRs) apply.
func upstreamWatchCleanList(in []string) []string {
	var out []string
	for _, v := range in {
		if v = sanitizeString(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
