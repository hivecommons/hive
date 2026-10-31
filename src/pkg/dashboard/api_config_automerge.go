package dashboard

import (
	"fmt"
	"net/http"
	"regexp"
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
		NativeMergeQueue     *nativeQueueSettingsPatch `json:"native_merge_queue"`
		SelfAuthored         *bool                     `json:"self_authored"`
		MaxMerges            *int                      `json:"max_merges"`
		MinHeadAge           *string                   `json:"min_head_age"`
		RequiredChecks       []string                  `json:"required_checks"`
		AllowUnprotectedBase []string                  `json:"allow_unprotected_base"`
		NoCIOK               []string                  `json:"no_ci_ok"`
		// TrustedBotAuthors is the FULL desired list; an empty (non-nil) list
		// disables the trusted-bot lane, absent leaves it untouched.
		TrustedBotAuthors []string `json:"trusted_bot_authors"`
		// BotTakeoverEnabled is auto_merge.bot_takeover.enabled; flat so the
		// dialog's plain toggle can stage it.
		BotTakeoverEnabled *bool `json:"bot_takeover_enabled"`
		// RequireCurrentBaseDefault is auto_merge.require_current_base_default,
		// the Features toggle "Merge only on current base" (#15335). Per-repo
		// overrides travel on PUT /api/config/governor/repos.
		RequireCurrentBaseDefault *bool `json:"require_current_base_default"`
		TrustedAuthors            *struct {
			Enabled                 *bool    `json:"enabled"`
			Repos                   []string `json:"repos"`
			RequireRole             *string  `json:"require_role"`
			RequireGitHubPermission *bool    `json:"require_github_permission"`
			ExcludeLabels           []string `json:"exclude_labels"`
		} `json:"trusted_authors"`
		ContributorLane *struct {
			Enabled                            *bool    `json:"enabled"`
			SoakMinutes                        *int     `json:"soak_minutes"`
			RequireReviewerVerdict             *bool    `json:"require_reviewer_verdict"`
			Repos                              []string `json:"repos"`
			ExcludeLabels                      []string `json:"exclude_labels"`
			RequireTiers                       []string `json:"require_tiers"`
			RegistryTiersSufficient            *bool    `json:"registry_tiers_sufficient"`
			AutoPromotionScope                 *string  `json:"auto_promotion_scope"`
			ApproveHeldRunsForRegistryTrusted  *bool    `json:"approve_held_runs_for_registry_trusted"`
			OwnerApprovalReleasesWorkflowFence *bool    `json:"owner_approval_releases_workflow_fence"`
		} `json:"contributor_lane"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	nativeQueue, err := updateNativeQueueSettings(s.deps.Config.AutoMerge.NativeMergeQueue, body.NativeMergeQueue)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
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
	var contributorLaneRepos []string
	var contributorLaneLabels []string
	var contributorLaneTiers []string
	var contributorLaneScope string
	if body.ContributorLane != nil {
		if body.ContributorLane.SoakMinutes != nil && *body.ContributorLane.SoakMinutes < 0 {
			jsonError(w, "contributor_lane.soak_minutes must be 0 (default 30) or greater", http.StatusBadRequest)
			return
		}
		if body.ContributorLane.Repos != nil {
			contributorLaneRepos = normalizeAutoMergeRepoList(body.ContributorLane.Repos)
		}
		if body.ContributorLane.ExcludeLabels != nil {
			for _, label := range body.ContributorLane.ExcludeLabels {
				if strings.TrimSpace(label) == "" {
					jsonError(w, "contributor_lane.exclude_labels entries must be non-empty strings", http.StatusBadRequest)
					return
				}
			}
			contributorLaneLabels = normalizeAutoMergeRepoList(body.ContributorLane.ExcludeLabels)
		}
		if body.ContributorLane.RequireTiers != nil {
			for _, tier := range body.ContributorLane.RequireTiers {
				if !config.IsContributorLaneGrantableTier(tier) {
					jsonError(w, "contributor_lane.require_tiers entries must be operator-granted tiers ("+strings.Join(config.ContributorLaneGrantableTiers, ", ")+")", http.StatusBadRequest)
					return
				}
			}
			contributorLaneTiers = normalizeAutoMergeRepoList(body.ContributorLane.RequireTiers)
		}
		if body.ContributorLane.AutoPromotionScope != nil {
			scope := config.ContributorLaneAutoMergeConfig{AutoPromotionScope: *body.ContributorLane.AutoPromotionScope}
			if err := scope.Validate(); err != nil {
				jsonError(w, "contributor_lane.auto_promotion_scope must be one of "+strings.Join(config.ContributorLaneAutoPromotionScopes, ", "), http.StatusBadRequest)
				return
			}
			contributorLaneScope = scope.EffectiveAutoPromotionScope()
		}
	}

	// --- apply ---
	cfg := s.deps.Config
	previousAutoMerge := cfg.AutoMerge
	if body.NativeMergeQueue != nil {
		cfg.AutoMerge.NativeMergeQueue = nativeQueue
	}
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
	if body.BotTakeoverEnabled != nil {
		v := *body.BotTakeoverEnabled
		cfg.AutoMerge.BotTakeover.Enabled = &v
	}
	if body.RequireCurrentBaseDefault != nil {
		v := *body.RequireCurrentBaseDefault
		cfg.AutoMerge.RequireCurrentBaseDefault = &v
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
	if body.ContributorLane != nil {
		if body.ContributorLane.Enabled != nil {
			v := *body.ContributorLane.Enabled
			cfg.AutoMerge.ContributorLane.Enabled = &v
		}
		if body.ContributorLane.SoakMinutes != nil {
			cfg.AutoMerge.ContributorLane.SoakMinutes = *body.ContributorLane.SoakMinutes
		}
		if body.ContributorLane.RequireReviewerVerdict != nil {
			v := *body.ContributorLane.RequireReviewerVerdict
			cfg.AutoMerge.ContributorLane.RequireReviewerVerdict = &v
		}
		if body.ContributorLane.RegistryTiersSufficient != nil {
			v := *body.ContributorLane.RegistryTiersSufficient
			cfg.AutoMerge.ContributorLane.RegistryTiersSufficient = &v
		}
		if body.ContributorLane.ApproveHeldRunsForRegistryTrusted != nil {
			v := *body.ContributorLane.ApproveHeldRunsForRegistryTrusted
			cfg.AutoMerge.ContributorLane.ApproveHeldRunsForRegistryTrusted = &v
		}
		if body.ContributorLane.OwnerApprovalReleasesWorkflowFence != nil {
			v := *body.ContributorLane.OwnerApprovalReleasesWorkflowFence
			cfg.AutoMerge.ContributorLane.OwnerApprovalReleasesWorkflowFence = &v
		}
		if body.ContributorLane.Repos != nil {
			cfg.AutoMerge.ContributorLane.Repos = contributorLaneRepos
		}
		if body.ContributorLane.ExcludeLabels != nil {
			cfg.AutoMerge.ContributorLane.ExcludeLabels = contributorLaneLabels
		}
		if body.ContributorLane.RequireTiers != nil {
			cfg.AutoMerge.ContributorLane.RequireTiers = contributorLaneTiers
		}
		if body.ContributorLane.AutoPromotionScope != nil {
			cfg.AutoMerge.ContributorLane.AutoPromotionScope = contributorLaneScope
		}
	}
	syncAutoMergePolicyToGitHubClient(cfg, s.deps.GHClient)

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after auto-merge update", "error", err)
		if body.NativeMergeQueue != nil {
			cfg.AutoMerge = previousAutoMerge
			syncAutoMergePolicyToGitHubClient(cfg, s.deps.GHClient)
			jsonError(w, "failed to persist native queue settings", http.StatusInternalServerError)
			return
		}
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
	cl := am.ContributorLane
	contributorLaneRepos := cl.Repos
	if contributorLaneRepos == nil {
		contributorLaneRepos = []string{}
	}
	contributorLaneLabels := cl.ExcludeLabels
	if contributorLaneLabels == nil {
		contributorLaneLabels = append([]string{}, config.DefaultContributorLaneExcludeLabels...)
	}
	return map[string]interface{}{
		"self_authored":                selfAuthored,
		"self_authored_set":            am.SelfAuthored != nil,
		"max_merges":                   am.MaxMerges,
		"min_head_age":                 am.EffectiveMinHeadAge().String(),
		"required_checks":              checks,
		"allow_unprotected_base":       allowUnprotected,
		"no_ci_ok":                     noCIOK,
		"trusted_bot_authors":          trustedBots,
		"trusted_bot_authors_set":      am.TrustedBotAuthors != nil,
		"bot_takeover_enabled":         am.BotTakeover.IsEnabled(),
		"require_current_base_default": am.CurrentBaseDefaultEnabled(),
		"require_current_base":         emptyIfNilStrings(am.RequireCurrentBase),
		"skip_current_base":            emptyIfNilStrings(am.SkipCurrentBase),
		"native_merge_queue": map[string]interface{}{
			"enabled":                am.NativeMergeQueue.Enabled,
			"repos":                  emptyIfNilStrings(am.NativeMergeQueue.Repos),
			"bases":                  emptyIfNilStrings(am.NativeMergeQueue.Bases),
			"skip_current_base_gate": am.NativeMergeQueue.SkipCurrentBaseGate,
			"probe_ttl":              am.NativeMergeQueue.EffectiveProbeTTL().String(),
		},
		"trusted_authors": map[string]interface{}{
			"enabled":                   ta.Enabled,
			"repos":                     trustedAuthorRepos,
			"require_role":              ta.EffectiveRequireRole(),
			"require_github_permission": ta.EffectiveRequireGitHubPermission(),
			"exclude_labels":            trustedAuthorLabels,
			"roles":                     []string{config.RoleMerger, config.RoleOwner},
		},
		"contributor_lane": map[string]interface{}{
			"enabled":                                cl.IsEnabled(),
			"soak_minutes":                           cl.EffectiveSoakMinutes(),
			"require_reviewer_verdict":               cl.EffectiveRequireReviewerVerdict(),
			"repos":                                  contributorLaneRepos,
			"exclude_labels":                         contributorLaneLabels,
			"require_tiers":                          cl.EffectiveRequireTiers(),
			"registry_tiers_sufficient":              cl.EffectiveRegistryTiersSufficient(),
			"auto_promotion_scope":                   cl.EffectiveAutoPromotionScope(),
			"auto_promotion_scopes":                  config.ContributorLaneAutoPromotionScopes,
			"grantable_tiers":                        config.ContributorLaneGrantableTiers,
			"approve_held_runs_for_registry_trusted": cl.EffectiveApproveHeldRunsForRegistryTrusted(),
			"owner_approval_releases_workflow_fence": cl.EffectiveOwnerApprovalReleasesWorkflowFence(),
		},
	}
}

type nativeQueueSettingsPatch struct {
	Enabled             *bool    `json:"enabled"`
	Repos               []string `json:"repos"`
	Bases               []string `json:"bases"`
	SkipCurrentBaseGate *bool    `json:"skip_current_base_gate"`
	ProbeTTL            *string  `json:"probe_ttl"`
}

var nativeQueueRepoPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*/[a-zA-Z0-9_.-]+$`)
var nativeQueueBasePattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_./-]*$`)

func updateNativeQueueSettings(current config.NativeMergeQueueAutoMergeConfig, patch *nativeQueueSettingsPatch) (config.NativeMergeQueueAutoMergeConfig, error) {
	if patch == nil {
		return current, nil
	}
	next := current
	if patch.Enabled != nil {
		next.Enabled = *patch.Enabled
	}
	if patch.SkipCurrentBaseGate != nil {
		next.SkipCurrentBaseGate = *patch.SkipCurrentBaseGate
	}
	if patch.ProbeTTL != nil {
		ttl, err := time.ParseDuration(strings.TrimSpace(*patch.ProbeTTL))
		if err != nil || ttl < 0 {
			return current, fmt.Errorf("native_merge_queue.probe_ttl must be a non-negative duration (0 uses 30m)")
		}
		next.ProbeTTL = ttl
	}
	if patch.Repos != nil {
		for _, repo := range patch.Repos {
			repo = strings.TrimSpace(repo)
			if !nativeQueueRepoPattern.MatchString(repo) || strings.HasSuffix(repo, "/.") || strings.HasSuffix(repo, "/..") {
				return current, fmt.Errorf("native_merge_queue.repos must contain exact owner/repo names")
			}
		}
		next.Repos = normalizeAutoMergeRepoList(patch.Repos)
	}
	if patch.Bases != nil {
		next.Bases = make([]string, 0, len(patch.Bases))
		seen := map[string]bool{}
		for _, scope := range patch.Bases {
			repo, base, ok := strings.Cut(strings.TrimSpace(scope), "#")
			if !ok || !nativeQueueRepoPattern.MatchString(repo) || !nativeQueueBasePattern.MatchString(base) ||
				strings.Contains(base, "..") || strings.Contains(base, "//") || strings.HasSuffix(base, "/") || strings.HasSuffix(base, ".") {
				return current, fmt.Errorf("native_merge_queue.bases must contain exact owner/repo#base pairs (no wildcards)")
			}
			for _, part := range strings.Split(base, "/") {
				if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
					return current, fmt.Errorf("native_merge_queue.bases contains an invalid branch name")
				}
			}
			key := strings.ToLower(repo) + "#" + base
			if !seen[key] {
				next.Bases = append(next.Bases, key)
				seen[key] = true
			}
		}
	}
	// Disabling always remains possible, including legacy unscoped YAML.
	if next.Enabled {
		if len(next.Repos) == 0 || len(next.Bases) == 0 {
			return current, fmt.Errorf("enabling native merge queue in Settings requires explicit repositories and owner/repo#base pairs")
		}
		for _, scope := range next.Bases {
			repo, _, _ := strings.Cut(scope, "#")
			if !next.RepoSet()[strings.ToLower(repo)] {
				return current, fmt.Errorf("native_merge_queue.bases repository must also appear in repos")
			}
		}
	}
	return next, nil
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

func emptyIfNilStrings(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
