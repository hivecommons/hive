package dashboard

import (
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Release sentinel settings (hivecommons/hive#11854): the Settings controls
// for the top-level release_sentinel block — release_sentinel.enabled,
// release_sentinel.repo, release_sentinel.agent, release_sentinel.max_rounds,
// release_sentinel.round_timeout, release_sentinel.poll_interval,
// release_sentinel.ignore_workflows[], release_sentinel.release_workflows[],
// release_sentinel.release_branch, release_sentinel.retag_enabled and
// release_sentinel.retag_allow_intervening_commits. Mirrors the fleet-report
// PUT: its own small owner-only endpoint, with the current values riding the
// GET /api/config/governor payload under "release_sentinel".

// maxReleaseSentinelRounds bounds the per-release repair-round cap an
// operator may set; the loop exists to stay bounded.
const maxReleaseSentinelRounds = 50

func (s *Server) handleGovernorReleaseSentinelPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Enabled                      *bool    `json:"enabled"`
		Repo                         *string  `json:"repo"`
		Agent                        *string  `json:"agent"`
		MaxRounds                    *int     `json:"max_rounds"`
		RoundTimeout                 *string  `json:"round_timeout"`
		PollInterval                 *string  `json:"poll_interval"`
		IgnoreWorkflows              []string `json:"ignore_workflows"`
		ReleaseWorkflows             []string `json:"release_workflows"`
		ReleaseBranch                *string  `json:"release_branch"`
		RetagEnabled                 *bool    `json:"retag_enabled"`
		RetagAllowInterveningCommits *bool    `json:"retag_allow_intervening_commits"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	if body.Repo != nil {
		repo := strings.TrimSpace(*body.Repo)
		if repo != "" && !validFleetReportRepo(repo) {
			jsonError(w, "repo must be owner/name", http.StatusBadRequest)
			return
		}
	}
	if body.MaxRounds != nil && (*body.MaxRounds < 0 || *body.MaxRounds > maxReleaseSentinelRounds) {
		jsonError(w, "max_rounds must be between 0 and 50", http.StatusBadRequest)
		return
	}
	for _, f := range []struct {
		v    *string
		name string
	}{
		{body.RoundTimeout, "round_timeout"},
		{body.PollInterval, "poll_interval"},
	} {
		if f.v == nil {
			continue
		}
		if v := strings.TrimSpace(*f.v); v != "" {
			if d, err := time.ParseDuration(v); err != nil || d <= 0 {
				jsonError(w, f.name+" must be a positive Go duration such as 90m or 2h, or empty for the default", http.StatusBadRequest)
				return
			}
		}
	}
	if body.ReleaseBranch != nil && strings.ContainsAny(strings.TrimSpace(*body.ReleaseBranch), " \t\n~^:?*[\\") {
		jsonError(w, "release_branch is not a valid branch name", http.StatusBadRequest)
		return
	}

	// --- apply ---
	rs := &s.deps.Config.ReleaseSentinel
	if body.Enabled != nil {
		rs.Enabled = *body.Enabled
	}
	if body.Repo != nil {
		rs.Repo = strings.TrimSpace(*body.Repo)
	}
	if body.Agent != nil {
		rs.Agent = strings.TrimSpace(*body.Agent)
	}
	if body.MaxRounds != nil {
		rs.MaxRounds = *body.MaxRounds
	}
	if body.RoundTimeout != nil {
		rs.RoundTimeout = strings.TrimSpace(*body.RoundTimeout)
	}
	if body.PollInterval != nil {
		rs.PollInterval = strings.TrimSpace(*body.PollInterval)
	}
	if body.IgnoreWorkflows != nil {
		rs.IgnoreWorkflows = cleanSettingsList(body.IgnoreWorkflows)
	}
	if body.ReleaseWorkflows != nil {
		rs.ReleaseWorkflows = cleanSettingsList(body.ReleaseWorkflows)
	}
	if body.ReleaseBranch != nil {
		rs.ReleaseBranch = strings.TrimSpace(*body.ReleaseBranch)
	}
	if body.RetagEnabled != nil {
		rs.RetagEnabled = *body.RetagEnabled
	}
	if body.RetagAllowInterveningCommits != nil {
		rs.RetagAllowInterveningCommits = *body.RetagAllowInterveningCommits
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after release-sentinel update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_release_sentinel", auditDetail("section", "release_sentinel"), "")
	s.refreshAndPersist()
	jsonResponse(w, releaseSentinelSectionResponse(s.deps.Config))
}

// releaseSentinelSectionResponse renders release_sentinel for the Settings
// dialog: the raw configured values (so empty means "default") plus the
// resolved enabled/retag/repo the hive actually acts on, env overrides
// included.
func releaseSentinelSectionResponse(cfg *config.Config) map[string]interface{} {
	rs := cfg.ReleaseSentinel
	return map[string]interface{}{
		"enabled":                         rs.Enabled,
		"effective_enabled":               cfg.ReleaseSentinelEnabled(),
		"repo":                            rs.Repo,
		"effective_repo":                  cfg.ReleaseSentinelRepo(),
		"agent":                           rs.Agent,
		"max_rounds":                      rs.MaxRounds,
		"round_timeout":                   rs.RoundTimeout,
		"poll_interval":                   rs.PollInterval,
		"ignore_workflows":                append([]string{}, rs.IgnoreWorkflows...),
		"release_workflows":               append([]string{}, rs.ReleaseWorkflows...),
		"release_branch":                  rs.ReleaseBranch,
		"retag_enabled":                   rs.RetagEnabled,
		"effective_retag_enabled":         cfg.ReleaseSentinelRetagEnabled(),
		"retag_allow_intervening_commits": rs.RetagAllowInterveningCommits,
	}
}
