// Governor configuration endpoints: sensing, thresholds/scaling, labels,
// budget, notifications, health, logging, attribution, hub/contribute config,
// LiteLLM probing/configuration, Bob key handling, agent/repo management, and
// repo access checks. Split out of api.go's `// --- Governor config endpoints ---`
// section per #6570 (slice 2/5), stacking on slice 1 (#6579).
package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// --- Governor config endpoints ---

func (s *Server) handleGovernorConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config

	// Build agents list
	agents := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		agents = append(agents, name)
	}

	// Extract thresholds from modes (exclude idle which is always 0)
	thresholds := map[string]int{}
	for modeName, mode := range cfg.Governor.Modes {
		if modeName != "idle" {
			thresholds[modeName] = mode.Threshold
		}
	}

	// The raw map above is what the operator SET (0 = unset). The governor
	// ladders on the resolved values, which for an unset mode are the defaults
	// scaled by repo count (#3498). Send both, so the settings panel can show
	// the numbers actually in force next to the ones being edited instead of
	// falling back to its own copy of the unscaled defaults.
	repoCount := cfg.Project.RepoCount()
	effectiveThresholds := map[string]int{
		"quiet": cfg.Governor.EffectiveThreshold("quiet", repoCount),
		"busy":  cfg.Governor.EffectiveThreshold("busy", repoCount),
		"surge": cfg.Governor.EffectiveThreshold("surge", repoCount),
	}

	// Build full org/repo paths
	org := cfg.Project.Org
	repos := make([]string, 0, len(cfg.Project.Repos))
	for _, repo := range cfg.Project.Repos {
		if strings.Contains(repo, "/") {
			repos = append(repos, repo)
		} else {
			repos = append(repos, org+"/"+repo)
		}
	}

	// Build notifications — mask sensitive values like the old hive does
	notifications := map[string]interface{}{
		"ntfyServer":     "",
		"ntfyTopic":      "",
		"discordWebhook": "",
		"hasNtfy":        false,
		"hasDiscord":     false,
	}
	if cfg.Notifications.Ntfy != nil {
		notifications["ntfyServer"] = cfg.Notifications.Ntfy.Server
		notifications["ntfyTopic"] = cfg.Notifications.Ntfy.Topic
		notifications["hasNtfy"] = cfg.Notifications.Ntfy.Server != ""
	}
	if cfg.Notifications.Discord != nil {
		notifications["discordWebhook"] = maskSecret(cfg.Notifications.Discord.Webhook)
		notifications["hasDiscord"] = cfg.Notifications.Discord.Webhook != ""
	}

	primaryRepo := cfg.Project.PrimaryRepo
	if primaryRepo != "" && org != "" && !strings.Contains(primaryRepo, "/") {
		primaryRepo = org + "/" + primaryRepo
	}

	jsonResponse(w, map[string]interface{}{
		"agents":              agents,
		"thresholds":          thresholds,
		"effectiveThresholds": effectiveThresholds,
		"thresholdScaling":    cfg.Governor.ThresholdScalingMode(),
		"repoCount":           repoCount,
		"labels":              cfg.Governor.Labels.Exempt,
		"holdLabels":          github.HoldLabels,
		// requireLabels is the OPPOSITE polarity from "labels" (exempt):
		// project.issue_filter.require_labels — when non-empty, agents may
		// ONLY initiate work on issues carrying at least one of them. Edited
		// on the same Labels tab so operators have one place for label policy.
		"requireLabels": cfg.Project.IssueFilter.RequireLabels,
		"repos":         repos,
		"primaryRepo":   primaryRepo,
		"budget": map[string]interface{}{
			"totalTokens": cfg.Governor.Budget.TotalTokens,
			"periodDays":  cfg.Governor.Budget.PeriodDays,
			"criticalPct": cfg.Governor.Budget.CriticalPct,
		},
		"notifications": notifications,
		"health": map[string]interface{}{
			"healthcheckInterval": cfg.Governor.Health.HealthcheckInterval,
			"restartCooldown":     cfg.Governor.Health.RestartCooldown,
			"modelLock":           cfg.Governor.Health.ModelLock,
			"watchdog":            watchdogConfigPayload(cfg),
		},
		"sensing": map[string]interface{}{
			"ghRatePatterns":     cfg.Governor.Sensing.GHRatePatterns,
			"cliExcludePatterns": cfg.Governor.Sensing.CLIExcludePatterns,
			"loginPatterns":      cfg.Governor.Sensing.LoginPatterns,
			"ttlSeconds":         cfg.Governor.Sensing.TTLSeconds,
			"pullbackSeconds":    cfg.Governor.Sensing.PullbackSeconds,
		},
		"logging": map[string]interface{}{
			"dir":        cfg.Governor.Logging.Dir,
			"maxSizeMB":  cfg.Governor.Logging.MaxSizeMB,
			"maxAgeDays": cfg.Governor.Logging.MaxAgeDays,
			"maxBackups": cfg.Governor.Logging.MaxBackups,
			"compress":   cfg.Governor.Logging.Compress,
			"level":      cfg.Governor.Logging.Level,
		},
		"litellm":               litellmSectionResponse(&cfg.Governor.LiteLLM),
		"trajectory":            trajectorySectionResponse(&cfg.Governor),
		"classifier":            classifierSectionResponse(),
		"features":              featuresSectionResponse(cfg),
		"review":                cfg.Review,
		"auto_merge":            autoMergeSectionResponse(cfg),
		"convergence":           s.convergenceSectionResponse(cfg),
		"advisory":              advisorySectionResponse(cfg),
		"project_observability": s.projectObservabilityResponse(cfg),
		"replan":                replanSectionResponse(cfg),
		"work_source":           workSourceSectionResponse(cfg),
		"security":              securitySectionResponse(cfg),
		"attribution": map[string]interface{}{
			// Effective value (default ON when unset) — the UI renders the
			// switch from this, so an untouched hive shows it on.
			"attributionTrailer": cfg.Governor.AttributionTrailerEnabled(),
		},
		"general_advanced": generalAdvancedSectionResponse(cfg),
		"hub": map[string]interface{}{
			"enabled": cfg.Hub.Enabled,
			// namespace is read-only, runtime-derived display info (never
			// persisted to hive.yaml, never accepted back on save) — the
			// Kubernetes namespace this pod is actually running in. See
			// podNamespace's doc comment for the POD_NAMESPACE/NAMESPACE/
			// service-account-file fallback chain. Omitted (empty string)
			// outside a cluster, which the Hub tab renders by skipping the
			// line rather than showing a blank/"undefined" value.
			"namespace":                          podNamespace(),
			"url":                                cfg.Hub.URL,
			"dashboard_url":                      cfg.Hub.DashboardURL,
			"snapshot_url":                       cfg.Hub.SnapshotURL,
			"is_public":                          cfg.Hub.IsPublic,
			"auto_snapshot":                      cfg.Hub.AutoSnapshot,
			"snapshot_frame_ancestors":           cfg.Dashboard.SnapshotFrameAncestors,
			"auto_upgrade":                       cfg.Hub.AutoUpgrade,
			"snapshot_interval_min":              cfg.Hub.SnapshotIntervalMin,
			"contribute_suspended":               cfg.Hub.ContributeSuspended,
			"contribute_titles_mode":             cfg.Hub.ContributeTitlesMode,
			"contribute_authors_mode":            cfg.Hub.ContributeAuthorsMode,
			"contribute_labels_mode":             cfg.Hub.ContributeLabelsMode,
			"contribute_allow_labels":            cfg.Hub.ContributeAllowLabels,
			"contribute_deny_labels":             cfg.Hub.ContributeDenyLabels,
			"contribute_deny_titles":             cfg.Hub.ContributeDenyTitles,
			"contribute_deny_authors":            cfg.Hub.ContributeDenyAuthors,
			"contribute_allow_models":            cfg.Hub.ContributeAllowModels,
			"contribute_reject_unknown_models":   cfg.Hub.ContributeRejectUnknownModels,
			"contribute_skip_assigned_to_others": cfg.Hub.ContributeSkipAssignedToOthers,
			// Cooldown toggle + period. contribute_cooldown_enabled is the RESOLVED
			// on/off (nil pointer -> true) so both UI surfaces render a concrete
			// switch state; contribute_cooldown_hours is the EFFECTIVE period (the
			// 168h default surfaces when unset) so the number input shows the value
			// actually in force.
			"contribute_cooldown_enabled":  cfg.Hub.IsContributeCooldownEnabled(),
			"contribute_cooldown_hours":    cfg.Hub.ContributeCooldownHoursOrDefault(),
			"contribute_delegatable_roles": normalizeContributeDelegatableRoles(cfg.Hub.ContributeDelegatableRoles),
			"disabled_repos":               cfg.Hub.DisabledRepos,
			"disabled_tiers":               cfg.Hub.DisabledTiers,
			"tier_limits":                  cfg.Hub.TierLimits,
			// available_repos is the READ-ONLY list of repo full-names the hive knows
			// about (from the live status snapshot), so the Management tab can render
			// a per-repo enable toggle mirror of the Governor Hub "Repos for Contribute"
			// list. A repo is ENABLED unless it appears in disabled_repos.
			"available_repos": s.contributeAvailableRepos(),
		},
	})
}

