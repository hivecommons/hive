package dashboard

import (
	"net/http"
	"sort"
	"strings"
	"time"

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
	jsonResponse(w, s.autoMergeSectionResponseWithBots(s.deps.Config))
}

// autoMergeSectionResponseWithBots is autoMergeSectionResponse plus the
// bot_authors catalogue: every known bot, every bot login currently authoring
// an open PR in a governed repo (discovered from the scheduler's last
// enumeration, never a GitHub call), and whether each is trusted right now.
func (s *Server) autoMergeSectionResponseWithBots(cfg *config.Config) map[string]interface{} {
	resp := autoMergeSectionResponse(cfg)
	var discovered []string
	if s != nil && s.deps != nil && s.deps.Scheduler != nil {
		if a := s.deps.Scheduler.GetLastActionable(); a != nil {
			discovered = discoverBotAuthors(a.PRs.Items, cfg.GitHub.BotLogin())
		}
	}
	resp["bot_authors"] = botAuthorCatalogue(cfg.AutoMerge, discovered)
	return resp
}

// discoverBotAuthors returns the distinct bot logins (GitHub App identities
// end in "[bot]") authoring PRs in the snapshot, excluding the hive's own App.
func discoverBotAuthors(prs []ghpkg.PullRequest, ownLogin string) []string {
	seen := map[string]bool{}
	var out []string
	for _, pr := range prs {
		login := strings.TrimSpace(pr.Author)
		lower := strings.ToLower(login)
		if login == "" || pr.AppAuthored || !strings.HasSuffix(lower, "[bot]") {
			continue
		}
		if ownLogin != "" && strings.EqualFold(login, ownLogin) {
			continue
		}
		if seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, login)
	}
	sort.Strings(out)
	return out
}

// botAuthorCatalogue merges known, discovered, and explicitly configured bot
// logins into one list of {login, source, trusted} rows. source is "known",
// "discovered", or "custom" (configured but neither known nor currently seen).
func botAuthorCatalogue(am config.AutoMergeConfig, discovered []string) []map[string]interface{} {
	trusted := am.TrustedBotAuthorSet()
	type row struct {
		login, source string
	}
	var rows []row
	index := map[string]int{}
	add := func(login, source string) {
		lower := strings.ToLower(strings.TrimSpace(login))
		if lower == "" {
			return
		}
		if _, ok := index[lower]; ok {
			return
		}
		index[lower] = len(rows)
		rows = append(rows, row{login: strings.TrimSpace(login), source: source})
	}
	for _, b := range config.KnownBotAuthors {
		add(b, "known")
	}
	for _, b := range discovered {
		add(b, "discovered")
	}
	configured := am.TrustedBotAuthors
	if configured == nil {
		configured = config.DefaultTrustedBotAuthors
	}
	for _, b := range configured {
		add(b, "custom")
	}
	out := make([]map[string]interface{}, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]interface{}{
			"login":   r.login,
			"source":  r.source,
			"trusted": trusted[strings.ToLower(r.login)],
		})
	}
	return out
}

