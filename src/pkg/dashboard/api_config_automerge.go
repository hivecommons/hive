package dashboard

import (
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// handleAutoMergeGet returns the top-level auto_merge config (AutoMergeConfig)
// so the Governor Features tab can prefill its Auto-Merge controls.
//
// OWNER-ONLY, matching the rest of the governor-config surface: this block
// gates the self-authored merge sweep, so exposing or editing it is an
// operator-level concern.
func (s *Server) handleAutoMergeGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, autoMergeSectionResponse(s.deps.Config))
}

// handleAutoMergePut updates the top-level auto_merge config. Every field is a
// pointer/nilable, so an absent key leaves that setting untouched — the same
// "only what you send is changed" contract the other governor-config writers
// use (see handleGovernorFeatures). saveConfig() persists a secret-free
// overlay to the PVC that the entrypoint merges on restart.
func (s *Server) handleAutoMergePut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		SelfAuthored         *bool    `json:"self_authored"`
		MaxMerges            *int     `json:"max_merges"`
		RequiredChecks       []string `json:"required_checks"`
		AllowUnprotectedBase []string `json:"allow_unprotected_base"`
		NoCIOK               []string `json:"no_ci_ok"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	if body.MaxMerges != nil && *body.MaxMerges < 0 {
		jsonError(w, "max_merges must be 0 (default) or greater", http.StatusBadRequest)
		return
	}

	// --- apply ---
	cfg := s.deps.Config
	if body.SelfAuthored != nil {
		v := *body.SelfAuthored
		cfg.AutoMerge.SelfAuthored = &v
	}
	if body.MaxMerges != nil {
		cfg.AutoMerge.MaxMerges = *body.MaxMerges
	}
	if body.RequiredChecks != nil {
		checks := make([]string, 0, len(body.RequiredChecks))
		for _, c := range body.RequiredChecks {
			c = strings.TrimSpace(c)
			if c != "" {
				checks = append(checks, c)
			}
		}
		cfg.AutoMerge.RequiredChecks = checks
	}
	if body.AllowUnprotectedBase != nil {
		cfg.AutoMerge.AllowUnprotectedBase = normalizeAutoMergeRepoList(body.AllowUnprotectedBase)
	}
	if body.NoCIOK != nil {
		cfg.AutoMerge.NoCIOK = normalizeAutoMergeRepoList(body.NoCIOK)
	}
	syncAutoMergePolicyToGitHubClient(cfg, s.deps.GHClient)

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after auto-merge update", "error", err)
	}

	s.auditFromRequest(r, "config_auto_merge", auditDetail("section", "auto_merge"), "")
	s.refreshAndPersist()
	jsonResponse(w, autoMergeSectionResponse(cfg))
}

func syncAutoMergePolicyToGitHubClient(cfg *config.Config, ghClient *ghpkg.Client) {
	if cfg == nil || ghClient == nil {
		return
	}
	set, _ := cfg.AutoMerge.RequiredCheckSet()
	ghClient.SetRequiredChecks(set)
	ghClient.SetMergeRequestAllowUnprotectedBaseRepos(cfg.AutoMerge.AllowUnprotectedBaseSet())
	ghClient.SetMergeRequestNoCIAllowedRepos(cfg.AutoMerge.NoCIOKSet())
}

// autoMergeSectionResponse renders AutoMergeConfig for the dashboard. The
// self_authored tri-state resolves to its effective default (nil = enabled)
// while selfAuthoredSet lets the UI distinguish an explicit choice from the
// default.
func autoMergeSectionResponse(cfg *config.Config) map[string]interface{} {
	am := cfg.AutoMerge
	selfAuthored := true
	if am.SelfAuthored != nil {
		selfAuthored = *am.SelfAuthored
	}
	checks := am.RequiredChecks
	if checks == nil {
		checks = []string{}
	}
	allowUnprotected := am.AllowUnprotectedBase
	if allowUnprotected == nil {
		allowUnprotected = []string{}
	}
	noCIOK := am.NoCIOK
	if noCIOK == nil {
		noCIOK = []string{}
	}
	return map[string]interface{}{
		"self_authored":          selfAuthored,
		"self_authored_set":      am.SelfAuthored != nil,
		"max_merges":             am.MaxMerges,
		"required_checks":        checks,
		"allow_unprotected_base": allowUnprotected,
		"no_ci_ok":               noCIOK,
	}
}

func normalizeAutoMergeRepoList(repos []string) []string {
	out := make([]string, 0, len(repos))
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo != "" {
			out = append(out, repo)
		}
	}
	return out
}