// contributeAvailableRepos returns the sorted, de-duplicated set of repo full-names
// the hive currently knows about (from the status snapshot). Read-only; used to
// render the "Repos for Contribute" enable toggles in the Management tab mirror.
func (s *Server) contributeAvailableRepos() []string {
	seen := map[string]struct{}{}
	var out []string
	s.statusMu.RLock()
	if s.status != nil {
		for _, repo := range s.status.Repos {
			name := repo.Full
			if name == "" {
				name = repo.Name
			}
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	s.statusMu.RUnlock()
	sort.Strings(out)
	return out
}

func (s *Server) handleGovernorSensing(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		EvalIntervalS      int      `json:"eval_interval_s"`
		GHRatePatterns     []string `json:"ghRatePatterns"`
		CLIExcludePatterns []string `json:"cliExcludePatterns"`
		LoginPatterns      []string `json:"loginPatterns"`
		TTLSeconds         int      `json:"ttlSeconds"`
		PullbackSeconds    int      `json:"pullbackSeconds"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	const minEvalIntervalS = 10    // 10 seconds minimum
	const maxEvalIntervalS = 86400 // 24 hours
	if body.EvalIntervalS != 0 {
		if body.EvalIntervalS < minEvalIntervalS || body.EvalIntervalS > maxEvalIntervalS {
			jsonError(w, fmt.Sprintf("eval_interval_s must be between %d and %d", minEvalIntervalS, maxEvalIntervalS), http.StatusBadRequest)
			return
		}
		s.deps.Config.Governor.EvalIntervalS = body.EvalIntervalS
	}
	if body.GHRatePatterns != nil {
		for _, p := range body.GHRatePatterns {
			if _, err := regexp.Compile(p); err != nil {
				jsonError(w, fmt.Sprintf("invalid ghRatePattern regex %q: %v", p, err), http.StatusBadRequest)
				return
			}
		}
		s.deps.Config.Governor.Sensing.GHRatePatterns = body.GHRatePatterns
	}
	if body.CLIExcludePatterns != nil {
		for _, p := range body.CLIExcludePatterns {
			if _, err := regexp.Compile(p); err != nil {
				jsonError(w, fmt.Sprintf("invalid cliExcludePattern regex %q: %v", p, err), http.StatusBadRequest)
				return
			}
		}
		s.deps.Config.Governor.Sensing.CLIExcludePatterns = body.CLIExcludePatterns
	}
	if body.LoginPatterns != nil {
		var filtered []string
		for _, p := range body.LoginPatterns {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, err := regexp.Compile(p); err != nil {
				jsonError(w, fmt.Sprintf("invalid login pattern regex %q: %v", p, err), http.StatusBadRequest)
				return
			}
			filtered = append(filtered, p)
		}
		s.deps.Config.Governor.Sensing.LoginPatterns = filtered
	}
	const maxTTLSeconds = 86400 // 24 hours
	if body.TTLSeconds != 0 {
		if body.TTLSeconds < 1 || body.TTLSeconds > maxTTLSeconds {
			jsonError(w, fmt.Sprintf("ttlSeconds must be between 1 and %d", maxTTLSeconds), http.StatusBadRequest)
			return
		}
		s.deps.Config.Governor.Sensing.TTLSeconds = body.TTLSeconds
	}
	const maxPullbackSeconds = 86400 // 24 hours
	if body.PullbackSeconds != 0 {
		if body.PullbackSeconds < 1 || body.PullbackSeconds > maxPullbackSeconds {
			jsonError(w, fmt.Sprintf("pullbackSeconds must be between 1 and %d", maxPullbackSeconds), http.StatusBadRequest)
			return
		}
		s.deps.Config.Governor.Sensing.PullbackSeconds = body.PullbackSeconds
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config", "error", err)
	}
	s.auditFromRequest(r, "config_governor_sensing", auditDetail("section", "sensing"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorThresholds(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body map[string]int
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if err := validateGovernorThresholds(body); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	for modeName, threshold := range body {
		if mode, ok := s.deps.Config.Governor.Modes[modeName]; ok {
			mode.Threshold = threshold
			s.deps.Config.Governor.Modes[modeName] = mode
		}
	}

	// #4037: the operator just typed these numbers, so they stop being
	// pack-seeded bases and become absolutes that EffectiveThreshold returns
	// verbatim — the #3498 guarantee that a hand-tuned `surge: 300` is never
	// multiplied by the repo count. Cleared for the WHOLE set, deliberately:
	// leaving the untouched modes scaling while this one does not can invert
	// the mode ladder (a scaled busy above an unscaled surge). Editing any
	// threshold means the operator owns all of them.
	s.deps.Config.Governor.ThresholdsSource = ""

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after threshold update", "error", err)
	}

	// Trigger immediate governor re-evaluation so mode change is visible
	// without waiting for the next eval ticker (up to 5 minutes).
	if s.deps.EnumerateFunc != nil {
		go s.deps.EnumerateFunc()
	}

	s.auditFromRequest(r, "config_governor_thresholds", auditDetail("section", "thresholds"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// handleGovernorThresholdScalingGet returns the configured threshold-scaling
// curve (with its default applied) so the Thresholds tab can prefill its
// select without loading the whole governor config payload. OWNER-ONLY,
// matching the write side and the rest of the governor-config surface.
func (s *Server) handleGovernorThresholdScalingGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, map[string]string{
		"threshold_scaling": s.deps.Config.Governor.ThresholdScalingMode(),
	})
}

// handleGovernorThresholdScaling sets how the DEFAULT mode thresholds scale
// with the hive's repo count (#3498).
//
// It is a separate route from handleGovernorThresholds because that one's body
// is a flat map[string]int of mode name to threshold; a string curve does not
// fit it, and widening it to map[string]any would put a parse branch on the
// path every threshold drag already takes.
func (s *Server) handleGovernorThresholdScaling(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		ThresholdScaling string `json:"thresholdScaling"`
		// Snake-case alias so callers can send the same key the GET returns
		// and the YAML config uses.
		ThresholdScalingSnake string `json:"threshold_scaling"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	raw := body.ThresholdScaling
	if raw == "" {
		raw = body.ThresholdScalingSnake
	}
	scaling := sanitizeString(raw)
	// Same gate as config.validate, so the write path cannot persist a value
	// that fails the next config load.
	if !config.ValidateThresholdScaling(scaling) {
		jsonError(w, "thresholdScaling must be one of: linear, sqrt, none (or empty for the default)", http.StatusBadRequest)
		return
	}
	s.deps.Config.Governor.ThresholdScaling = scaling

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after threshold scaling update", "error", err)
	}

	// Changing the curve moves every scaled threshold, so re-evaluate now
	// rather than leaving the hive on the old ladder until the next eval tick —
	// matching what a threshold drag already does.
	if s.deps.EnumerateFunc != nil {
		go s.deps.EnumerateFunc()
	}

	s.auditFromRequest(r, "config_governor_threshold_scaling", auditDetail("section", "thresholdScaling"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorLabels(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	// Both label polarities save through this one endpoint (the Labels tab
	// edits both). POINTER-typed so an absent key means "unchanged" — a save
	// that only touched the require list must not wipe the exempt list to
	// empty, and vice versa.
	var body struct {
		Labels        *[]string `json:"labels"`
		RequireLabels *[]string `json:"require_labels"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Labels != nil {
		if err := validateGovernorLabels(*body.Labels); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if body.RequireLabels != nil {
		if err := validateGovernorLabels(*body.RequireLabels); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if body.Labels != nil {
		filtered := make([]string, 0, len(*body.Labels))
		for _, l := range *body.Labels {
			isPermanent := false
			for _, h := range github.HoldLabels {
				if l == h {
					isPermanent = true
					break
				}
			}
			for _, p := range github.PermanentExemptLabels {
				if l == p {
					isPermanent = true
					break
				}
			}
			if !isPermanent {
				filtered = append(filtered, l)
			}
		}
		s.deps.Config.Governor.Labels.Exempt = filtered
		if s.deps.GHClient != nil {
			s.deps.GHClient.SetExemptLabels(filtered)
		}
	}
	if body.RequireLabels != nil {
		// The require gate (project.issue_filter): empty list = filter off.
		// Takes effect on the next enumeration via the scan client.
		s.deps.Config.Project.IssueFilter.RequireLabels = *body.RequireLabels
		if s.deps.GHClient != nil {
			s.deps.GHClient.SetIssueFilter(s.deps.Config.Project.IssueFilter)
		}
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after label update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_labels", auditDetail("section", "labels"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorBudget(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	// Pointer fields distinguish "field absent from the JSON" (nil) from
	// "field explicitly set to 0". The dashboard sends only the inputs the
	// user actually touched, so editing Total Tokens alone POSTs
	// {"totalTokens":N} with no periodDays/criticalPct. With plain ints
	// those absent fields decoded to 0 and were rejected by validation as
	// out-of-range. Nil now means "leave the stored value alone", while an
	// explicit 0 is still honored (totalTokens: 0 disables budget tracking).
	var body struct {
		TotalTokens *int64 `json:"totalTokens"`
		PeriodDays  *int   `json:"periodDays"`
		CriticalPct *int   `json:"criticalPct"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// Validate against the effective post-update values: supplied fields use
	// the incoming value, omitted fields keep what is already stored. This
	// keeps a partial update from being judged against a phantom zero.
	current := s.deps.Config.Governor.Budget
	totalTokens, periodDays, criticalPct := current.TotalTokens, current.PeriodDays, current.CriticalPct
	if body.TotalTokens != nil {
		totalTokens = *body.TotalTokens
	}
	if body.PeriodDays != nil {
		periodDays = *body.PeriodDays
	}
	if body.CriticalPct != nil {
		criticalPct = *body.CriticalPct
	}

	// The sanity floor judges only what THIS request supplied, so a spoke
	// already storing a below-floor limit can still edit its other budget
	// fields (#5508). Checked before the range validation so the operator is
	// told about the unit mistake first.
	if err := validateSuppliedBudgetFloor(body.TotalTokens); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := validateGovernorBudget(totalTokens, periodDays, criticalPct); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if body.TotalTokens != nil {
		s.deps.Config.Governor.Budget.TotalTokens = totalTokens
		s.deps.Governor.SetBudgetLimit(totalTokens)
	}
	if body.PeriodDays != nil {
		s.deps.Config.Governor.Budget.PeriodDays = periodDays
	}
	if body.CriticalPct != nil {
		s.deps.Config.Governor.Budget.CriticalPct = criticalPct
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after budget update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_budget", auditDetail("section", "budget"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// handleGovernorBudgetReset opens a fresh budget window on demand. The
// operator's recovery path when the window's spend has (or a mis-sized limit
// has) closed the kick gate: fix the limit via PUT /budget, then reset the
// window here so kicks resume immediately — budgeting stays ON, unlike the
// "ignore budget" bypass. Spend re-anchors at zero and the once-per-window
// alerts rearm.
func (s *Server) handleGovernorBudgetReset(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	s.deps.Governor.ResetBudgetWindow()
	s.auditFromRequest(r, "governor_budget_window_reset", auditDetail("section", "budget"), "")
	floor := s.refreshAndPersistSeq()
	// minStatusSeq: see handleResetRestarts — lets the dashboard discard
	// pre-reset status snapshots so the budget bar can't revert (#4348).
	jsonResponse(w, map[string]any{"ok": true, "status": "reset", "minStatusSeq": floor})
}

func (s *Server) handleGovernorNotifications(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		NtfyServer     string `json:"ntfyServer"`
		NtfyTopic      string `json:"ntfyTopic"`
		DiscordWebhook string `json:"discordWebhook"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := validateNotificationURL(body.NtfyServer, "ntfyServer"); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateNotificationURL(body.DiscordWebhook, "discordWebhook"); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	isMasked := func(v string) bool { return strings.HasPrefix(v, "•") }
	if (body.NtfyServer != "" && !isMasked(body.NtfyServer)) || (body.NtfyTopic != "" && !isMasked(body.NtfyTopic)) {
		if s.deps.Config.Notifications.Ntfy == nil {
			s.deps.Config.Notifications.Ntfy = &config.NtfyConfig{}
		}
		if body.NtfyServer != "" && !isMasked(body.NtfyServer) {
			s.deps.Config.Notifications.Ntfy.Server = body.NtfyServer
		}
		if body.NtfyTopic != "" && !isMasked(body.NtfyTopic) {
			s.deps.Config.Notifications.Ntfy.Topic = body.NtfyTopic
		}
	}
	if body.DiscordWebhook != "" && !isMasked(body.DiscordWebhook) {
		if s.deps.Config.Notifications.Discord == nil {
			s.deps.Config.Notifications.Discord = &config.DiscordConfig{}
		}
		s.deps.Config.Notifications.Discord.Webhook = body.DiscordWebhook
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after notification update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_notifications", auditDetail("section", "notifications"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorHealth(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		HealthcheckInterval int   `json:"healthcheckInterval"`
		RestartCooldown     int   `json:"restartCooldown"`
		ModelLock           *bool `json:"modelLock"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := validateGovernorHealth(body.HealthcheckInterval, body.RestartCooldown); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if body.HealthcheckInterval > 0 {
		s.deps.Config.Governor.Health.HealthcheckInterval = body.HealthcheckInterval
	}
	if body.RestartCooldown > 0 {
		s.deps.Config.Governor.Health.RestartCooldown = body.RestartCooldown
	}
	if body.ModelLock != nil {
		s.deps.Config.Governor.Health.ModelLock = *body.ModelLock
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after health update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_health", auditDetail("section", "health"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorLogging(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		MaxSizeMB  int    `json:"maxSizeMB"`
		MaxAgeDays int    `json:"maxAgeDays"`
		MaxBackups int    `json:"maxBackups"`
		Compress   *bool  `json:"compress"`
		Level      string `json:"level"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := validateGovernorLogging(s.deps.Config.Governor.Logging.Dir, body.MaxSizeMB, body.MaxAgeDays); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if body.MaxSizeMB > 0 {
		s.deps.Config.Governor.Logging.MaxSizeMB = body.MaxSizeMB
	}
	if body.MaxAgeDays > 0 {
		s.deps.Config.Governor.Logging.MaxAgeDays = body.MaxAgeDays
	}
	if body.MaxBackups > 0 {
		s.deps.Config.Governor.Logging.MaxBackups = body.MaxBackups
	}
	if body.Compress != nil {
		s.deps.Config.Governor.Logging.Compress = *body.Compress
	}
	if body.Level != "" {
		switch body.Level {
		case "debug", "info", "warn", "error":
			s.deps.Config.Governor.Logging.Level = body.Level
		default:
			jsonError(w, "level must be one of: debug, info, warn, error", http.StatusBadRequest)
			return
		}
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after logging update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_logging", auditDetail("section", "logging"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// handleGovernorAttribution updates the hive-wide attribution-trailer toggle.
// One boolean, applied to ALL agents (no per-agent granularity): it gates ONLY
// the visible "— hive: …" trailer appended to hive-created PRs and issues.
// The audit-log entry for every such creation is written unconditionally, so
// turning the trailer off never loses the invocation record.
func (s *Server) handleGovernorAttribution(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		AttributionTrailer *bool `json:"attributionTrailer"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.AttributionTrailer != nil {
		s.deps.Config.Governor.AttributionTrailer = body.AttributionTrailer
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after attribution update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_attribution", auditDetail("section", "attribution"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func normalizeContributeDelegatableRoles(roles []string) []string {
	seen := map[string]bool{}
	for _, role := range []string{"scanner", "quality", "outreach"} {
		seen[role] = true
	}
	for _, role := range roles {
		role = normalizeAgentRole(role)
		if role == "" || role == "supervisor" {
			continue
		}
		seen[role] = true
	}
	out := make([]string, 0, len(seen))
	for role := range seen {
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

func (s *Server) handleGovernorHub(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Enabled                        *bool                      `json:"enabled"`
		URL                            string                     `json:"url"`
		DashboardURL                   string                     `json:"dashboard_url"`
		SnapshotURL                    string                     `json:"snapshot_url"`
		IsPublic                       *bool                      `json:"is_public"`
		AutoSnapshot                   *bool                      `json:"auto_snapshot"`
		SnapshotFrameAncestors         []string                   `json:"snapshot_frame_ancestors"`
		AutoUpgrade                    *bool                      `json:"auto_upgrade"`
		ContributeSuspended            *bool                      `json:"contribute_suspended"`
		ContributeTitlesMode           *string                    `json:"contribute_titles_mode"`
		ContributeAuthorsMode          *string                    `json:"contribute_authors_mode"`
		ContributeLabelsMode           *string                    `json:"contribute_labels_mode"`
		ContributeAllowLabels          []string                   `json:"contribute_allow_labels"`
		ContributeDenyLabels           []string                   `json:"contribute_deny_labels"`
		ContributeDenyTitles           []string                   `json:"contribute_deny_titles"`
		ContributeDenyAuthors          []string                   `json:"contribute_deny_authors"`
		ContributeAllowModels          []string                   `json:"contribute_allow_models"`
		ContributeRejectUnknownModels  *bool                      `json:"contribute_reject_unknown_models"`
		ContributeSkipAssignedToOthers *bool                      `json:"contribute_skip_assigned_to_others"`
		ContributeCooldownEnabled      *bool                      `json:"contribute_cooldown_enabled"`
		ContributeCooldownHours        *int                       `json:"contribute_cooldown_hours"`
		ContributeDelegatableRoles     []string                   `json:"contribute_delegatable_roles"`
		DisabledRepos                  []string                   `json:"disabled_repos"`
		DisabledTiers                  []string                   `json:"disabled_tiers"`
		TierLimits                     map[string]config.TierRate `json:"tier_limits"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	if body.Enabled != nil {
		cfg.Hub.Enabled = *body.Enabled
	}
	if body.URL != "" {
		cfg.Hub.URL = body.URL
	}
	if body.DashboardURL != "" {
		cfg.Hub.DashboardURL = body.DashboardURL
	}
	cfg.Hub.SnapshotURL = body.SnapshotURL
	if body.IsPublic != nil {
		cfg.Hub.IsPublic = *body.IsPublic
	}
	if body.AutoSnapshot != nil {
		cfg.Hub.AutoSnapshot = *body.AutoSnapshot
	}
	if body.SnapshotFrameAncestors != nil {
		normalized, err := config.ValidateSnapshotFrameAncestors(body.SnapshotFrameAncestors)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		cfg.Dashboard.SnapshotFrameAncestors = normalized
	}
	if body.AutoUpgrade != nil {
		cfg.Hub.AutoUpgrade = *body.AutoUpgrade
	}
	if body.ContributeSuspended != nil {
		cfg.Hub.ContributeSuspended = *body.ContributeSuspended
	}
	if body.ContributeTitlesMode != nil {
		cfg.Hub.ContributeTitlesMode = *body.ContributeTitlesMode
	}
	if body.ContributeAuthorsMode != nil {
		cfg.Hub.ContributeAuthorsMode = *body.ContributeAuthorsMode
	}
	if body.ContributeLabelsMode != nil {
		cfg.Hub.ContributeLabelsMode = *body.ContributeLabelsMode
	}
	if body.ContributeAllowLabels != nil {
		cfg.Hub.ContributeAllowLabels = body.ContributeAllowLabels
	}
	if body.ContributeDenyLabels != nil {
		cfg.Hub.ContributeDenyLabels = body.ContributeDenyLabels
	}
	if body.ContributeDenyTitles != nil {
		cfg.Hub.ContributeDenyTitles = body.ContributeDenyTitles
	}
	if body.ContributeDenyAuthors != nil {
		cfg.Hub.ContributeDenyAuthors = body.ContributeDenyAuthors
	}
	if body.ContributeAllowModels != nil {
		cfg.Hub.ContributeAllowModels = body.ContributeAllowModels
	}
	if body.ContributeRejectUnknownModels != nil {
		cfg.Hub.ContributeRejectUnknownModels = *body.ContributeRejectUnknownModels
	}
	if body.ContributeSkipAssignedToOthers != nil {
		cfg.Hub.ContributeSkipAssignedToOthers = *body.ContributeSkipAssignedToOthers
	}
	// Cooldown toggle: store the client's on/off intent as a non-nil pointer so it
	// is round-tripped exactly (nil would default back to enabled). A client that
	// omits the field leaves the current value untouched.
	if body.ContributeCooldownEnabled != nil {
		v := *body.ContributeCooldownEnabled
		cfg.Hub.ContributeCooldownEnabled = &v
	}
	// Cooldown period (hours): stored as-is; the config resolver
	// (ContributeCooldownHoursOrDefault) clamps to the valid range at read time and
	// applyDefaults clamps any persisted value, so a stray input can never park an
	// issue forever. A client sending 0 means "use the default".
	if body.ContributeCooldownHours != nil {
		cfg.Hub.ContributeCooldownHours = *body.ContributeCooldownHours
	}
	if body.ContributeDelegatableRoles != nil {
		cfg.Hub.ContributeDelegatableRoles = normalizeContributeDelegatableRoles(body.ContributeDelegatableRoles)
	}
	if body.DisabledRepos != nil {
		cfg.Hub.DisabledRepos = body.DisabledRepos
	}
	if body.DisabledTiers != nil {
		cfg.Hub.DisabledTiers = body.DisabledTiers
	}
	if body.TierLimits != nil {
		cfg.Hub.TierLimits = body.TierLimits
	}
	// Normalize any client-supplied filter mode to a valid value (unknown/empty
	// -> deny), so a bad payload can't leave a mode that silently changes
	// enforcement in an unexpected way.
	cfg.Hub.ContributeTitlesMode = config.NormalizeFilterMode(cfg.Hub.ContributeTitlesMode)
	cfg.Hub.ContributeAuthorsMode = config.NormalizeFilterMode(cfg.Hub.ContributeAuthorsMode)
	cfg.Hub.ContributeLabelsMode = config.NormalizeFilterMode(cfg.Hub.ContributeLabelsMode)
	s.auditFromRequest(r, "config_governor_hub", auditDetail("section", "hub"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// apiKeyLikeLength: env var NAMES are short and conventionally use
// underscores, while bearer keys are typically 40+ chars without any.
const apiKeyLikeLength = 40

// looksLikeAPIKeyValue guards the env-name field against users pasting an
// actual API key VALUE into it (which would bake "sk-..." into hive.yaml
// as an env var name and silently resolve to an empty key at runtime).
func looksLikeAPIKeyValue(s string) bool {
	return strings.HasPrefix(s, "sk-") ||
		(len(s) > apiKeyLikeLength && !strings.Contains(s, "_"))
}

const (
	// litellmProbeTimeout bounds the save-time /v1/models check.
	litellmProbeTimeout = 8 * time.Second
	// litellmProbeMaxErrBody caps how much of a gateway error body is
	// surfaced back to the dialog.
	litellmProbeMaxErrBody = 512
)

// litellmKeyMaterialPatterns match key-identifying material that some gateways
// (e.g. LiteLLM) echo back in auth-failure bodies: the SHA-256 key hash, the
// masked "Received API Key = sk-...XXXX" hint, and bare sk- tokens. We keep the
// human-useful "401 auth failed" signal but strip these — the key hash is a
// stable fingerprint of the secret that should not appear in the dashboard or
// logs.
var litellmKeyMaterialPatterns = []*regexp.Regexp{
	// "Key Hash (Token) = <64 hex>" (also matches "Key Hash = <hex>").
	regexp.MustCompile(`(?i)(key hash[^=]*=\s*)[0-9a-f]{16,}`),
	// "Received API Key = sk-...GTNw" (masked hint) or any explicit key= field.
	regexp.MustCompile(`(?i)(received api key\s*=\s*)\S+`),
	// Any bare sk- token that slipped through.
	regexp.MustCompile(`sk-[A-Za-z0-9._\-]{6,}`),
	// A long standalone hex fingerprint (32+ hex chars).
	regexp.MustCompile(`\b[0-9a-f]{32,}\b`),
}

// redactLiteLLMKeyMaterial removes API-key hints and key hashes from a gateway
// error body before it is surfaced to the dashboard or logged.
func redactLiteLLMKeyMaterial(s string) string {
	for i, re := range litellmKeyMaterialPatterns {
		if i < 2 {
			// Keep the field label, redact the value.
			s = re.ReplaceAllString(s, "${1}[redacted]")
		} else {
			s = re.ReplaceAllString(s, "[redacted]")
		}
	}
	return s
}

// probeLiteLLMModels queries {endpoint}/v1/models with the given key and
// returns the number of models the gateway offers. This is a LIVE probe
// only — it never falls back to the static model aliases, so a failing
// gateway can never masquerade as "N models available" (the bug where
// Test Connection reported the 7 static fallback aliases as success).
// On failure the error carries the gateway's actual message (truncated)
// so the UI can show e.g. LiteLLM's "token not found" instead of a bare
// status code.
func probeLiteLLMModels(endpoint, apiKey string) (int, error) {
	return probeModelsWithHeaders(endpoint, apiKey, nil)
}

// probeModelsWithHeaders is probeLiteLLMModels with an optional set of extra
// request headers (e.g. watsonx's X-IBM-Project-ID). apiKey, when non-empty, is
// still sent as the Bearer — for watsonx the caller passes the minted IAM token
// as apiKey, not the raw key.
func probeModelsWithHeaders(endpoint, apiKey string, extraHeaders map[string]string) (int, error) {
	modelsURL := strings.TrimRight(endpoint, "/") + "/v1/models"
	req, err := http.NewRequest("GET", modelsURL, nil)
	if err != nil {
		return 0, fmt.Errorf("building request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range extraHeaders {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	// Re-validate every redirect hop: a gateway endpoint is operator-supplied
	// but the redirect target is chosen by the remote server at request time.
	client := &http.Client{Timeout: litellmProbeTimeout, CheckRedirect: noRedirectToPrivate}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cannot reach gateway: %w", err)
	}
	defer closeHTTPBody(resp.Body)

	if resp.StatusCode != http.StatusOK {
		// Error path: only a truncated slice of the body is surfaced to the
		// dialog (error bodies can be huge and may echo the key).
		body, _ := io.ReadAll(io.LimitReader(resp.Body, litellmProbeMaxErrBody))
		gatewayMsg := redactLiteLLMKeyMaterial(strings.TrimSpace(string(body)))
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			// The two auth failures lead users to different fixes.
			if apiKey == "" {
				return 0, fmt.Errorf("gateway requires an API key and none is configured (HTTP %d): %s",
					resp.StatusCode, gatewayMsg)
			}
			return 0, fmt.Errorf("gateway rejected the configured key (HTTP %d): %s",
				resp.StatusCode, gatewayMsg)
		default:
			return 0, fmt.Errorf("gateway returned HTTP %d: %s", resp.StatusCode, gatewayMsg)
		}
	}

	// Success path: parse the FULL body with the exact same lenient decoder
	// the model-discovery dropdown uses (fetchModelsFromEndpoint) — a JSON
	// object with a "data" array of items each carrying an "id" string is
	// sufficient. We do NOT require top-level object=="list" (some gateways
	// omit or reorder it) and tolerate extra/unknown fields. Reading only a
	// truncated prefix here (the old bug) corrupted large valid lists into
	// invalid JSON and produced a false "non-OpenAI response" negative.
	models, err := parseModelsResponse(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("gateway returned a non-OpenAI /v1/models response")
	}
	return len(models), nil
}

// redactSecret removes any occurrence of a secret value from a message so
// gateway error bodies can never echo the key back to the client or logs.
func redactSecret(msg, secret string) string {
	if secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, "[redacted]")
}

// maskHintVisibleChars is how many trailing characters of a secret survive
// masking in UI hints ("••••WMg" style) — enough to recognize a key,
// useless to reconstruct one.
const maskHintVisibleChars = 4

// maskSecretHint returns a display-safe hint for a secret value: bullets
// plus the last few characters. Values too short to safely reveal a tail
// are fully masked.
func maskSecretHint(v string) string {
	if len(v) <= maskHintVisibleChars*2 {
		return "••••"
	}
	return "••••" + v[len(v)-maskHintVisibleChars:]
}

// litellmSectionResponse builds the governor-config GET payload for the
// LiteLLM tab. The resolved key VALUE is never serialized — only hasKey,
// the store it came from, and a masked tail hint. The apiKeyEnv/apiKeyFile
// CONFIG fields normally hold a var NAME / file PATH (not secrets), but a
// user who pasted an actual key into them before the guardrails existed
// would otherwise get it echoed straight back into the tab — in one live
// case the raw key rendered in plaintext during screen shares. Key-like
// values in those fields are therefore masked too, with LooksLikeKey
// flags so the UI can tell the user to move the value.
func litellmSectionResponse(lc *config.LiteLLMConfig) map[string]interface{} {
	apiKeyEnv := lc.APIKeyEnv
	apiKeyEnvLooksLikeKey := looksLikeAPIKeyValue(apiKeyEnv)
	if apiKeyEnvLooksLikeKey {
		apiKeyEnv = maskSecretHint(apiKeyEnv)
	}
	apiKeyFile := lc.APIKeyFile
	apiKeyFileLooksLikeKey := !strings.HasPrefix(apiKeyFile, "/") && looksLikeAPIKeyValue(apiKeyFile)
	if apiKeyFileLooksLikeKey {
		apiKeyFile = maskSecretHint(apiKeyFile)
	}
	key := lc.ResolveAPIKey()
	keyHint := ""
	if key != "" {
		keyHint = maskSecretHint(key)
	}
	return map[string]interface{}{
		"endpoint":               lc.Endpoint,
		"apiKeyEnv":              apiKeyEnv,
		"apiKeyEnvLooksLikeKey":  apiKeyEnvLooksLikeKey,
		"apiKeyFile":             apiKeyFile,
		"apiKeyFileLooksLikeKey": apiKeyFileLooksLikeKey,
		"defaultModel":           lc.DefaultModel,
		"caBundle":               lc.CABundle,
		"localProxy":             lc.LocalProxy,
		"hasKey":                 key != "",
		"keyHint":                keyHint,
		// redactSecret guards the pathological case of a key-like env var
		// NAME appearing in the source string.
		"keySource": redactSecret(lc.ResolveAPIKeySource(), key),
	}
}

// classifierSectionResponse surfaces the effective tier-classification keyword
// lists (Phase 4 Part C) to the dashboard governor-config view. It returns the
// keywords actually in force — config-driven when a `classifier:` block is set,
// else the built-in defaults — so operators can see and (via hive.yaml) edit
// which title keywords map issues to the Simple/Complex tiers. classify.SetLanes
// already surfaces per-agent lane_keywords per-agent; these are the analogous
// global tier lists.
func classifierSectionResponse() map[string]interface{} {
	simple, complex := classify.TierKeywords()
	return map[string]interface{}{
		"simpleKeywords": simple,
		"complexSignals": complex,
	}
}

// liteLLMProbeResult runs the live probe against the effective endpoint
// using the SAME key resolution the inference translator uses
// (ResolveAPIKey: api_key_file → Secret mount → PVC file → env), unless
// overrideKey is set (a key just submitted in the same request, which the
// key files may not reflect yet). Returns nil when no endpoint is
// configured.
func (s *Server) liteLLMProbeResult(lc *config.LiteLLMConfig, overrideKey string) map[string]interface{} {
	ep := lc.ResolveEndpoint()
	if ep == "" {
		return nil
	}
	probeKey := overrideKey
	if probeKey == "" {
		probeKey = lc.ResolveAPIKey()
	}
	n, err := probeLiteLLMModels(ep, probeKey)
	if err != nil {
		return map[string]interface{}{"ok": false, "error": redactSecret(err.Error(), probeKey)}
	}
	result := map[string]interface{}{"ok": true, "models": n}
	// If the proxy has learned this key's entitled subset (some gateways
	// advertise the full catalog on /v1/models but scope the key to fewer),
	// surface how many of the ADVERTISED models are actually usable so Test
	// Connection can say "N models available (M usable with your key)". Use
	// the intersection against the advertised list — not the raw entitled
	// count, which may list ids this gateway doesn't advertise.
	if fn := getEntitledModelsFn(); fn != nil {
		if entitled, _, ok := fn(ep); ok && len(entitled) > 0 {
			if advertised, ferr := fetchModelsFromEndpoint(ep, probeKey); ferr == nil {
				if usable := len(intersectEntitled(advertised, entitled)); usable < n {
					result["usable"] = usable
				}
			}
		}
	}
	return result
}

// handleGovernorLiteLLMTest is the Test Connection endpoint: a live
// /v1/models probe with the currently effective endpoint + key. Unlike
// /api/inference/models/{backend} it NEVER substitutes the static
// fallback aliases, so the result reflects only what the gateway said.
func (s *Server) handleGovernorLiteLLMTest(w http.ResponseWriter, r *http.Request) {
	lc := s.deps.Config.Governor.LiteLLM
	probe := s.liteLLMProbeResult(&lc, "")
	if probe == nil {
		jsonError(w, "no litellm endpoint configured", http.StatusBadRequest)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "probe": probe})
}

// handleGovernorLiteLLM updates governor.litellm from the dashboard's
// LiteLLM config tab / first-use dialog. An API key VALUE (apiKey) is
// stored via storeLiteLLMAPIKey (PVC file + best-effort hive-secrets
// Secret) — never in hive.yaml, logs, or responses; hive.yaml records
// only the file path. After saving, the gateway is probed at /v1/models
// and the result returned so a bad endpoint/key fails visibly at save
// time instead of as agent 401s later.
func (s *Server) handleGovernorLiteLLM(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Endpoint     *string `json:"endpoint"`
		APIKey       *string `json:"apiKey"`
		APIKeyEnv    *string `json:"apiKeyEnv"`
		APIKeyFile   *string `json:"apiKeyFile"`
		DefaultModel *string `json:"defaultModel"`
		CABundle     *string `json:"caBundle"`
		LocalProxy   *bool   `json:"localProxy"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	// Apply to a copy first so validation failure leaves config untouched.
	lc := cfg.Governor.LiteLLM
	if body.Endpoint != nil {
		lc.Endpoint = strings.TrimSpace(*body.Endpoint)
	}
	if body.APIKeyEnv != nil {
		keyEnv := strings.TrimSpace(*body.APIKeyEnv)
		if looksLikeAPIKeyValue(keyEnv) {
			jsonError(w, "this looks like an API key — paste it in the API Key field instead; "+
				"this field takes an environment variable NAME (e.g. HIVE_LITELLM_API_KEY)",
				http.StatusBadRequest)
			return
		}
		lc.APIKeyEnv = keyEnv
	}
	if body.APIKeyFile != nil {
		keyFile := strings.TrimSpace(*body.APIKeyFile)
		// Same paste-the-key-value guardrail as the env-name field; real
		// key file paths are absolute, keys never are.
		if !strings.HasPrefix(keyFile, "/") && looksLikeAPIKeyValue(keyFile) {
			jsonError(w, "this looks like an API key — paste it in the API Key field instead; "+
				"this field takes a file PATH (e.g. /secrets/litellm_api_key)",
				http.StatusBadRequest)
			return
		}
		// SECURITY (audit N8, CWE-200/918): the same confinement the gateway
		// upsert applies. This is the LiteLLM twin of that handler and carries
		// the identical defect — the guardrail above short-circuits for ANY
		// absolute path, so an arbitrary file could be stored and later read.
		if keyFile != "" && !config.SecretFilePathAllowed(keyFile) {
			jsonError(w, "api_key_file must be under /secrets or "+config.WritableSecretsDir,
				http.StatusBadRequest)
			return
		}
		lc.APIKeyFile = keyFile
	}
	if body.DefaultModel != nil {
		lc.DefaultModel = strings.TrimSpace(*body.DefaultModel)
	}
	if body.CABundle != nil {
		lc.CABundle = strings.TrimSpace(*body.CABundle)
	}
	if body.LocalProxy != nil {
		lc.LocalProxy = *body.LocalProxy
	}
	if err := lc.Validate(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Store a submitted key VALUE outside hive.yaml and point api_key_file
	// at it. Empty apiKey means "no change" so the tab can be re-saved
	// without re-entering the key.
	submittedKey := ""
	if body.APIKey != nil {
		submittedKey = strings.TrimSpace(*body.APIKey)
	}
	if submittedKey != "" {
		keyFile, err := s.storeLiteLLMAPIKey(submittedKey)
		if err != nil {
			jsonError(w, "failed to store API key: "+redactSecret(err.Error(), submittedKey),
				http.StatusInternalServerError)
			return
		}
		lc.APIKeyFile = keyFile
		// Remediation for the pre-guardrail leak: a key VALUE pasted into
		// the env-name field never worked as a name (os.Getenv("sk-...")
		// is empty) and is a plaintext secret in hive.yaml. Now that a
		// real key is stored properly, scrub it.
		if looksLikeAPIKeyValue(lc.APIKeyEnv) {
			lc.APIKeyEnv = ""
			s.logger.Info("cleared key-like value from litellm api_key_env (replaced by stored API key)")
		}
	}
	cfg.Governor.LiteLLM = lc

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after litellm update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_litellm", auditDetail("section", "litellm"), "")

	// Register the endpoint for model discovery (empty list unregisters)
	// and re-apply routes for live agents already running on litellm.
	// This must come after the key store above: SetInferenceRoute
	// snapshots the key into each route, so the refresh is what makes a
	// rotated key take effect without an agent restart.
	var endpoints []string
	if ep := lc.ResolveEndpoint(); ep != "" {
		endpoints = []string{ep}
	}
	s.UpdateInferenceEndpoint("litellm", endpoints)
	if s.deps.AgentMgr != nil {
		s.deps.AgentMgr.RefreshInferenceRoutes("litellm")
	}

	s.refreshAndPersist()

	// Save-time validation probe (live /v1/models — never the static
	// fallback list) so the dialog can immediately show "N models
	// available" or the gateway's real error.
	resp := map[string]interface{}{"ok": true, "status": "updated"}
	if probe := s.liteLLMProbeResult(&lc, submittedKey); probe != nil {
		resp["probe"] = probe
	}
	jsonResponse(w, resp)
}

// handleGovernorBobStatus reports whether a bob API key is configured, and
// WHERE it came from — never the value. The source string
// ("file:/data/secrets/bob_api_key", "env:HIVE_BOB_API_KEY") is the same
// safe-to-log form ResolveAPIKeySource returns and is what the dashboard
// shows so an operator can tell an admin-managed Secret key from one they
// entered here.
func (s *Server) handleGovernorBobStatus(w http.ResponseWriter, r *http.Request) {
	bc := s.deps.Config.Governor.Bob
	source := bc.ResolveAPIKeySource()
	jsonResponse(w, map[string]interface{}{
		"ok": true,
		// configured is presence-only; the key value is never serialized.
		"configured": source != "",
		"source":     source,
		// keyName is the operator-chosen LABEL for the key, not the key value —
		// safe to serialize. Empty on hives that never recorded a name (the
		// dashboard renders that as "(unnamed)"), so no backwards-compat break.
		"keyName": bc.KeyName,
	})
}

// handleGovernorBobKey stores a bob API key VALUE submitted from the spoke
// dashboard's governor Bob tab. The key is hive-wide, not per-agent: this
// endpoint backs a single hive-scoped setting shared by every bob agent.
// The value is written to a 0600 PVC file
// (and best-effort into the hive-secrets Secret); hive.yaml records only the
// resulting file PATH. The value never appears in the response, in hive.yaml,
// or in any log line.
//
// Authorization is the standard config-endpoint gating: this is a PUT, so the
// roleEnforcement middleware rejects a read-only role with 403 before the
// handler runs.
func (s *Server) handleGovernorBobKey(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		APIKey *string `json:"apiKey"`
		// KeyName is an optional human LABEL recorded alongside the key so
		// managers can tell keys apart without seeing values. Absent
		// (nil) leaves any existing name untouched; present-but-empty clears it.
		KeyName *string `json:"keyName"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.APIKey == nil {
		jsonError(w, "apiKey is required", http.StatusBadRequest)
		return
	}
	var keyName string
	nameProvided := body.KeyName != nil
	if nameProvided {
		keyName = strings.TrimSpace(*body.KeyName)
		if len(keyName) > bobKeyNameMaxLen {
			jsonError(w, fmt.Sprintf("keyName is too long (limit %d characters)", bobKeyNameMaxLen),
				http.StatusBadRequest)
			return
		}
	}
	key := strings.TrimSpace(*body.APIKey)
	if key == "" {
		// Distinct from DELETE: an empty PUT is a mistake (an empty field
		// submitted), not an intentional revoke, so say so rather than
		// silently wiping a working key.
		jsonError(w, "apiKey is empty — paste the bob API key, or use Clear to remove the existing one",
			http.StatusBadRequest)
		return
	}
	if len(key) > bobKeyMaxLen {
		jsonError(w, fmt.Sprintf("apiKey is too long (limit %d characters)", bobKeyMaxLen),
			http.StatusBadRequest)
		return
	}
	// A key with interior whitespace/newlines is always a broken paste, and
	// storing one is a proven auth failure ("invalid jwt string" 401 → every
	// bob agent parks at the auth prompt). Refuse at save time with the real
	// explanation. Leading/trailing whitespace was already trimmed above.
	if strings.ContainsAny(key, " \t\r\n") {
		jsonError(w, "apiKey contains interior whitespace or line breaks — a bob API key is a single unbroken token; re-copy it from bob.ibm.com and paste it again",
			http.StatusBadRequest)
		return
	}

	cfg := s.deps.Config
	// Apply to a copy so a store failure leaves config untouched.
	bc := cfg.Governor.Bob
	keyFile, err := s.storeBobAPIKey(key)
	if err != nil {
		// redactSecret guards against the key surfacing via a filesystem
		// error that happened to embed it.
		jsonError(w, "failed to store API key: "+redactSecret(err.Error(), key),
			http.StatusInternalServerError)
		return
	}
	bc.APIKeyFile = keyFile
	// A key VALUE pasted into api_key_env would be a plaintext secret in
	// hive.yaml and never worked as a name (os.Getenv("...") is empty).
	// Now that a real key is stored properly, scrub it.
	// Record the label only when the field was present: absent leaves the
	// existing name, present-but-empty clears it (both per the body contract).
	if nameProvided {
		bc.KeyName = keyName
	}
	if looksLikeAPIKeyValue(bc.APIKeyEnv) {
		bc.APIKeyEnv = ""
		s.logger.Info("cleared key-like value from bob api_key_env (replaced by stored API key)")
	}
	cfg.Governor.Bob = bc

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after bob api key update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_bob_key", auditDetail("section", "bob", "action", "set"), "")
	s.refreshAndPersist()

	// No agent restart is required to ADOPT the key: main.go injected a
	// resolver closure over the live config into the agent manager
	// (SetBobAPIKeyResolver), and it re-reads the key file on every agent
	// launch. Agents already parked in "failed: no API key" cannot pick it up
	// on their own, though: the key is Secret, so it is delivered only by tmux
	// set-environment, which is inherited by shells created AFTER it runs — and
	// their pane shell predates the key. RelaunchBobAgentsAwaitingKey therefore
	// recreates those sessions so a fresh shell inherits the key. Running,
	// paused, and non-bob agents are untouched, and a second save finds nothing
	// left to do.
	// s.deps.Ctx, NOT r.Context(): the launch path derives the agent's
	// long-lived pane-polling goroutine from this context, so a request-scoped
	// one would cancel it the moment this response is written. Every other
	// Start/Restart/Resume caller in the dashboard passes s.deps.Ctx too.
	var relaunched []string
	if s.deps != nil && s.deps.AgentMgr != nil {
		relaunched = s.deps.AgentMgr.RelaunchBobAgentsAwaitingKey(s.deps.Ctx)
	}
	if len(relaunched) > 0 {
		s.logger.Info("relaunched bob agents after api key save",
			"count", len(relaunched), "agents", strings.Join(relaunched, ","))
	}

	jsonResponse(w, map[string]interface{}{
		"ok":         true,
		"configured": true,
		// Path only, never the value.
		"source": "file:" + keyFile,
		// The pod does not need restarting; the resolver reads the key live.
		"restartNeeded": false,
		// How many parked bob agents were started by this save, so the UI can
		// report what actually happened instead of telling the user to do it.
		"relaunched": len(relaunched),
		// The label, never the value (see handleGovernorBobStatus).
		"keyName": bc.KeyName,
	})
}

// handleGovernorBobKeyClear revokes a stored bob API key: it removes the PVC
// key file and drops the api_key_file pointer from hive.yaml, returning the
// hive to the documented "key required" state rather than a half-configured
// one.
//
// It deliberately does NOT try to delete the key from the hive-secrets Secret
// or from an admin-mounted /secrets/bob_api_key: those are managed outside the
// dashboard, and ResolveAPIKey consults them ahead of the PVC file. The
// response reports the source that remains so the UI can tell the user the key
// is still supplied by an admin-managed store instead of falsely claiming it
// was cleared.
func (s *Server) handleGovernorBobKeyClear(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	removed, err := clearBobKeyFile()
	if err != nil {
		jsonError(w, "failed to clear API key: "+err.Error(), http.StatusInternalServerError)
		return
	}

	cfg := s.deps.Config
	bc := cfg.Governor.Bob
	// Only drop the pointer if it names the file we just removed; an operator
	// who pointed api_key_file at their own path keeps that setting. The label
	// describes the key we just cleared, so drop it in the same case — leaving
	// a stale "Team inference key" beside a now-empty slot would mislead.
	if bc.APIKeyFile == writableBobKeyFile {
		bc.APIKeyFile = ""
		bc.KeyName = ""
	}
	cfg.Governor.Bob = bc

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after bob api key clear", "error", err)
	}
	s.logger.Info("bob api key cleared", "api_key_file", writableBobKeyFile, "file_removed", removed)
	s.auditFromRequest(r, "config_governor_bob_key", auditDetail("section", "bob", "action", "clear"), "")
	s.refreshAndPersist()

	// Re-resolve: an admin-managed Secret/env key may still supply one.
	remaining := cfg.Governor.Bob.ResolveAPIKeySource()
	jsonResponse(w, map[string]interface{}{
		"ok":         true,
		"configured": remaining != "",
		"source":     remaining,
	})
}

func (s *Server) handleGovernorAddAgent(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Name    string `json:"name"`
		Backend string `json:"backend"`
		Model   string `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil || body.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}

	body.Name = sanitizeString(body.Name)
	body.Backend = sanitizeString(body.Backend)
	body.Model = sanitizeString(body.Model)

	if body.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	if strings.ContainsAny(body.Name, " ./\\") || !kickTemplatePattern.MatchString(body.Name+".md") {
		jsonError(w, "name must contain only alphanumeric characters, hyphens, and underscores", http.StatusBadRequest)
		return
	}
	const maxAgentNameLen = 64
	if len(body.Name) > maxAgentNameLen {
		jsonError(w, fmt.Sprintf("name must be at most %d characters", maxAgentNameLen), http.StatusBadRequest)
		return
	}

	if _, exists := s.deps.Config.Agents[body.Name]; exists {
		jsonError(w, "agent already exists", http.StatusConflict)
		return
	}

	if body.Backend == "" {
		body.Backend = "claude"
	}
	// Reject an unsupported backend before the agent is created, rather than
	// creating an agent that can never launch.
	if err := s.deps.Config.Governor.ValidateBackend(body.Backend); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	agentCfg := config.AgentConfig{
		Backend: body.Backend,
		Model:   body.Model,
		Enabled: true,
	}
	// An explicit re-add is the operator changing their mind, and it is the
	// only signal that lifts a deletion tombstone. Without this the agent
	// would be added now and pruned again by the next config reload.
	if s.deps.Config.ClearAgentRemoved(body.Name) {
		s.logger.Info("agent deletion tombstone lifted by explicit re-add", "agent", body.Name)
	}
	s.deps.Config.Agents[body.Name] = agentCfg
	s.deps.AgentMgr.AddAgent(body.Name, agentCfg)
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config", "error", err)
	}

	s.auditFromRequest(r, "add_agent", auditDetail("backend", body.Backend, "model", body.Model), body.Name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "added", "agent": body.Name})
}

func (s *Server) handleGovernorRemoveAgent(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	delete(s.deps.Config.Agents, name)
	s.deps.AgentMgr.RemoveAgent(name)

	// Tombstone the name BEFORE saving. Deleting from the in-memory map and
	// re-saving the overlay was all this handler used to do, and it did not
	// stick for two independent reasons:
	//
	//  1. /data/agent-configs/<name>.yaml was left on disk, and Load()'s
	//     MergeAgentOverrides UNIONS that directory over the config — so the
	//     next config reload (fsnotify; measured at ~36s after the delete on a
	//     live hive) re-materialized the agent. That is the reported
	//     "readded themselves a minute or so later".
	//  2. Even with the file gone, ApplyPack — which runs on every restart —
	//     re-created any pack agent missing from the roster.
	//
	// The tombstone closes both: MergeAgentOverrides skips and prunes it, and
	// ApplyPack refuses to re-create it.
	tombstoned := s.deps.Config.MarkAgentRemoved(name)
	overlayExisted, overlayDeleted := s.removeAgentOverlayFile(name)

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after agent removal", "error", err)
	}
	s.auditFromRequest(r, "remove_agent", "", name)
	s.refreshAndPersist()
	s.logger.Info("agent removed and tombstoned", "agent", name,
		"note", "will not be re-created by an ACMM pack apply until explicitly re-added")
	// One greppable audit line for the whole removal action: whether the
	// tombstone was newly written, whether a stale per-agent overlay file was
	// present and whether it was deleted, and the resulting tombstone set so a
	// grep by agent name tells the story of a non-sticking removal report
	// (#2439) without exec'ing into the pod to diff config files.
	s.logger.Info("audit: agent removed via dashboard",
		"hive_id", s.deps.Config.HiveID,
		"agent", name,
		"tombstoned", tombstoned,
		"overlay_file_existed", overlayExisted,
		"overlay_file_deleted", overlayDeleted,
		"removed_agents", s.deps.Config.RemovedAgents,
	)
	jsonResponse(w, agentDeletionResponse("removed", name, packLevelsDefining(name)))
}

// removeAgentOverlayFile deletes /data/agent-configs/<name>.yaml. A leftover
// overlay file is re-merged by every config load, which is one of the two ways
// a deleted agent used to come back.
//
// Returns (existed, deleted) purely for the caller's audit log: existed reports
// whether a per-agent overlay file was present before the delete (the stale-file
// resurrection vector), and deleted reports whether the removal succeeded. The
// deletion behavior itself is unchanged — an error is still non-fatal because the
// tombstone already prevents the merge from resurrecting the agent.
func (s *Server) removeAgentOverlayFile(name string) (existed, deleted bool) {
	agentsDir := s.deps.Config.Data.AgentsDir
	if agentsDir == "" {
		return false, false
	}
	// Observe presence before deleting so the audit log can distinguish
	// "no stale file" from "stale file removed". Stat errors other than
	// not-exist leave existed false — we only claim a file was present when
	// we positively saw one.
	overlayPath := filepath.Join(agentsDir, name+".yaml")
	if _, statErr := os.Stat(overlayPath); statErr == nil {
		existed = true
	}
	if err := config.RemoveAgentFile(agentsDir, name); err != nil {
		// Not fatal: the tombstone already prevents the merge from
		// resurrecting the agent. Log it at WARN so a stale file — the
		// resurrection vector for #2439 — is loud and greppable with its path.
		s.logger.Warn("failed to remove agent overlay file after delete (tombstone still applies)",
			"hive_id", s.deps.Config.HiveID, "agent", name, "path", overlayPath, "error", err)
		return existed, false
	}
	return existed, existed
}

// packLevelsDefining returns the ACMM levels whose pack defines this agent, so
// the delete response can tell the operator up front that the level's pack
// would otherwise have re-added it. Legibility is the point: silently undoing
// the operator's action a minute later is what turned this into a bug report
// instead of a question.
func packLevelsDefining(name string) []int {
	var levels []int
	for _, pack := range config.ACMMPacks() {
		for _, pa := range pack.Agents {
			if pa.Name == name {
				levels = append(levels, pack.Level)
				break
			}
		}
	}
	return levels
}

// agentDeletionResponse builds the delete response, including an explicit
// human-readable note when the agent is part of an ACMM pack.
func agentDeletionResponse(status, name string, packLevels []int) map[string]any {
	resp := map[string]any{"ok": true, "status": status, "agent": name, "tombstoned": true}
	if len(packLevels) == 0 {
		resp["note"] = "This agent is not part of any ACMM pack; it will stay deleted."
		return resp
	}
	parts := make([]string, 0, len(packLevels))
	for _, level := range packLevels {
		parts = append(parts, strconv.Itoa(level))
	}
	resp["packLevels"] = packLevels
	resp["note"] = fmt.Sprintf(
		"%s is defined by the ACMM pack for level %s. It has been recorded as deliberately deleted, so no restart or level change will bring it back. Re-add it from the Governor grid if you change your mind.",
		name, strings.Join(parts, ", "))
	return resp
}

func (s *Server) handleGovernorRepos(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Repos       []string `json:"repos"`
		PrimaryRepo *string  `json:"primaryRepo,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if len(body.Repos) == 0 && body.PrimaryRepo == nil {
		jsonError(w, "at least one repo is required", http.StatusBadRequest)
		return
	}
	org := s.deps.Config.Project.Org

	// Single-host-per-spoke (defence in depth; the Repos tab also checks this
	// client-side so it fails fast). Every repo the user submits must live on
	// this hive's forge — mixing github.com and a GHE instance silently breaks
	// App auth for half the repos. Check the raw pasted values BEFORE the loop
	// below strips the host off each one. hiveForgeHost() is this hive's own
	// GitHub host (github.com or the GHE hostname), the same value the dashboard
	// derives from github_base_url.
	// Snapshot so a validation failure AFTER we mutate the in-memory config can
	// restore it — the "always exactly one default" guard below runs once the
	// final repos+primary are known, and a reject must not leave a half-applied
	// config in memory (which would then be persisted on the next unrelated save).
	prevOrg := s.deps.Config.Project.Org
	prevRepos := append([]string(nil), s.deps.Config.Project.Repos...)
	prevPrimary := s.deps.Config.Project.PrimaryRepo
	prevBaseURL := s.deps.Config.GitHub.BaseURL
	prevAPIURL := s.deps.Config.GitHub.APIURL

	spokeHost := s.hiveForgeHost()
	validateRepos := prevRepos
	if len(body.Repos) > 0 {
		validateRepos = body.Repos
	}
	validatePrimary := prevPrimary
	if body.PrimaryRepo != nil {
		validatePrimary = *body.PrimaryRepo
	}
	adoptOrg := org
	if nextOrg, errMsg := governorReposAdoptOrg(org, validateRepos, validatePrimary, spokeHost); errMsg != "" {
		jsonError(w, errMsg, http.StatusBadRequest)
		return
	} else if nextOrg != "" {
		adoptOrg = nextOrg
	}
	validateRepos = normalizeGovernorRepoRefs(adoptOrg, validateRepos)
	validatePrimary = normalizeGovernorRepoRef(adoptOrg, validatePrimary)
	if issue := config.ValidateProjectRepoTargets(adoptOrg, validateRepos, validatePrimary, spokeHost); issue != nil {
		jsonError(w, issue.Message, http.StatusBadRequest)
		return
	}
	if adoptOrg != org {
		s.logger.Info("project org changed from repo paste", "from", org, "to", adoptOrg)
		org = adoptOrg
		s.deps.Config.Project.Org = adoptOrg
		if s.deps.GHClient != nil {
			s.deps.GHClient.SetOrg(adoptOrg)
		}
	}
	// Feed the normalized values back into the body so the persistence code
	// below stores bare names even when its own url-parse branch does not fire.
	if len(body.Repos) > 0 {
		body.Repos = validateRepos
	}
	if body.PrimaryRepo != nil {
		body.PrimaryRepo = &validatePrimary
	}

	if len(body.Repos) > 0 {
		stripped := make([]string, 0, len(body.Repos))
		for _, repo := range body.Repos {
			repo = sanitizeString(repo)
			if repo == "" || strings.Contains(repo, "..") || strings.ContainsAny(repo, "<>\"';&|") {
				jsonError(w, fmt.Sprintf("invalid repo name: %s", repo), http.StatusBadRequest)
				return
			}
			if parsed, err := url.Parse(repo); err == nil && parsed.Scheme != "" && parsed.Host != "" {
				parts := strings.SplitN(strings.TrimPrefix(parsed.Path, "/"), "/", 3)
				if len(parts) >= 2 {
					if parts[0] != "" {
						org = parts[0]
						s.deps.Config.Project.Org = org
					}
					repo = parts[1]
					if parsed.Host != "github.com" {
						s.deps.Config.GitHub.BaseURL = parsed.Scheme + "://" + parsed.Host
						s.deps.Config.GitHub.APIURL = parsed.Scheme + "://" + parsed.Host + "/api/v3"
					}
				}
			}
			if org != "" && strings.HasPrefix(repo, org+"/") {
				stripped = append(stripped, strings.TrimPrefix(repo, org+"/"))
			} else {
				stripped = append(stripped, repo)
			}
		}
		s.deps.Config.Project.Repos = stripped
		if s.deps.GHClient != nil {
			s.deps.GHClient.SetRepos(stripped)
		}
	}

	if body.PrimaryRepo != nil {
		newPrimary := sanitizeString(*body.PrimaryRepo)
		if parsed, err := url.Parse(newPrimary); err == nil && parsed.Scheme != "" && parsed.Host != "" {
			parts := strings.SplitN(strings.TrimPrefix(parsed.Path, "/"), "/", 3)
			if len(parts) >= 2 {
				newPrimary = parts[1]
			}
		}
		if org != "" && strings.HasPrefix(newPrimary, org+"/") {
			newPrimary = strings.TrimPrefix(newPrimary, org+"/")
		}
		oldPrimary := s.deps.Config.Project.PrimaryRepo
		s.deps.Config.Project.PrimaryRepo = newPrimary
		if newPrimary != oldPrimary {
			s.logger.Info("primary repo changed", "from", oldPrimary, "to", newPrimary)
			if s.deps.AdvisoryResetFunc != nil {
				go s.deps.AdvisoryResetFunc(newPrimary)
			}
		}
	}

	// Always exactly one default: a hive with repos must name a primary_repo that
	// is one of them — that is the repo the advisory issue is maintained in. The
	// Repos tab disables Save until a star is chosen, but enforce it server-side
	// too so no client (or a stale one) can persist repos with no default. Reject
	// and restore the pre-mutation config so the in-memory state stays coherent.
	if final := s.deps.Config.Project.Repos; len(final) > 0 {
		primary := s.deps.Config.Project.PrimaryRepo
		inList := false
		for _, r := range final {
			if r == primary {
				inList = true
				break
			}
		}
		if primary == "" || !inList {
			s.deps.Config.Project.Org = prevOrg
			s.deps.Config.Project.Repos = prevRepos
			s.deps.Config.Project.PrimaryRepo = prevPrimary
			s.deps.Config.GitHub.BaseURL = prevBaseURL
			s.deps.Config.GitHub.APIURL = prevAPIURL
			if s.deps.GHClient != nil {
				s.deps.GHClient.SetOrg(prevOrg)
				s.deps.GHClient.SetRepos(prevRepos)
			}
			jsonError(w, "set a default repo before saving — one of the monitored repos must be marked as the default (the repo where the advisory issue is maintained)", http.StatusBadRequest)
			return
		}
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config", "error", err)
	}
	if s.deps.EnumerateFunc != nil {
		go s.deps.EnumerateFunc()
	}
	s.auditFromRequest(r, "config_governor_repos", auditDetail("section", "repos"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// hiveForgeHost returns the bare GitHub hostname this hive's repos and App live
// on ("github.com" or a GHE hostname like "github.ibm.com"). It is the single
// source of truth the single-host-per-spoke guard compares each submitted repo
// against, and mirrors config.GitHubConfig.HostLabel() (the same value the
// dashboard reads as github_base_url's host). Falls back to public github.com
// when no config is loaded (tests/early boot).

type parsedGovernorRepoRef struct {
	Owner string
	Name  string
	Host  string
	OK    bool
}

func parseGovernorRepoRef(ref string) parsedGovernorRepoRef {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return parsedGovernorRepoRef{}
	}
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		parts := strings.SplitN(strings.TrimPrefix(parsed.Path, "/"), "/", 3)
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			return parsedGovernorRepoRef{Owner: parts[0], Name: parts[1], Host: strings.ToLower(parsed.Host), OK: true}
		}
		return parsedGovernorRepoRef{Host: strings.ToLower(parsed.Host)}
	}
	stripped := strings.Trim(trimmed, "/")
	parts := strings.Split(stripped, "/")
	if len(parts) >= 3 && strings.Contains(parts[0], ".") && parts[1] != "" && parts[2] != "" {
		return parsedGovernorRepoRef{Owner: parts[1], Name: parts[2], Host: strings.ToLower(parts[0]), OK: true}
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" && !strings.Contains(parts[0], ".") {
		return parsedGovernorRepoRef{Owner: parts[0], Name: parts[1], OK: true}
	}
	return parsedGovernorRepoRef{}
}

func governorReposAdoptOrg(currentOrg string, repos []string, primary, spokeHost string) (string, string) {
	adoptOrg := strings.TrimSpace(currentOrg)
	refs := append([]string{}, repos...)
	if strings.TrimSpace(primary) != "" {
		refs = append(refs, primary)
	}
	for _, ref := range refs {
		parsed := parseGovernorRepoRef(ref)
		if parsed.Host != "" && !sameForgeHost(parsed.Host, spokeHost) {
			return "", fmt.Sprintf("repo %q is on %s but this hive is on %s — a hive's repos must all be on one GitHub host. Remove the mismatched repo or use a repo on %s.", strings.TrimSpace(ref), parsed.Host, spokeHost, spokeHost)
		}
		if !parsed.OK || parsed.Owner == "" || strings.EqualFold(parsed.Owner, currentOrg) {
			continue
		}
		if adoptOrg != "" && !strings.EqualFold(adoptOrg, currentOrg) && !strings.EqualFold(adoptOrg, parsed.Owner) {
			return "", fmt.Sprintf("repos name multiple GitHub orgs (%s and %s). A hive can monitor one org at a time; submit repos from a single destination org to migrate.", adoptOrg, parsed.Owner)
		}
		adoptOrg = parsed.Owner
	}
	return adoptOrg, ""
}

func normalizeGovernorRepoRefs(org string, repos []string) []string {
	if len(repos) == 0 {
		return repos
	}
	out := make([]string, len(repos))
	for i, repo := range repos {
		out[i] = normalizeGovernorRepoRef(org, repo)
	}
	return out
}

func normalizeGovernorRepoRef(org, ref string) string {
	parsed := parseGovernorRepoRef(ref)
	if parsed.OK && parsed.Name != "" && (parsed.Owner == "" || strings.EqualFold(parsed.Owner, org)) {
		return parsed.Name
	}
	normalized, _ := config.NormalizeRepoForOrg(org, ref)
	return normalized
}

func (s *Server) hiveForgeHost() string {
	if s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.GitHub.HostLabel()
	}
	return "github.com"
}

// repoRefHostLabel extracts the GitHub host a repo string was pasted with, or ""
// when none was given (a bare "repo" or "owner/repo", which by definition belongs
// to the hive's own forge). Mirrors hub.repoRefHost: a full URL or a leading
// dotted segment ("github.ibm.com/org/repo") names an explicit host. Returned
// lowercased for case-insensitive comparison in sameForgeHost.
func repoRefHostLabel(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.Trim(s, "/")
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "/")
	if len(parts) > 1 && strings.Contains(parts[0], ".") {
		return strings.ToLower(parts[0])
	}
	return ""
}

// sameForgeHost reports whether two host labels refer to the same GitHub. Both
// "" and "github.com" mean public GitHub; a GHE host equals only itself.
// Case-insensitive. Mirrors hub.sameGitHubHost so the dashboard's single-host
// rule matches the hub's request/assign validation exactly.
func sameForgeHost(a, b string) bool {
	norm := func(h string) string {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || h == "github.com" {
			return "github.com"
		}
		return h
	}
	return norm(a) == norm(b)
}

func configuredProjectOrgForGitHubApp(cfg *config.Config, logger *slog.Logger) string {
	if cfg == nil {
		return ""
	}
	org := strings.TrimSpace(cfg.Project.Org)
	if org == "" {
		return ""
	}
	forgeHost := strings.TrimSpace(cfg.GitHub.HostLabel())
	if forgeHost == "" {
		forgeHost = "github.com"
	}
	if !strings.Contains(org, ".") || !sameForgeHost(org, forgeHost) {
		return org
	}

	primary := strings.TrimSpace(cfg.Project.PrimaryRepo)
	if primary == "" && len(cfg.Project.Repos) > 0 {
		primary = strings.TrimSpace(cfg.Project.Repos[0])
	}
	derived := firstRepoPathSegment(primary)
	if derived == "" || strings.EqualFold(derived, org) {
		return org
	}
	if logger != nil {
		logger.Warn("project org is configured as the GitHub forge host; deriving GitHub App organization from primary repo",
			"configured_org", org, "derived_org", derived, "primary_repo", primary, "forge_host", forgeHost)
	}
	return derived
}

func firstRepoPathSegment(ref string) string {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "https://")
	ref = strings.TrimPrefix(ref, "http://")
	ref = strings.Trim(ref, "/")
	if ref == "" {
		return ""
	}
	parts := strings.Split(ref, "/")
	if len(parts) > 1 && strings.Contains(parts[0], ".") {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

// handleGovernorRepoCheckAccess verifies that this hive's GitHub App can access
// a repo the operator is about to add, and — when it cannot — returns the
// per-forge App install/authorize URL so the Repos tab can guide the user to
// grant access before the repo is finally accepted (requirement #3).
//
// The check today probes at ORG granularity: DiscoverInstallationID(org) returns
// ErrNoInstallationForOrg when the App is not installed on the repo's org at all,
// which is the common "new org the App was never installed on" case and the one
// that hard-blocks reads and writes. On a positive result we report ok:true.
//
// TODO(repo-access-probe): tighten to REPO granularity. An App installed on the
// org "Only select repositories" may still lack THIS repo (see #2353's
// write-forbidden case). The deeper probe should mint an installation token and
// GET /repos/{org}/{repo} (s.deps.GHClient.GetRepo) — a 404/403 there means the
// installation exists but this repo is not in its selected set, which needs the
// same "manage installation" URL to add the repo to the selection. That probe is
// scoped out of this change to keep it focused; the org-level check plus the
// install workflow already covers the "App not installed on the org" footgun.
func (s *Server) handleGovernorRepoCheckAccess(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo string `json:"repo"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	ref := sanitizeString(body.Repo)
	if ref == "" {
		jsonError(w, "repo is required", http.StatusBadRequest)
		return
	}
	// Single-host guard here too: an added repo on a different forge is rejected
	// before we even probe access, with the same message as the save path.
	spokeHost := s.hiveForgeHost()
	if h := repoRefHostLabel(ref); h != "" && !sameForgeHost(h, spokeHost) {
		jsonError(w, fmt.Sprintf("repo %q is on %s but this hive is on %s — a hive's repos must all be on one GitHub host.", ref, h, spokeHost), http.StatusBadRequest)
		return
	}
	// Resolve the org the repo lives in. The paste may be "org/repo", a bare
	// "repo" (org is the hive's configured org), or a full URL. Strip any host
	// and pull the org segment when present.
	org := s.deps.Config.Project.Org
	stripped := ref
	stripped = strings.TrimPrefix(stripped, "https://")
	stripped = strings.TrimPrefix(stripped, "http://")
	stripped = strings.Trim(stripped, "/")
	parts := strings.Split(stripped, "/")
	// Drop a leading host segment ("github.ibm.com/org/repo" -> "org/repo").
	if len(parts) > 1 && strings.Contains(parts[0], ".") {
		parts = parts[1:]
	}
	if len(parts) >= 2 && parts[0] != "" {
		org = parts[0]
	}
	if org == "" {
		jsonError(w, "cannot determine the org for this repo — use org/repo format", http.StatusBadRequest)
		return
	}

	// An App-less hive (advisory-only, no private key) cannot and need not probe
	// installation access: it never writes. Treat it as "no access check needed"
	// so the add proceeds — the single-host guard above already ran.
	if s.deps.GHAppAuth == nil || !s.deps.GHAppAuth.HasKey() {
		okResponse(w, map[string]string{"status": "no-app"})
		return
	}

	ctx := s.deps.Ctx
	if ctx == nil {
		ctx = r.Context()
	}
	if _, err := s.deps.GHAppAuth.DiscoverInstallationID(ctx, org); err != nil {
		// Not installed on this org (or discovery failed). Surface the correct
		// per-forge install URL and arm the existing pending-install mechanism so
		// the banner/recheck plumbing the App-required flow already uses drives
		// this to completion. AppInstallURL() returns "" for a GHE host whose App
		// slug was never configured — the UI renders that as a config message
		// rather than a dead link.
		installURL := ""
		if s.deps.Config != nil {
			installURL = s.deps.Config.GitHub.AppInstallURL()
		}
		s.SetPendingGitHubAppInstall()
		s.logger.Info("repo add blocked: app not installed on org", "org", org, "repo", ref, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":           false,
			"needsInstall": true,
			"org":          org,
			"installUrl":   installURL,
			"error":        fmt.Sprintf("the Hive GitHub App is not installed on %q, so it cannot read or write %s. Install/authorize the App for %q, then re-check.", org, ref, org),
		})
		return
	}
	okResponse(w, map[string]string{"status": "ok", "org": org})
}