func normalizeBotLoginList(logins []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(logins))
	for _, l := range logins {
		l = strings.TrimSpace(l)
		lower := strings.ToLower(l)
		if l == "" || seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, l)
	}
	return out
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
		MinHeadAge           *string  `json:"min_head_age"`
		RequiredChecks       []string `json:"required_checks"`
		AllowUnprotectedBase []string `json:"allow_unprotected_base"`
		NoCIOK               []string `json:"no_ci_ok"`
		// TrustedBotAuthors is the FULL desired list; an empty (non-nil) list
		// disables the trusted-bot lane, absent leaves it untouched.
		TrustedBotAuthors []string `json:"trusted_bot_authors"`
		TrustedAuthors    *struct {
			Enabled                 *bool    `json:"enabled"`
			Repos                   []string `json:"repos"`
			RequireRole             *string  `json:"require_role"`
			RequireGitHubPermission *bool    `json:"require_github_permission"`
			ExcludeLabels           []string `json:"exclude_labels"`
		} `json:"trusted_authors"`
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
	var minHeadAge time.Duration
	if body.MinHeadAge != nil {
		parsed, err := time.ParseDuration(strings.TrimSpace(*body.MinHeadAge))
		if err != nil || parsed <= 0 {
			jsonError(w, "min_head_age must be a positive duration (for example \"3m\")", http.StatusBadRequest)
			return
		}
		minHeadAge = parsed
	}
	var trustedAuthorRepos []string
	var trustedAuthorLabels []string
	if body.TrustedAuthors != nil {
		if body.TrustedAuthors.RequireRole != nil {
			role := strings.ToLower(strings.TrimSpace(*body.TrustedAuthors.RequireRole))
			if role != config.RoleMerger && role != config.RoleOwner {
				jsonError(w, "trusted_authors.require_role must be merger or owner", http.StatusBadRequest)
				return
			}
		}
		if body.TrustedAuthors.Repos != nil {
			trustedAuthorRepos = normalizeAutoMergeRepoList(body.TrustedAuthors.Repos)
		}
		if body.TrustedAuthors.ExcludeLabels != nil {
			for _, label := range body.TrustedAuthors.ExcludeLabels {
				if strings.TrimSpace(label) == "" {
					jsonError(w, "trusted_authors.exclude_labels entries must be non-empty strings", http.StatusBadRequest)
					return
				}
			}
			trustedAuthorLabels = normalizeAutoMergeRepoList(body.TrustedAuthors.ExcludeLabels)
		}
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
	if body.MinHeadAge != nil {
		cfg.AutoMerge.MinHeadAge = minHeadAge
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
	if body.TrustedBotAuthors != nil {
		cfg.AutoMerge.TrustedBotAuthors = normalizeBotLoginList(body.TrustedBotAuthors)
	}
	if body.TrustedAuthors != nil {
		if body.TrustedAuthors.Enabled != nil {
			cfg.AutoMerge.TrustedAuthors.Enabled = *body.TrustedAuthors.Enabled
		}
		if body.TrustedAuthors.Repos != nil {
			cfg.AutoMerge.TrustedAuthors.Repos = trustedAuthorRepos
		}
		if body.TrustedAuthors.RequireRole != nil {
			cfg.AutoMerge.TrustedAuthors.RequireRole = strings.ToLower(strings.TrimSpace(*body.TrustedAuthors.RequireRole))
		}
		if body.TrustedAuthors.RequireGitHubPermission != nil {
			v := *body.TrustedAuthors.RequireGitHubPermission
			cfg.AutoMerge.TrustedAuthors.RequireGitHubPermission = &v
		}
		if body.TrustedAuthors.ExcludeLabels != nil {
			cfg.AutoMerge.TrustedAuthors.ExcludeLabels = trustedAuthorLabels
		}
	}
	syncAutoMergePolicyToGitHubClient(cfg, s.deps.GHClient)

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after auto-merge update", "error", err)
	}

	s.auditFromRequest(r, "config_auto_merge", auditDetail("section", "auto_merge"), "")
	s.refreshAndPersist()
	jsonResponse(w, s.autoMergeSectionResponseWithBots(cfg))
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
	trustedBots := am.TrustedBotAuthors
	if trustedBots == nil {
		trustedBots = append([]string{}, config.DefaultTrustedBotAuthors...)
	}
	ta := am.TrustedAuthors
	trustedAuthorRepos := ta.Repos
	if trustedAuthorRepos == nil {
		trustedAuthorRepos = []string{}
	}
	trustedAuthorLabels := ta.ExcludeLabels
	if trustedAuthorLabels == nil {
		trustedAuthorLabels = append([]string{}, config.DefaultTrustedAuthorExcludeLabels...)
	}
	return map[string]interface{}{
		"self_authored":           selfAuthored,
		"self_authored_set":       am.SelfAuthored != nil,
		"max_merges":              am.MaxMerges,
		"min_head_age":            am.EffectiveMinHeadAge().String(),
		"required_checks":         checks,
		"allow_unprotected_base":  allowUnprotected,
		"no_ci_ok":                noCIOK,
		"trusted_bot_authors":     trustedBots,
		"trusted_bot_authors_set": am.TrustedBotAuthors != nil,
		"trusted_authors": map[string]interface{}{
			"enabled":                   ta.Enabled,
			"repos":                     trustedAuthorRepos,
			"require_role":              ta.EffectiveRequireRole(),
			"require_github_permission": ta.EffectiveRequireGitHubPermission(),
			"exclude_labels":            trustedAuthorLabels,
			"roles":                     []string{config.RoleMerger, config.RoleOwner},
		},
	}
}

func syncAutoMergePolicyToGitHubClient(cfg *config.Config, ghClient *ghpkg.Client) {
	if cfg == nil || ghClient == nil {
		return
	}
	set, _ := cfg.AutoMerge.RequiredCheckSet()
	ghClient.SetRequiredChecks(set)
	ghClient.SetRequiredChecksForRepo(cfg.AutoMerge.RequiredCheckSetForRepo)
	ghClient.SetMergeRequestAllowUnprotectedBaseRepos(cfg.AutoMerge.AllowUnprotectedBaseSet())
	ghClient.SetMergeRequestNoCIAllowedRepos(cfg.AutoMerge.NoCIOKSet())
	ghClient.SetAutoMergeMinHeadAge(cfg.AutoMerge.EffectiveMinHeadAge())
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
