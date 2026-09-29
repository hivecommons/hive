package config

import (
	"path"
	"strings"
)

// GitHubActivityConfig gates the hub's org-wide GitHub issue/PR activity feed.
// The feed is default-off. When enabled, the hub polls org repos, diffs issue
// and PR state, and posts concise lines to notifications.discord.factory_webhook.
type GitHubActivityConfig struct {
	Enabled          bool     `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Org              string   `yaml:"org,omitempty" json:"org,omitempty"`
	APIURL           string   `yaml:"api_url,omitempty" json:"api_url,omitempty"`
	PollIntervalS    int      `yaml:"poll_interval_s,omitempty" json:"poll_interval_s,omitempty"`
	Repos            []string `yaml:"repos,omitempty" json:"repos,omitempty"`
	Events           []string `yaml:"events,omitempty" json:"events,omitempty"`
	AllowAuthors     []string `yaml:"allow_authors,omitempty" json:"allow_authors,omitempty"`
	DenyAuthors      []string `yaml:"deny_authors,omitempty" json:"deny_authors,omitempty"`
	FilterBots       *bool    `yaml:"filter_bots,omitempty" json:"filter_bots,omitempty"`
	FilterDependabot *bool    `yaml:"filter_dependabot,omitempty" json:"filter_dependabot,omitempty"`
}

func (g GitHubActivityConfig) EffectiveOrg(projectOrg string) string {
	if strings.TrimSpace(g.Org) != "" {
		return strings.TrimSpace(g.Org)
	}
	return strings.TrimSpace(projectOrg)
}

func (g GitHubActivityConfig) BotsFiltered() bool {
	return g.FilterBots == nil || *g.FilterBots
}

func (g GitHubActivityConfig) DependabotFiltered() bool {
	return g.FilterDependabot == nil || *g.FilterDependabot
}

// ContributeAnnouncement is the operator-set message surfaced to contributors on
// /contribute, the SSE stream, and connected relays. Text is plain, server-
// sanitised display text; Level is "info" or "warning"; ExpiresAt is optional
// RFC3339. ID is minted by the dashboard whenever Text changes so per-viewer
// dismissals reset for a new announcement.
type ContributeAnnouncement struct {
	ID        string `yaml:"id,omitempty" json:"id,omitempty"`
	Text      string `yaml:"text,omitempty" json:"text,omitempty"`
	Level     string `yaml:"level,omitempty" json:"level,omitempty"`
	ExpiresAt string `yaml:"expires_at,omitempty" json:"expires_at,omitempty"`
}

// ContributeConfig groups operator-facing settings for contributor surfaces that
// are not admission/queue controls.
type ContributeConfig struct {
	HelpLinks []ContributeHelpLink `yaml:"help_links,omitempty" json:"help_links,omitempty"`
}

type ContributeHelpLink struct {
	Label string `yaml:"label" json:"label"`
	URL   string `yaml:"url" json:"url"`
}

type HubConfig struct {
	Enabled             bool   `yaml:"enabled"`
	URL                 string `yaml:"url"`
	IsPublic            bool   `yaml:"is_public"`
	SnapshotURL         string `yaml:"snapshot_url"`
	DashboardURL        string `yaml:"dashboard_url"`
	HiveType            string `yaml:"hive_type"`
	ClusterID           string `yaml:"cluster_id"`
	AutoSnapshot        bool   `yaml:"auto_snapshot"`
	AutoUpgrade         bool   `yaml:"auto_upgrade"`
	AutoUpgradeMode     string `yaml:"auto_upgrade_mode,omitempty"`
	ContributeSuspended bool   `yaml:"contribute_suspended"`
	// ContributeWallEnabled opts a hive into the public contributor wall on
	// /contribute. Default false so no deployment gets a public posting surface
	// without an operator decision.
	ContributeWallEnabled bool `yaml:"contribute_wall_enabled,omitempty"`
	// ContributeWallRetentionDays bounds persisted contributor-wall posts on the
	// hub data volume. 0/unset resolves to the dashboard's 90-day default.
	ContributeWallRetentionDays int `yaml:"contribute_wall_retention_days,omitempty"`
	// NPSEnabled opts this hive's dashboard into the NPS feedback prompt, whose
	// responses are forwarded to the hub (issue #9610). A POINTER so an absent
	// value means "use the default": ON for hub-provisioned hosted spokes, OFF
	// for every self-hosted or standalone install, because the hive never sends
	// data off-box without an explicit operator opt-in. HIVE_NPS_ENABLED
	// overrides it. Resolve with NPSFeedbackEnabled(), never the raw field.
	NPSEnabled *bool `yaml:"nps_enabled,omitempty"`
	// NPSRelayURL is the base URL of the hivecommons NPS relay (issue #9619).
	// A standalone spoke (NPS enabled, no hub link) registers a self-generated
	// Ed25519 key at <url>/register and POSTs signed responses here; a hub
	// pulls from <url>/pending and acks at <url>/ack. Empty (the default)
	// disables the relay path entirely. HIVE_NPS_RELAY_URL overrides it.
	// Resolve with EffectiveNPSRelayURL(), never the raw field.
	NPSRelayURL string `yaml:"nps_relay_url,omitempty"`
	// NPSRelayPullSecret is the hub's credential for pulling and acking relay
	// entries (issue #9619). Hub-only and a secret: never logged, and excluded
	// from JSON. HIVE_NPS_RELAY_PULL_SECRET overrides it.
	NPSRelayPullSecret string `yaml:"nps_relay_pull_secret,omitempty" json:"-"`
	// Contribute title/author/label filters use a single list plus a mode:
	//   - FilterModeAllow ("allow"): allowlist — an item passes ONLY if it
	//     matches the list (a non-empty list is required for the filter to gate;
	//     an empty allow list means "no items pass" is intentionally avoided —
	//     see passesContributeFilter, where an empty allow list is treated as
	//     "filter off" so a half-configured filter never silently blocks all).
	//   - FilterModeDeny ("deny", default): denylist — an item is skipped if it
	//     matches the list; everything else passes.
	// The *DenyTitles/*DenyAuthors/*DenyLabels fields hold the LIST for each
	// filter regardless of mode (names kept for backward compatibility with
	// existing on-disk config; the mode decides allow vs deny). ContributeAllowLabels
	// is retained only for one-time migration into DenyLabels+LabelsMode.
	ContributeTitlesMode  string   `yaml:"contribute_titles_mode,omitempty"`
	ContributeAuthorsMode string   `yaml:"contribute_authors_mode,omitempty"`
	ContributeLabelsMode  string   `yaml:"contribute_labels_mode,omitempty"`
	ContributeAllowLabels []string `yaml:"contribute_allow_labels"`
	ContributeDenyLabels  []string `yaml:"contribute_deny_labels"`
	// ContributeNeedsDecisionLabel is applied by contributor relays when an agent
	// concludes an issue is waiting on a maintainer decision. Nil means the default
	// needs-decision label; an explicit empty string disables relay labelling and
	// does not add a decision label to the skip set.
	ContributeNeedsDecisionLabel *string `yaml:"contribute_needs_decision_label,omitempty"`
	// ContributeSkipLabels is the hive-wide "not contributor work" label set.
	// Matching is case-insensitive and uses path.Match-style glob patterns (not
	// substring matching), so "discussion" matches that label and
	// "wayfinder:*" matches "wayfinder:grilling". The effective set defaults to
	// DefaultContributeSkipLabels and always includes "blocked" as the historical
	// floor, even if an operator-supplied list omits it.
	ContributeSkipLabels          []string `yaml:"contribute_skip_labels,omitempty"`
	ContributeDenyTitles          []string `yaml:"contribute_deny_titles"`
	ContributeDenyAuthors         []string `yaml:"contribute_deny_authors"`
	ContributeAllowModels         []string `yaml:"contribute_allow_models"`
	ContributeRejectUnknownModels bool     `yaml:"contribute_reject_unknown_models"`
	// ContributeMinReasoningEffort is the contributor reasoning-effort floor:
	// a relay whose reported reasoning_effort ranks below it on
	// ReasoningEffortLadder (minimal < low < medium < high < xhigh < max) is
	// rejected at connect time. Empty means no floor. The comparison is
	// per-backend (ReasoningEffortMeetsFloor), so a floor above a backend's
	// highest level is satisfied by that backend's highest level.
	ContributeMinReasoningEffort string `yaml:"contribute_min_reasoning_effort,omitempty"`
	// ContributeRejectUnknownEffort decides how a relay whose effort cannot be
	// ranked (empty, unrecognised, or invalid for its backend) is treated when
	// a floor is set: rejected when true, admitted when false — the effort
	// analogue of ContributeRejectUnknownModels.
	ContributeRejectUnknownEffort bool `yaml:"contribute_reject_unknown_effort,omitempty"`
	// ContributeRepoFilters are optional, full owner/name keyed admission
	// filters layered on top of the hive-wide title/author/label filters.
	ContributeRepoFilters map[string]ContributeRepoFilter `yaml:"contribute_repo_filters,omitempty" json:"contribute_repo_filters,omitempty"`
	// ContributeSkipAssignedToOthers, when true, makes the /contribute queue
	// skip any issue that is already assigned to someone OTHER than the
	// contributor requesting work. An issue assigned to the contributor
	// themselves (or unassigned) is still eligible. Default false preserves the
	// prior behavior of handing out issues regardless of assignment (#2357).
	ContributeSkipAssignedToOthers bool `yaml:"contribute_skip_assigned_to_others"`
	// ContributeCooldownEnabled toggles the POST-COMPLETION cooldown that keeps a
	// just-worked issue out of the /contribute queue for a while (see
	// contribute_ws.go markTaskCompleted / isTaskInCooldown). It is a POINTER so
	// that an absent value (older on-disk config that predates this toggle) means
	// "unset" and defaults to ENABLED — the prior, backward-compatible behavior.
	// A non-nil false explicitly DISABLES cooldown gating (no completed issue is
	// ever excluded from the queue for cooldown; failure quarantine is unaffected
	// and stays on). Use IsContributeCooldownEnabled() to resolve the effective
	// value rather than reading the pointer directly.
	ContributeCooldownEnabled *bool `yaml:"contribute_cooldown_enabled,omitempty"`
	// ContributeCooldownHours is the WITH-PR completion cooldown period in hours —
	// the operator-tunable replacement for the hardcoded default of
	// contributeCooldownDefaultHours (168h / one week). 0 or unset means "use the
	// default"; any positive value is clamped to
	// [contributeCooldownMinHours, contributeCooldownMaxHours] in Normalize.
	// Resolve with ContributeCooldownHoursOrDefault(), never by reading the raw
	// field (which may legitimately be 0 == default). The short NO-PR cooldown is
	// left as its own const and is not tuned here (the operator specifically asked
	// for the week-long period to be adjustable).
	ContributeCooldownHours int `yaml:"contribute_cooldown_hours,omitempty"`
	// TaskMCPRelatedWorkRecencyDays bounds how far back task MCP related_work()
	// includes merged/closed work when serving cache-only context.
	TaskMCPRelatedWorkRecencyDays int `yaml:"task_mcp_related_work_recency_days,omitempty" json:"task_mcp_related_work_recency_days,omitempty"`
	// TaskMCPHistoryLimit bounds cached history() rows before pagination.
	TaskMCPHistoryLimit int `yaml:"task_mcp_history_limit,omitempty" json:"task_mcp_history_limit,omitempty"`
	// TaskMCPKnowledgeLimit bounds cached knowledge() results before pagination.
	TaskMCPKnowledgeLimit int `yaml:"task_mcp_knowledge_limit,omitempty" json:"task_mcp_knowledge_limit,omitempty"`
	// TaskMCPDependenciesLimit bounds dependency graph rows before pagination.
	TaskMCPDependenciesLimit int `yaml:"task_mcp_dependencies_limit,omitempty" json:"task_mcp_dependencies_limit,omitempty"`
	// ContributeCloseAlreadyDone lets the hub close an issue when a contributor's
	// no_work_needed verdict is explicitly "already done" and the cited PR/commit
	// verifies as landed on the repo's default branch. Nil defaults OFF: the safe
	// default is comment + label, so maintainers can close after reviewing.
	ContributeCloseAlreadyDone *bool `yaml:"contribute_close_already_done,omitempty"`
	// ContributeAlreadyDoneLabel is applied to issues a contributor found already
	// resolved. It is also in the default contribute skip label set, so labelled
	// issues stay out of the offer queue until a maintainer removes it.
	ContributeAlreadyDoneLabel string `yaml:"contribute_already_done_label,omitempty"`
	// ContributeAlreadyDoneHoldDays is the longer offer suppression for
	// an "already done" verdict while the label/comment path is pending or if
	// cited evidence cannot be verified. It is
	// deliberately separate from ContributeCooldownHours so operators can shorten
	// ordinary task cooldowns without re-offering likely-settled issues daily.
	ContributeAlreadyDoneHoldDays int `yaml:"contribute_already_done_hold_days,omitempty"`
	// Deprecated: use contribute_already_done_hold_days. Kept so existing
	// experimental configs from the partial #8477 branch continue to load.
	ContributeAlreadyDoneUnverifiedHoldDays int `yaml:"contribute_already_done_unverified_hold_days,omitempty"`
	// ContributeQueueOrder is the OPERATOR PRIORITY OVERRIDE for the ready-work
	// queue: an ordered list of "owner/repo#number" keys the operator dragged to
	// the front on the Operations tab. When set, these issues are OFFERED FIRST —
	// both in the queue display (ReadyQueue) and in selectTask's candidate ordering
	// — in exactly this order; everything else follows in the established default
	// order. It only reorders OFFER PRIORITY: a key here that is filtered out by
	// admission / cooldown / disabled-repo / in-flight rules is still excluded, and
	// a stale key (no longer actionable) is simply skipped. Persisted through the
	// same Config.Hub.* mechanism as the other admission settings so it survives
	// restart, and edited only through the authenticated PUT /api/contribute/queue/order
	// endpoint (owner/read-write only).
	ContributeQueueOrder []string `yaml:"contribute_queue_order,omitempty"`
	// ContributeQueueHold is the OPERATOR HOLD set for the ready-work queue: an
	// unordered list of "owner/repo#number" keys the operator parked from the
	// Operations tab. A held issue is NEVER offered — it is excluded from BOTH the
	// queue display's offer-eligible set (ReadyQueue) and selectTask's candidate
	// selection — and it stays parked INDEFINITELY until the operator Resumes it.
	// This is DISTINCT from cooldown (time-based, self-clearing): a hold is a
	// manual, persistent operator decision. Held rows remain VISIBLE on the
	// Operations tab (rendered greyed with an "on hold" badge) so the operator can
	// always see and Resume them. Persisted through the same Config.Hub.* mechanism
	// as ContributeQueueOrder so it survives restart, and edited only through the
	// authenticated POST /api/contribute/queue/hold endpoint (owner/read-write only).
	ContributeQueueHold []string `yaml:"contribute_queue_hold,omitempty"`
	// ContributeQueueHoldReasons is an OPTIONAL parallel map (canonical
	// "owner/repo#number" key -> short operator note) annotating why an issue in
	// ContributeQueueHold was parked. It is a companion to — not a replacement for —
	// ContributeQueueHold: the []string set above remains the authoritative source of
	// truth for WHICH issues are held (every admission check reads it); this map only
	// carries the human-facing REASON, surfaced in the on-hold badge tooltip. A hold
	// with no reason simply has no entry here (the badge falls back to its generic
	// text), so holding without a note works exactly as before. Kept as a parallel
	// map, rather than folding the reason into ContributeQueueHold, so the many
	// read sites of the []string set stay untouched. Written only by the same
	// authenticated POST /api/contribute/queue/hold endpoint that maintains the set,
	// and pruned to the held keys on every write so it never leaks stale reasons.
	ContributeQueueHoldReasons map[string]string `yaml:"contribute_queue_hold_reasons,omitempty"`
	// ContributeRequireExplicitAccept gates HOW a contributor's scoped GitHub
	// credential is delivered relative to task acceptance (kubestellar/hive#2537).
	// The credential is ALWAYS delivered only AFTER an acceptance decision — it no
	// longer travels bundled in the task_assign message. This toggle only chooses
	// WHO makes that decision:
	//   - nil / false (DEFAULT): trusted-source AUTO-ACCEPT. A task that already
	//     passed admission (the title/author/label filters, disabled-repo/tier
	//     gates, cooldown, and the per-tier trust gate in selectTask) is
	//     auto-accepted the instant it is assigned, and the scoped credential is
	//     delivered immediately after — no human in the loop. This keeps an
	//     unattended fleet running exactly as before: the only observable change is
	//     that the credential arrives in a distinct message right after task_assign
	//     rather than inside it.
	//   - true: EXPLICIT (manual/human) acceptance. The hub withholds the credential
	//     until the client sends a task_accepted for the assigned task; a task that
	//     is never accepted (declined, timed out, or reconnected away) never
	//     receives a credential. This is the opt-in "mandatory acceptance" mode for
	//     operators who want a wait state.
	// A POINTER so an absent value (older on-disk config) resolves to the
	// backward-compatible auto-accept default via IsContributeRequireExplicitAccept().
	ContributeRequireExplicitAccept *bool `yaml:"contribute_require_explicit_accept,omitempty"`
	// ContributeDelegatableRoles is the hive-wide allow-list of spoke agent roles
	// a contributor relay may request via HIVE_AGENT_ROLE / auth_response.role.
	// Empty means the safe default set: scanner, quality, outreach. Privileged roles
	// (ci-maintainer, sec-check, architect) must be explicitly listed here AND
	// granted on the contributor profile; supervisor is never delegatable.
	ContributeDelegatableRoles []string `yaml:"contribute_delegatable_roles,omitempty"`
	// StandbyContributors is the hive-wide approved list for standby
	// (RFC #7629): the GitHub logins whose relays may be offered work from a
	// lane paused for budget. Approval is durable and lives here; a relay
	// declaring standby is volunteering, which grants nothing. A login here
	// grants nothing else either — not a trust tier, not a role, not a
	// credential. Empty (the default) means standby can offer work to nobody,
	// and a lane with `standby.enabled: true` and an empty list fails the
	// load rather than sitting inert. Resolve through
	// StandbyContributorSet()/IsStandbyContributorApproved().
	StandbyContributors []string `yaml:"standby_contributors,omitempty"`
	// StandbyModelTiers maps whole contributor model configurations to the
	// RFC #6825 capability tiers. Owner-authored, and it ships EMPTY with no
	// defaults: an unmapped configuration is unknown, and unknown never
	// qualifies, so a hive that has not written this mapping reports "0
	// qualify" rather than admitting a model nobody assessed.
	StandbyModelTiers []StandbyModelTier `yaml:"standby_model_tiers,omitempty"`
	// StandbyItemTiers maps classes of WORK ITEM to the tier a donated
	// configuration must have to be offered one. Owner-authored, and it ships
	// EMPTY: with no entries, item-tier matching is not in force at all and
	// the lane floor decides alone, exactly as it did before S7. An item that
	// is not on a non-empty list is `unknown`, and an unknown item is not
	// standby-eligible.
	//
	// It is the authoritative T3 list. Hive's classifier can PROPOSE a tier
	// for an item, and no proposal can put an item on this list or widen what
	// the list says — see `standby.ItemTiers` and the design record's answer
	// to the RFC's fourth open question.
	StandbyItemTiers []StandbyItemTier `yaml:"standby_item_tiers,omitempty"`
	// StandbyAllowPrivateRepos opts standby dispatch into private
	// repositories. Default OFF: a standby contributor receives the full task
	// context, which for a private repository is read access in substance.
	// Resolve through IsStandbyPrivateReposAllowed().
	StandbyAllowPrivateRepos bool                   `yaml:"standby_allow_private_repos,omitempty"`
	ContributeAnnouncement   ContributeAnnouncement `yaml:"contribute_announcement,omitempty" json:"contribute_announcement,omitempty"`
	DisabledRepos            []string               `yaml:"disabled_repos"`
	DisabledTiers            []string               `yaml:"disabled_tiers"`
	TierLimits               map[string]TierRate    `yaml:"tier_limits"`
	SnapshotIntervalMin      int                    `yaml:"snapshot_interval_min"`
	// NPSTiming overrides the dashboard NPS prompt's eligibility rules (issue
	// #9610). Every zero field keeps the console-identical default; resolve
	// with EffectiveNPSTiming(), never the raw fields. See nps_options_config.go.
	NPSTiming NPSTimingConfig `yaml:"nps_timing,omitempty"`
	// NPSDetractorIssues opts this hive into letting a detractor (score 1)
	// open a PUBLIC issue from their NPS feedback, with explicit consent.
	// Default OFF. See nps_options_config.go.
	NPSDetractorIssues NPSDetractorIssuesConfig `yaml:"nps_detractor_issues,omitempty"`
}

// Contribute completion-cooldown defaults and clamp bounds. These live in the
// config package because both the resolver methods below and the Normalize path
// reference them; the dashboard keeps its own equal DEFAULT const
// (completedTaskCooldownHours) as the runtime fallback for hubs built without a
// Config (e.g. direct-in-test construction).
const (
	// DefaultTaskMCPRelatedWorkRecencyDays is the cache-only related_work()
	// recency window when hive.yaml does not override it.
	DefaultTaskMCPRelatedWorkRecencyDays = 14
	DefaultTaskMCPHistoryLimit           = 20
	DefaultTaskMCPKnowledgeLimit         = 20
	DefaultTaskMCPDependenciesLimit      = 20
	// contributeCooldownDefaultHours is the with-PR completion cooldown used when
	// ContributeCooldownHours is unset/0 — one week, matching the historical
	// hardcoded default.
	contributeCooldownDefaultHours = 168
	// contributeCooldownMinHours / contributeCooldownMaxHours clamp an
	// operator-supplied period to a sane range (one hour .. one year) so a stray
	// value cannot park an issue effectively forever or disable the cooldown by
	// rounding to zero.
	contributeCooldownMinHours        = 1
	contributeCooldownMaxHours        = 8760
	contributeAlreadyDoneLabelDefault = "hive/already-done"
	contributeAlreadyDoneDefaultDays  = 30
	contributeAlreadyDoneMinDays      = 1
	contributeAlreadyDoneMaxDays      = 365
)

// IsContributeCooldownEnabled resolves the effective on/off state of the
// post-completion cooldown. A nil pointer (unset, older config) defaults to
// ENABLED for backward compatibility; an explicit false disables it.
func (h HubConfig) IsContributeCooldownEnabled() bool {
	return h.ContributeCooldownEnabled == nil || *h.ContributeCooldownEnabled
}

// IsContributeCloseAlreadyDone resolves the verified already-done auto-close
// toggle. Unset defaults to false; the default action is comment + label.
func (h HubConfig) IsContributeCloseAlreadyDone() bool {
	return h.ContributeCloseAlreadyDone != nil && *h.ContributeCloseAlreadyDone
}

// ContributeAlreadyDoneLabelOrDefault resolves the label that marks issues a
// contributor found already resolved.
func (h HubConfig) ContributeAlreadyDoneLabelOrDefault() string {
	if label := strings.TrimSpace(h.ContributeAlreadyDoneLabel); label != "" {
		return label
	}
	return contributeAlreadyDoneLabelDefault
}

// IsContributeRequireExplicitAccept resolves the effective acceptance mode for
// contributor credential delivery (kubestellar/hive#2537). A nil pointer (unset,
// older config) resolves to FALSE — trusted-source auto-accept — so an existing
// deployment keeps handing credentials to admitted tasks without a wait state; an
// explicit true opts into mandatory (human/manual) acceptance where the credential
// is withheld until the client accepts the assigned task.
func (h HubConfig) IsContributeRequireExplicitAccept() bool {
	return h.ContributeRequireExplicitAccept != nil && *h.ContributeRequireExplicitAccept
}

func (h HubConfig) TaskMCPRelatedWorkRecencyDaysOrDefault() int {
	if h.TaskMCPRelatedWorkRecencyDays > 0 {
		return h.TaskMCPRelatedWorkRecencyDays
	}
	return DefaultTaskMCPRelatedWorkRecencyDays
}

func (h HubConfig) TaskMCPHistoryLimitOrDefault() int {
	if h.TaskMCPHistoryLimit > 0 {
		return h.TaskMCPHistoryLimit
	}
	return DefaultTaskMCPHistoryLimit
}

func (h HubConfig) TaskMCPKnowledgeLimitOrDefault() int {
	if h.TaskMCPKnowledgeLimit > 0 {
		return h.TaskMCPKnowledgeLimit
	}
	return DefaultTaskMCPKnowledgeLimit
}

func (h HubConfig) TaskMCPDependenciesLimitOrDefault() int {
	if h.TaskMCPDependenciesLimit > 0 {
		return h.TaskMCPDependenciesLimit
	}
	return DefaultTaskMCPDependenciesLimit
}

const ContributeSkipLabelsEnvVar = "HIVE_CONTRIBUTE_SKIP_LABELS"

var defaultContributeSkipLabels = []string{
	"blocked",
	contributeAlreadyDoneLabelDefault,
	"tracking",
	"epic",
	"discussion",
	"question",
	"needs-decision",
	"needs-triage",
}

// DefaultContributeSkipLabels returns the default hive-wide "not contributor
// work" label patterns. Callers receive a copy so tests and UI code cannot
// mutate the process-wide defaults.
func DefaultContributeSkipLabels() []string {
	out := make([]string, len(defaultContributeSkipLabels))
	copy(out, defaultContributeSkipLabels)
	return out
}

func parseContributeSkipLabels(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if label := strings.TrimSpace(part); label != "" {
			out = append(out, label)
		}
	}
	return out
}

func normalizeContributeSkipLabels(labels []string, needsDecisionLabel string) []string {
	if len(labels) == 0 {
		labels = DefaultContributeSkipLabels()
	}
	out := make([]string, 0, len(labels)+1)
	seen := map[string]struct{}{}
	add := func(label string) {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "" {
			return
		}
		if _, ok := seen[label]; ok {
			return
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	for _, label := range labels {
		add(label)
	}
	add(blockedWorkflowSkipLabel)
	add(needsDecisionLabel)
	return out
}

const (
	blockedWorkflowSkipLabel  = "blocked"
	defaultNeedsDecisionLabel = "needs-decision"
)

// ContributeNeedsDecisionLabelOrDefault resolves the label relays should apply
// to issues that are waiting on a maintainer decision. An explicit empty string
// disables label application; unset config uses the default label.
func (h HubConfig) ContributeNeedsDecisionLabelOrDefault() string {
	if h.ContributeNeedsDecisionLabel == nil {
		return defaultNeedsDecisionLabel
	}
	return strings.TrimSpace(*h.ContributeNeedsDecisionLabel)
}

// ContributeSkipLabelPatterns resolves the effective hive-wide "not contributor
// work" label patterns. It applies the default plus the historical
// blocked-label and already-done-label floors defensively so tests and direct
// HubConfig literals behave like loaded config.
func (h HubConfig) ContributeSkipLabelPatterns() []string {
	labels := h.ContributeSkipLabels
	if len(labels) == 0 {
		labels = DefaultContributeSkipLabels()
	}
	labels = append(append([]string{}, labels...), h.ContributeAlreadyDoneLabelOrDefault())
	return normalizeContributeSkipLabels(labels, h.ContributeNeedsDecisionLabelOrDefault())
}

// MatchContributeSkipLabel returns the issue label that matches the configured
// contributor-skip set. Patterns are case-insensitive and use path.Match-style
// glob syntax; invalid patterns fall back to exact case-insensitive matching so
// a typo cannot broaden the skip set.
func (h HubConfig) MatchContributeSkipLabel(labels []string) (string, bool) {
	patterns := h.ContributeSkipLabelPatterns()
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			continue
		}
		candidate := strings.ToLower(trimmed)
		for _, pattern := range patterns {
			matched, err := path.Match(pattern, candidate)
			if err != nil {
				matched = candidate == strings.ToLower(strings.TrimSpace(pattern))
			}
			if matched {
				return trimmed, true
			}
		}
	}
	return "", false
}

var defaultContributeDelegatableRoles = []string{"scanner", "quality", "outreach"}

// ContributeDelegatableRoleSet resolves the hive-wide allow-list of spoke roles
// a clanker may claim. Empty config preserves the safe v1 default; supervisor is
// deliberately removed even if an operator lists it because it manages the fleet.
func (h HubConfig) ContributeDelegatableRoleSet() map[string]bool {
	out := make(map[string]bool, len(defaultContributeDelegatableRoles)+len(h.ContributeDelegatableRoles))
	for _, role := range defaultContributeDelegatableRoles {
		out[role] = true
	}
	roles := h.ContributeDelegatableRoles
	for _, role := range roles {
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "" || role == "supervisor" {
			continue
		}
		out[role] = true
	}
	return out
}

// IsContributeRoleDelegatable reports whether role is enabled hive-wide for
// clanker delegation. It does not make any per-contributor or trust-tier decision.
func (h HubConfig) IsContributeRoleDelegatable(role string) bool {
	return h.ContributeDelegatableRoleSet()[strings.ToLower(strings.TrimSpace(role))]
}

// ContributeCooldownHoursOrDefault resolves the with-PR cooldown PERIOD in
// hours. A value <= 0 (unset) yields the default (contributeCooldownDefaultHours);
// a positive value is returned as-is (Normalize has already clamped any stored
// value to the valid range, and this method re-clamps defensively for callers
// that build a Hub without running Normalize, e.g. tests).
func (h HubConfig) ContributeCooldownHoursOrDefault() int {
	if h.ContributeCooldownHours <= 0 {
		return contributeCooldownDefaultHours
	}
	if h.ContributeCooldownHours < contributeCooldownMinHours {
		return contributeCooldownMinHours
	}
	if h.ContributeCooldownHours > contributeCooldownMaxHours {
		return contributeCooldownMaxHours
	}
	return h.ContributeCooldownHours
}

// ContributeAlreadyDoneHoldDaysOrDefault resolves the longer hold for
// already-done verdicts.
func (h HubConfig) ContributeAlreadyDoneHoldDaysOrDefault() int {
	days := h.ContributeAlreadyDoneHoldDays
	if days <= 0 {
		days = h.ContributeAlreadyDoneUnverifiedHoldDays
	}
	if days <= 0 {
		return contributeAlreadyDoneDefaultDays
	}
	if days < contributeAlreadyDoneMinDays {
		return contributeAlreadyDoneMinDays
	}
	if days > contributeAlreadyDoneMaxDays {
		return contributeAlreadyDoneMaxDays
	}
	return days
}

type TierRate struct {
	MaxPerHour    int `yaml:"max_per_hour" json:"max_per_hour"`
	MaxPerDay     int `yaml:"max_per_day" json:"max_per_day"`
	MaxConcurrent int `yaml:"max_concurrent" json:"max_concurrent"`
}
