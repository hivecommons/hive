package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stageengine"
	"gopkg.in/yaml.v3"
)

type GovernorConfig struct {
	Modes                map[string]ModeConfig `yaml:"modes"`
	EvalIntervalS        int                   `yaml:"eval_interval_s"`
	EvalIntervalMaxS     int                   `yaml:"eval_interval_max_s,omitempty"`
	EvalIntervalWebhookS int                   `yaml:"eval_interval_webhook_s,omitempty"`
	// WebhookWakeDebounceS is how long the governor loop waits after a
	// cache-invalidating GitHub webhook before running an early eval cycle,
	// so a burst of deliveries (push + check fan-out) coalesces into one
	// cycle. WebhookWakeMinGapS is the minimum gap between two
	// webhook-triggered cycles. Both default at Load; see
	// docs/operator-reference.md "Webhook-driven PR cache invalidation".
	WebhookWakeDebounceS int `yaml:"webhook_wake_debounce_s,omitempty"`
	WebhookWakeMinGapS   int `yaml:"webhook_wake_min_gap_s,omitempty"`
	// CIFailureWebhookKick reacts to webhook CI failures without waiting
	// for a sweep. Cooldown bounds kicks per PR head (default 600 seconds).
	CIFailureWebhookKick   *bool `yaml:"ci_failure_webhook_kick,omitempty"`
	CIFailureKickCooldownS int   `yaml:"ci_failure_kick_cooldown_s,omitempty"`
	// MergeTrainStallWarnS is the stalled-train warning threshold in seconds.
	MergeTrainStallWarnS       int `yaml:"merge_train_stall_warn_s,omitempty"`
	ConserveIntervalMultiplier int `yaml:"conserve_interval_multiplier,omitempty"`
	OptionalSweepEveryNCycles  int `yaml:"optional_sweep_every_n_cycles,omitempty"`
	// ExplainMode is the hive-wide default explain mode for agents that leave
	// their own explain_mode unset. "" means "no hive default configured", in
	// which case ExplainModeEnvVar is consulted and then off — see
	// ResolveExplainModeDefault for the full precedence and why the setting
	// lives here and not only in the environment (#4712).
	//
	// Valid values: "" | off | brief | full — the same set as the per-agent
	// field, validated by ValidateExplainMode.
	ExplainMode string        `yaml:"explain_mode,omitempty"`
	Labels      LabelsConfig  `yaml:"labels"`
	Sensing     SensingConfig `yaml:"sensing"`
	// AgentRuntimeGovernorConfig groups the governor-nested keys the agent
	// runtime owns (watchdog, engines, gateways, trajectory, rotation).
	// Inlined so the YAML shape is unchanged (hivecommons/hive#13536).
	AgentRuntimeGovernorConfig `yaml:",inline"`
	Health                     HealthConfig  `yaml:"health"`
	Budget                     BudgetConfig  `yaml:"budget"`
	Logging                    LoggingConfig `yaml:"logging"`
	// Backup holds the location of the self-service backup encryption key.
	// It records a PATH only, never the key value: hosted spoke owners have no
	// deployment-env access, so the key has to be settable through this
	// (governor) config surface rather than only through HIVE_BACKUP_KEY.
	Backup BackupConfig `yaml:"backup,omitempty" json:"backup,omitempty"`
	Replan ReplanConfig `yaml:"replan"`
	// Reroute tunes redispatching a returned PR to another online clanker.
	Reroute RerouteConfig `yaml:"reroute,omitempty" json:"reroute,omitempty"`
	// AttributionTrailer controls the VISIBLE invocation-attribution line
	// ("— hive: agent=… backend=… model=…") appended at creation time to the
	// body of PRs the hive opens for agents (the PR-request watcher) and issues
	// the hive itself creates. It is a *bool so absent (nil) is distinct from an
	// explicit false: default is ON (see AttributionTrailerEnabled), matching
	// the github.app_authored_prs convention. It gates ONLY the visible trailer
	// — the audit-log entry for every such creation is written unconditionally,
	// so turning this off never removes the operator's ability to answer "which
	// backend/model produced this PR?".
	AttributionTrailer *bool `yaml:"attribution_trailer,omitempty"`

	// ThresholdScaling selects how the DEFAULT mode thresholds scale with the
	// number of repos this hive watches (#3498). An explicit
	// governor.modes.<mode>.threshold is never scaled — it always wins.
	//
	// Valid values: "" (= linear) | linear | sqrt | none.
	// See EffectiveThreshold for the resolution rules and why linear is the
	// default.
	ThresholdScaling string `yaml:"threshold_scaling,omitempty" json:"threshold_scaling,omitempty"`

	// CoverageTarget is the test-coverage goal, in percent (1–100), that the
	// quality card, the ci-maintainer pct-bar, the agent coverage preamble, and
	// the ACMM advisor's L6 coverage gate measure against. 0 means
	// unset and resolves to DefaultCoverageTarget — see EffectiveCoverageTarget.
	CoverageTarget int `yaml:"coverage_target,omitempty" json:"coverage_target,omitempty"`

	// FirstTimerWelcome is the one-time comment the hive leaves on a
	// first-time contributor's PR inviting them to run their own hive
	// (see pkg/config/first_timer_welcome.go). Default ON.
	FirstTimerWelcome FirstTimerWelcomeConfig `yaml:"first_timer_welcome,omitempty" json:"first_timer_welcome,omitempty"`

	// CadenceOwners records WHO last set each governor mode cadence, keyed
	// mode → agent → owner (FieldOwnerOperator). It is the cadence analogue of
	// AgentConfig.ModelOwner/BackendOwner (#5558): a pack could never stomp an
	// operator's model, but could always stomp their cadence — the asymmetry
	// behind #5632, where every steady-state ApplyPack silently reverted
	// operator-set cadences to the pack defaults. Only operator claims are
	// recorded; an absent entry means "pack-owned (or pre-dating this field)",
	// which keeps existing hives on today's behavior until an operator
	// actually edits a cadence.
	//
	// Unlike ThresholdsSource this IS per-entry: cadences cannot invert a mode
	// ladder the way thresholds can, and per-entry is exactly the granularity
	// the Governor grid edits at. It lives here, not in ModeConfig, because
	// ModeConfig's flat YAML map treats every non-`threshold` key as an agent
	// cadence.
	CadenceOwners map[string]map[string]string `yaml:"cadence_owners,omitempty" json:"cadence_owners,omitempty"`

	// CadenceScope selects whether governor mode is resolved from the whole
	// hive queue (aggregate, the historical behavior) or separately for each
	// successfully scanned repo. Repo-scoped mode resolution compares each
	// repo's own actionable queue depth to the base thresholds, so repo-count
	// threshold scaling does not apply in that scope.
	//
	// Valid values: "" (= aggregate) | aggregate | per_repo.
	CadenceScope string `yaml:"cadence_scope,omitempty" json:"cadence_scope,omitempty"`

	// ThresholdsSource records WHERE the explicit thresholds in Modes came
	// from, so "explicit always wins" can apply to the values an operator
	// typed without also applying to the ones an ACMM pack seeded (#4037).
	//
	// Only ThresholdSourcePack is meaningful; empty means "operator-set, or
	// pre-dating this field", which is the safe reading for every hive that
	// already exists — see EffectiveThreshold for why that direction was
	// chosen over defaulting the other way.
	//
	// It is a WHOLE-SET marker rather than one per mode. Per-mode provenance
	// was the obvious shape and it is a trap: an operator who hand-tunes only
	// `surge` would leave `busy` and `quiet` still scaling, and a scaled
	// busy on a 39-repo hive (5 x 39 = 195) sitting above an unscaled surge of
	// 30 INVERTS the mode ladder. Treating the thresholds as one set means the
	// moment an operator edits any of them, all three become theirs verbatim,
	// which cannot invert. It also keeps `threshold_source` out of ModeConfig's
	// flat YAML map, where every non-`threshold` key is an agent cadence.
	ThresholdsSource string `yaml:"thresholds_source,omitempty" json:"thresholds_source,omitempty"`

	// Advisory tunes the advisory digest experience: how many findings the
	// digest shows, and how long a finding may go un-reconfirmed before the
	// hive retires it. See AdvisoryConfig.
	Advisory AdvisoryConfig `yaml:"advisory,omitempty" json:"advisory,omitempty"`

	// AutoCloseSuperseded closes only hive-authored PRs superseded by a
	// different merged PR. A person's PR still needs their decision.
	AutoCloseSuperseded *bool `yaml:"auto_close_superseded,omitempty" json:"auto_close_superseded,omitempty"`

	// Hotspot* tune the merge-hotspot advisor. Zero values use the defaults;
	// disabling issues keeps reporting enabled without filing on GitHub.
	HotspotEnabled     *bool `yaml:"hotspot_enabled,omitempty" json:"hotspot_enabled,omitempty"`
	HotspotWindowDays  int   `yaml:"hotspot_window_days,omitempty" json:"hotspot_window_days,omitempty"`
	HotspotMinPRs      int   `yaml:"hotspot_min_prs,omitempty" json:"hotspot_min_prs,omitempty"`
	HotspotMinOverlaps int   `yaml:"hotspot_min_overlaps,omitempty" json:"hotspot_min_overlaps,omitempty"`
	HotspotMinLines    int   `yaml:"hotspot_min_lines,omitempty" json:"hotspot_min_lines,omitempty"`
	HotspotRefileDays  int   `yaml:"hotspot_refile_days,omitempty" json:"hotspot_refile_days,omitempty"`
	// HotspotMaxOpen caps the modularize issues open at once per repo; the
	// advisor files the next-ranked hotspot only when one closes.
	// HotspotMaxPerPass caps new issues per advisor pass.
	HotspotMaxOpen    int `yaml:"hotspot_max_open,omitempty" json:"hotspot_max_open,omitempty"`
	HotspotMaxPerPass int `yaml:"hotspot_max_per_pass,omitempty" json:"hotspot_max_per_pass,omitempty"`

	// ProjectObservability configures what the telemetry and operations agents
	// recommend for the MANAGED PROJECT. It is deliberately separate from
	// Config.OTel/Tracing, which export Hive's own telemetry.
	ProjectObservability ProjectObservabilityConfig `yaml:"project_observability,omitempty" json:"project_observability,omitempty"`

	// WorkSource selects where hive reads work items (Step 01 of the loop).
	// Absent or type="" defaults to GitHub Issues — backward-compatible for
	// all existing hives.
	WorkSource WorkSourceConfig `yaml:"work_source,omitempty" json:"work_source,omitempty"`

	// ACMM tunes the dashboard's ACMM evaluation surface — today only where
	// the "Open Issue" buttons file gap issues (GitHub, or the work source).
	// Zero value = GitHub, the historical behavior. See ACMMConfig.
	ACMM ACMMConfig `yaml:"acmm,omitempty" json:"acmm,omitempty"`

	// FleetReport controls spoke self-reporting to hivecommons/hive. The default
	// is dry-run: operators must explicitly set file_upstream=true before any
	// report leaves the hive.
	FleetReport FleetReportConfig `yaml:"fleet_report,omitempty" json:"fleet_report,omitempty"`

	// Claims configures issue claims (hivecommons/hive#8380): the visible,
	// expiring marker on an issue that says someone is already working it,
	// covering the window before a PR exists. Zero value = off; see
	// ClaimsConfig.
	Claims ClaimsConfig `yaml:"claims,omitempty" json:"claims,omitempty"`
	// QuestionAutoclose closes answered question issues (#9584). Default off.
	QuestionAutoclose QuestionAutocloseConfig `yaml:"question_autoclose,omitempty" json:"question_autoclose,omitempty"`
	// ReviewGate configures the swarm-review gate the self-merge lane
	// evaluates on the App's own PRs before merging (hivecommons/hive#12592).
	// Zero value = shadow: record what the gate would have held, merge as
	// before. See ReviewGateConfig.
	ReviewGate ReviewGateConfig `yaml:"review_gate,omitempty" json:"review_gate,omitempty"`
}

// Review gate modes (GovernorConfig.ReviewGate.Mode, hivecommons/hive#12592).
const (
	// ReviewGateModeOff does not evaluate the gate on the self-merge lane
	// (review.require_approval / review.shadow still apply).
	ReviewGateModeOff = "off"
	// ReviewGateModeShadow evaluates the gate and records
	// shadow.review_gate.would_have_held on the evidence bundle, never
	// withholding a merge. The default.
	ReviewGateModeShadow = "shadow"
	// ReviewGateModeEnforce withholds the self-merge until the swarm has
	// approved the head (aggregate approve in review-verdicts.json).
	ReviewGateModeEnforce = "enforce"
)

// ReviewGateConfig is governor.review_gate. The effective mode on the
// self-merge lane is the strictest of Mode, review.require_approval
// (enforce) and review.shadow (shadow); no knob weakens another.
type ReviewGateConfig struct {
	// Mode is off | shadow | enforce; "" means shadow.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
}

// ValidReviewGateModes are the accepted governor.review_gate.mode values.
var ValidReviewGateModes = map[string]bool{
	"":                    true,
	ReviewGateModeOff:     true,
	ReviewGateModeShadow:  true,
	ReviewGateModeEnforce: true,
}

// ModeOrDefault returns the configured mode with the shadow default applied.
func (r ReviewGateConfig) ModeOrDefault() string {
	switch r.Mode {
	case ReviewGateModeOff, ReviewGateModeEnforce:
		return r.Mode
	default:
		return ReviewGateModeShadow
	}
}

// FleetReportConfig controls upstream fleet self-reporting.
type FleetReportConfig struct {
	// FileUpstream is the legacy explicit opt-in to create/comment on the
	// report repo. Kept for existing hive.yaml files; Enabled supersedes it.
	FileUpstream bool `yaml:"file_upstream,omitempty" json:"file_upstream,omitempty"`
	// Enabled is the tri-state switch. nil = default: ON for hub-managed
	// spokes and OFF for self-hosted hives. An explicit false wins over
	// file_upstream.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Repo is the owner/name the reports are filed on. Empty = hivecommons/hive.
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty"`
	// MaxPerDay caps new issues this hive may open per 24h. 0 = default 5.
	MaxPerDay int `yaml:"max_per_day,omitempty" json:"max_per_day,omitempty"`
	// IncludeRepos controls whether a self-report names the repositories
	// this hive watches (project.repos) and the owners derived from them
	// (#12338). nil = true. The report repo is public and hive.yaml does not
	// record repo visibility, so operators whose project.repos include
	// private repositories set this to false; the fields then render as "—".
	IncludeRepos *bool `yaml:"include_repos,omitempty" json:"include_repos,omitempty"`
	// IncludeOwners controls whether a self-report names the GitHub
	// login(s) of this hive's owner(s) (authorized users with the owner
	// role, the device-flow operator) so maintainers can see WHO is
	// affected. nil = true. false renders "(anonymous)" instead.
	IncludeOwners *bool `yaml:"include_owners,omitempty" json:"include_owners,omitempty"`
}

// IncludesRepos resolves include_repos: nil defaults to true.
func (f FleetReportConfig) IncludesRepos() bool {
	return f.IncludeRepos == nil || *f.IncludeRepos
}

type FleetConfig struct {
	RunWaitAmberSeconds int64 `yaml:"run_wait_amber_seconds,omitempty" json:"run_wait_amber_seconds,omitempty"`
}

// IncludesOwners resolves include_owners: nil defaults to true.
func (f FleetReportConfig) IncludesOwners() bool {
	return f.IncludeOwners == nil || *f.IncludeOwners
}

// DefaultFleetReportRepo is where self-reports land unless overridden.
const DefaultFleetReportRepo = "hivecommons/hive"

const defaultFleetReportMaxPerDay = 5

// DryRun is the legacy accessor: dry-run unless file_upstream is set. It
// ignores the hosted default; callers that know whether the hive is
// hub-managed should use FilesUpstream.
func (f FleetReportConfig) DryRun() bool { return !f.FilesUpstream(false) }

// FilesUpstream resolves the tri-state: explicit enabled wins, then the legacy
// file_upstream opt-in, then the hosted default.
func (f FleetReportConfig) FilesUpstream(hubManaged bool) bool {
	if f.Enabled != nil {
		return *f.Enabled
	}
	return f.FileUpstream || hubManaged
}

// EffectiveRepo returns the report repo, applying the default.
func (f FleetReportConfig) EffectiveRepo() string {
	if r := strings.TrimSpace(f.Repo); r != "" {
		return r
	}
	return DefaultFleetReportRepo
}

// EffectiveMaxPerDay returns the new-issue daily cap, applying the default.
func (f FleetReportConfig) EffectiveMaxPerDay() int {
	if f.MaxPerDay > 0 {
		return f.MaxPerDay
	}
	return defaultFleetReportMaxPerDay
}

type LabelsConfig struct {
	Exempt []string `yaml:"exempt"`
	// AutoMerge is the label a merger/owner queue action applies to a PR.
	// Configurable because the label has to live in someone else's
	// repository, where the local convention already exists: Prow-style
	// projects have used `lgtm` for exactly this decision for years, and a
	// hive that hard-codes its own name either collides with that or forces
	// every managed repo to grow a second label meaning the same thing.
	// Defaults to DefaultAutoMergeLabel. With Prow's commit binding, Hive
	// binds `lgtm` via the `prow/lgtm` status; alternatively choose a
	// distinct label such as `hive/auto-merge`.
	AutoMerge string `yaml:"automerge"`
	// Stage configures the hive-owned stage/NN-* pipeline labels
	// (hivecommons/hive#12429; docs/labels-and-control-signals.md).
	Stage StageLabelsConfig `yaml:"stage"`
	// Author names the hive-owned `author/<kind>` authorship labels
	// (hivecommons/hive#12581). The trust axis (`role/<tier>`) is not
	// renameable.
	Author AuthorLabelsConfig `yaml:"author"`
}

// AuthorLabelsConfig is governor.labels.author: the label names for the
// three authorship kinds pkg/authorkind detects. Each defaults to
// `author/<kind>`; a configured name must keep the `author/` prefix so the
// stage reconciler still owns it, and the three must differ.
type AuthorLabelsConfig struct {
	// Human is the label for a PR a person pushed themselves (default author/human).
	Human string `yaml:"human,omitempty"`
	// Clanker is the label for a PR a contributor's own hive authored under
	// that contributor's login (default author/clanker).
	Clanker string `yaml:"clanker,omitempty"`
	// Hive is the label for a PR this repo's own hive authored (default author/hive).
	Hive string `yaml:"hive,omitempty"`
}

// Names returns the effective label names (defaults filled in).
func (c AuthorLabelsConfig) Names() authorkind.Names {
	return authorkind.Names{Human: c.Human, Clanker: c.Clanker, Hive: c.Hive}.Normalize()
}

// Validate rejects names outside the author/ family, duplicates, and names
// that collide with the retired `author/<tier>` role spellings the
// reconciler removes.
func (c AuthorLabelsConfig) Validate() error {
	n := c.Names()
	seen := map[string]string{}
	for key, name := range map[string]string{"human": n.Human, "clanker": n.Clanker, "hive": n.Hive} {
		if !strings.HasPrefix(name, authorkind.Prefix) || len(name) == len(authorkind.Prefix) {
			return fmt.Errorf("governor.labels.author.%s %q must start with %q", key, name, authorkind.Prefix)
		}
		switch strings.TrimPrefix(name, authorkind.Prefix) {
		case "trusted", "contributor", "newcomer", "untrusted":
			return fmt.Errorf("governor.labels.author.%s %q is a retired role spelling; role labels live under role/", key, name)
		}
		if other, dup := seen[name]; dup {
			return fmt.Errorf("governor.labels.author.%s and .%s both use %q", other, key, name)
		}
		seen[name] = key
	}
	return nil
}

// StageLabelsConfig is governor.labels.stage: the hive-owned stage/NN-*,
// author/* and stage/stalled|regressed label families.
type StageLabelsConfig struct {
	// Enabled turns the stage reconciler on. nil = true.
	Enabled *bool `yaml:"enabled"`
	// Dwell overrides the expected time an item may sit in a stage before
	// it is marked stage/stalled, keyed by stage slug (e.g. "review-pending":
	// "1h"). Unset slugs keep the built-in table default; "0" means never.
	Dwell map[string]string `yaml:"dwell,omitempty"`
	// ContributorNudge configures the comment + relay interrupt the hive
	// sends a contributor whose PR entered stage/072-returned-to-contributor
	// (hivecommons/hive#12436).
	ContributorNudge ContributorNudgeConfig `yaml:"contributor_nudge"`
	// PresenceWindow is how recent a relay heartbeat must be for the
	// contributor to count as online (default 15m). Online contributors get
	// the fix-pr relay interrupt immediately; offline ones only the comment.
	PresenceWindow string `yaml:"presence_window,omitempty"`
	// ContributorGraceOnline is how long after the nudge an ONLINE
	// contributor (relay heartbeat within presence_window) has to push a new
	// head before the hive adopts the PR (default 2h).
	ContributorGraceOnline string `yaml:"contributor_grace_online,omitempty"`
	// ContributorGraceOffline is the same grace for an offline or external
	// author who only got the GitHub comment (default 15m): an online
	// clanker keeps its claim; an offline one gets fifteen minutes.
	ContributorGraceOffline string `yaml:"contributor_grace_offline,omitempty"`
	// Adoption configures stage/074-adopted-by-hive: the hive supersedes a
	// contributor PR that is still red after the grace with an App PR that
	// preserves the author's commits, and the scanner finishes it.
	Adoption StageAdoptionConfig `yaml:"adoption"`
	// Stalled configures the stage/stalled health marker.
	Stalled StageStalledConfig `yaml:"stalled"`
	// DCO configures the hive-owned `dco` / `no-dco` labels and the
	// "missing sign-off returns the PR to its author" rule
	// (hivecommons/hive#12542).
	DCO StageDCOConfig `yaml:"dco"`
	// Engine configures the central stage engine (ADR-0025,
	// hivecommons/hive#12999) every stage write goes through.
	Engine StageEngineConfig `yaml:"engine"`
}

// StageEngineConfig is governor.labels.stage.engine (ADR-0025).
type StageEngineConfig struct {
	// Mode is shadow or enforce (default enforce). enforce: the engine's
	// derivation is written. shadow: the engine derives and counts but
	// writes nothing, so every stage label stays as found (a pause). Read
	// on every sweep, so a change applies on the next pass without a
	// restart. The pre-PR-F value off (legacy pass-through) is rejected at
	// load: the legacy derivation path it selected no longer exists.
	Mode string `yaml:"mode,omitempty"`
}

// DefaultStageEngineMode is what an unset governor.labels.stage.engine.mode
// yields: enforce, since the ADR-0025 rollout gates held on hive's own spoke
// (48 h shadow with zero mismatches, then 48 h clean enforce). Set the key to
// shadow to pause stage writes.
const DefaultStageEngineMode = stageengine.ModeEnforce

// EffectiveMode parses Mode. Unset yields DefaultStageEngineMode; an
// unparsable value yields shadow, so a typo never silently enforces.
func (c StageEngineConfig) EffectiveMode() stageengine.Mode {
	if strings.TrimSpace(c.Mode) == "" {
		return DefaultStageEngineMode
	}
	m, err := stageengine.ParseMode(c.Mode)
	if err != nil {
		return stageengine.ModeShadow
	}
	return m
}

// Validate rejects a mode outside shadow|enforce.
func (c StageEngineConfig) Validate() error {
	if _, err := stageengine.ParseMode(c.Mode); err != nil {
		return fmt.Errorf("governor.labels.stage.engine.mode: %w", err)
	}
	return nil
}

// StageDCOConfig is governor.labels.stage.dco.
type StageDCOConfig struct {
	// Required says, per repo, whether a missing Signed-off-by sends the PR
	// to stage/071 (human) / 072 (clanker), 070 when self-authored, at once. Keys are "owner/repo", a
	// bare repo name, or "*"; values true/false override detection. A repo
	// not listed is required only when the hive DETECTS a DCO gate on it: a
	// `dco`/`DCO` context in the branch's required checks or reported on the
	// PR head (the DCO App or Prow's plugin). The labels themselves are
	// written on every managed repo regardless; only the stage routing and
	// the nudge ask are gated.
	Required map[string]bool `yaml:"required,omitempty"`
}

// RequiredFor resolves the per-repo override for display ("owner/repo"):
// exact key, then bare repo, then "*". ok is false when no key matches.
func (c StageDCOConfig) RequiredFor(display string) (required, ok bool) {
	if len(c.Required) == 0 {
		return false, false
	}
	display = strings.ToLower(strings.TrimSpace(display))
	bare := display
	if i := strings.LastIndex(display, "/"); i >= 0 {
		bare = display[i+1:]
	}
	for _, key := range []string{display, bare, "*"} {
		for k, v := range c.Required {
			if strings.ToLower(strings.TrimSpace(k)) == key {
				return v, true
			}
		}
	}
	return false, false
}

// StageStalledConfig is governor.labels.stage.stalled.
type StageStalledConfig struct {
	// RequireQuiet (nil = true) measures the stage dwell from the last
	// outside activity (head push, comment, review, CI change, label change
	// by anyone but the hive) so stage/stalled means "nobody is working
	// this". false measures from entering the stage. Either way the marker
	// is never applied while the item waits on the hive (checks running,
	// reviewer verdict pending) — hivecommons/hive#12534.
	RequireQuiet *bool `yaml:"require_quiet"`
}

// IsRequireQuiet reports whether stalled requires a quiet dwell (default on).
func (c StageStalledConfig) IsRequireQuiet() bool { return c.RequireQuiet == nil || *c.RequireQuiet }

// StageAdoptionConfig is governor.labels.stage.adoption.
type StageAdoptionConfig struct {
	// Enabled turns adoption on. nil = true. Off, PRs stay in 072 and only
	// stall.
	Enabled *bool `yaml:"enabled"`
	// AfterOffline is how long the author's relay must have been offline
	// (no heartbeat within presence_window) after the 072 nudge before the
	// hive adopts a clanker PR (default 15m). Applies to authors with a
	// contributor profile on this hive.
	AfterOffline string `yaml:"after_offline,omitempty"`
	// AfterUnregistered is the wait after the 072 nudge, with no new head,
	// before the hive adopts a clanker PR whose author has NO contributor
	// profile on this hive (another hive posted it; default 15m — no
	// presence reads as offline).
	AfterUnregistered string `yaml:"after_unregistered,omitempty"`
	// OfflineGrace retries a failed adoption replay once the author's
	// clanker remains offline for this duration (default 30m).
	OfflineGrace string `yaml:"offline_grace,omitempty"`
	// InPlace lets the hive adopt by pushing to the PR's own branch when
	// GitHub allows it (same-repo head or maintainer_can_modify) instead of
	// opening a superseding App PR. nil = true.
	InPlace *bool `yaml:"in_place,omitempty"`
	// StrictAttribution makes in-place adoption fail closed on changed heads:
	// only commits GitHub links to the App bot keep ownership (the stage store
	// remembers those heads so a re-observed SHA is not re-fetched, but it is
	// not an independent evidence source). nil = true; first-deploy/old-session
	// safety is handled by the stage store until any App-linked head has been
	// recorded for the repo.
	StrictAttribution *bool `yaml:"strict_attribution,omitempty"`
	// AutoMerge lets the contributor lane merge an in-place-adopted PR under
	// the hive's own authority once the hive-authored gates pass (CI green
	// on the adopted head, reviewer verdict, DCO, diff fences, soak), without
	// requiring the original author's trust tier. nil = true. Off, an
	// adopted PR by an untrusted contributor waits for a human /lgtm.
	AutoMerge *bool `yaml:"auto_merge,omitempty"`
	// MaintainerPolicy says what adoption does when the PR's author holds
	// admin or maintain on the repository (hivecommons/hive#14524):
	// AdoptionMaintainerPolicyAdopt (default) ignores the role — a
	// clanker PR whose hive disconnected is adopted like any other;
	// AdoptionMaintainerPolicyAsk never adopts on its own but parks the
	// question with the owner (an Adopt action in the activity monitor);
	// AdoptionMaintainerPolicyNever is the pre-#14524 rule: the hive never
	// takes a maintainer's PR over and releases any adoption of one.
	MaintainerPolicy string `yaml:"maintainer_policy,omitempty"`
	// CloseSuperseded says when a supersede adoption closes a clanker- or
	// hive-authored original once the hive's copy carries its commits
	// (hivecommons/hive#15526 sat open as "Adopted → #15574 · superseded"
	// because the write-back gate wanted a repo opt-in label the
	// contributor_adopt kind requires on items the hive did not author):
	// AdoptionCloseSupersededOnAdopt (default) closes it in the adopting
	// pass by policy — an agent's PR the hive re-authored is one logical PR,
	// so no opt-in label is asked for; AdoptionCloseSupersededOnMerge keeps
	// it open until the copy merges. Human-authored originals are not
	// governed here: they close in the adopting pass only when they carry
	// a writeback opt-in label (writeback.kinds.contributor_adopt), else
	// when the copy merges.
	CloseSuperseded string `yaml:"close_superseded,omitempty"`
	// Supersede tunes supersede-mode adoption (the hive-authored copy from
	// hive/adopt-<n>).
	Supersede StageAdoptionSupersedeConfig `yaml:"supersede,omitempty"`
}

// StageAdoptionSupersedeConfig tunes the hive-authored adoption copy.
type StageAdoptionSupersedeConfig struct {
	CloseFirst *bool `yaml:"close_first,omitempty"`
}

func (c StageAdoptionSupersedeConfig) CloseFirstEnabled() bool {
	return c.CloseFirst == nil || *c.CloseFirst
}

const (
	AdoptionMaintainerPolicyAdopt   = "adopt"
	AdoptionMaintainerPolicyAsk     = "ask"
	AdoptionMaintainerPolicyNever   = "never"
	DefaultAdoptionMaintainerPolicy = AdoptionMaintainerPolicyAdopt
)

var AdoptionMaintainerPolicies = []string{AdoptionMaintainerPolicyAdopt, AdoptionMaintainerPolicyAsk, AdoptionMaintainerPolicyNever}

func (c StageAdoptionConfig) EffectiveMaintainerPolicy() string {
	v := strings.ToLower(strings.TrimSpace(c.MaintainerPolicy))
	for _, p := range AdoptionMaintainerPolicies {
		if v == p {
			return p
		}
	}
	return DefaultAdoptionMaintainerPolicy
}

// IsEnabled reports whether adoption runs (default on).
func (c StageAdoptionConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// AutoMergeEnabled reports whether an in-place-adopted head may merge under
// the hive's own authority (default on).
func (c StageAdoptionConfig) AutoMergeEnabled() bool { return c.AutoMerge == nil || *c.AutoMerge }

// InPlaceEnabled reports whether in-place adoption is allowed (default on).
func (c StageAdoptionConfig) InPlaceEnabled() bool { return c.InPlace == nil || *c.InPlace }

// StrictAttributionEnabled reports whether changed in-place adoption heads
// require hive-controlled evidence (default on).
func (c StageAdoptionConfig) StrictAttributionEnabled() bool {
	return c.StrictAttribution == nil || *c.StrictAttribution
}

// Defaults for the offline-adoption waits (stage 074, hivecommons/hive#12436).
const (
	DefaultAdoptAfterOffline      = 15 * time.Minute
	DefaultAdoptAfterUnregistered = 15 * time.Minute
	DefaultAdoptOfflineGrace      = 30 * time.Minute
)

// EffectiveOfflineGrace parses OfflineGrace (default 30m).
func (c StageAdoptionConfig) EffectiveOfflineGrace() time.Duration {
	return parseDurationOr(c.OfflineGrace, DefaultAdoptOfflineGrace)
}

// EffectiveAfterOffline parses AfterOffline (default 15m).
func (c StageAdoptionConfig) EffectiveAfterOffline() time.Duration {
	return parseDurationOr(c.AfterOffline, DefaultAdoptAfterOffline)
}

// EffectiveAfterUnregistered parses AfterUnregistered (default 15m).
func (c StageAdoptionConfig) EffectiveAfterUnregistered() time.Duration {
	return parseDurationOr(c.AfterUnregistered, DefaultAdoptAfterUnregistered)
}

// RerouteConfig is governor.reroute.
type RerouteConfig struct {
	// PickupTimeout bounds how long rerouted work can wait before falling
	// back to Adopt now. Go duration syntax; default 30m.
	PickupTimeout string `yaml:"pickup_timeout,omitempty" json:"pickup_timeout,omitempty"`
}

const DefaultReroutePickupTimeout = 30 * time.Minute

// EffectivePickupTimeout parses PickupTimeout (default 30m).
func (c RerouteConfig) EffectivePickupTimeout() time.Duration {
	return parseDurationOr(c.PickupTimeout, DefaultReroutePickupTimeout)
}

// Default graces before a red contributor PR is adopted (stage 074).
const (
	DefaultContributorGraceOnline  = 2 * time.Hour
	DefaultContributorGraceOffline = 15 * time.Minute
)

// EffectiveContributorGraceOnline parses ContributorGraceOnline (default 2h).
func (s StageLabelsConfig) EffectiveContributorGraceOnline() time.Duration {
	return parseDurationOr(s.ContributorGraceOnline, DefaultContributorGraceOnline)
}

// EffectiveContributorGraceOffline parses ContributorGraceOffline (default 15m).
func (s StageLabelsConfig) EffectiveContributorGraceOffline() time.Duration {
	return parseDurationOr(s.ContributorGraceOffline, DefaultContributorGraceOffline)
}

// ContributorNudgeConfig is governor.labels.stage.contributor_nudge.
type ContributorNudgeConfig struct {
	// Enabled turns the nudge on. nil = true.
	Enabled *bool `yaml:"enabled"`
	// RenudgeInterval is how long after the first nudge the hive nudges once
	// more (the documented cap is 2 nudges per head). Default: the stage's
	// dwell (24h), i.e. the re-nudge coincides with stage/stalled.
	RenudgeInterval string `yaml:"renudge_interval,omitempty"`
}

// IsEnabled reports whether the contributor nudge runs (default on).
func (c ContributorNudgeConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// DefaultPresenceWindow is governor.labels.stage.presence_window's default.
const DefaultPresenceWindow = 15 * time.Minute

// EffectiveRenudgeInterval parses RenudgeInterval; unset/invalid returns 0
// (the caller falls back to the stage dwell).
func (c ContributorNudgeConfig) EffectiveRenudgeInterval() time.Duration {
	return parseDurationOr(c.RenudgeInterval, 0)
}

// EffectivePresenceWindow parses PresenceWindow with the 15m default.
func (s StageLabelsConfig) EffectivePresenceWindow() time.Duration {
	return parseDurationOr(s.PresenceWindow, DefaultPresenceWindow)
}

// parseDurationOr parses a config duration string; empty, unparsable or
// non-positive values return def.
func parseDurationOr(raw string, def time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// IsEnabled reports whether the stage reconciler runs (default on).
func (s StageLabelsConfig) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// stageSlugs starts with the shared vocabulary so validation does not depend
// on another package registering the table as an import side effect.
var stageSlugs []string

// RegisterStageSlugs overrides the known stage slugs.
func RegisterStageSlugs(slugs []string) { stageSlugs = append([]string(nil), slugs...) }

// KnownStageSlugs returns a copy of the registered stage slugs.
func KnownStageSlugs() []string { return append([]string(nil), stageSlugs...) }

// KnownStageSlug reports whether slug is a registered stage slug. When no
// table has been registered every slug is accepted.
func KnownStageSlug(slug string) bool {
	if len(stageSlugs) == 0 {
		return true
	}
	for _, s := range stageSlugs {
		if s == slug {
			return true
		}
	}
	return false
}

// DwellOverrides parses Dwell into durations, skipping unparsable entries.
func (s StageLabelsConfig) DwellOverrides() map[string]time.Duration {
	if len(s.Dwell) == 0 {
		return nil
	}
	out := make(map[string]time.Duration, len(s.Dwell))
	for slug, raw := range s.Dwell {
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil || d < 0 {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(slug))] = d
	}
	return out
}

type SensingConfig struct {
	GHRatePatterns     []string `yaml:"gh_rate_patterns"`
	CLIExcludePatterns []string `yaml:"cli_exclude_patterns"`
	LoginPatterns      []string `yaml:"login_patterns"`
	TTLSeconds         int      `yaml:"ttl_seconds"`
	PullbackSeconds    int      `yaml:"pullback_seconds"`
}

// defaultLoginPatterns is the built-in login-detector pattern set (#3959):
// every entry matches a CLI's own login CHROME, never ordinary English, so an
// agent is not paused for merely reading or discussing an auth error. It is a
// named var (not an inline literal in applyDefaults) so persistence can
// recognize "this list IS the default" and skip writing it — see
// redactedForPersist, which is what keeps the default set from being
// materialized into saved configs and pinned there forever (#4041).
var defaultLoginPatterns = []string{
	// claude: exact prompt strings from Claude Code's login screens.
	"Please run /login",
	"Not logged in",
	// gh / copilot / gemini: the commands their CLIs tell the user to run.
	"gh auth login",
	"claude login",
	// "copilot auth login" is the Copilot CLI's full logged-out instruction.
	// The bare 2-word fragment "copilot auth" false-positived: it also matched
	// `copilot auth status` (an auth CHECK) and incidental doc/comment mentions
	// (e.g. bin/copilot-models.mjs), pausing logged-IN agents — a live quality
	// agent flapped on `(?i)copilot auth` for days (restart_count 83). Tightening
	// to the full command matches the specificity of its `gh auth login` /
	// `gemini auth login` siblings and still catches genuine Copilot logouts.
	"copilot auth login",
	"gemini auth login",
	// bob: its API-key entry prompts.
	"Enter Bob-Shell API Key",
	"Paste your API key here",
}

// legacyDefaultLoginPatterns is the pre-#3959 default login-detector list,
// frozen verbatim (values AND order) as it shipped from the day the field
// existed until da9f6ff2. It exists only for migration: every hive that saved
// its config in that window has this exact list materialized as explicit
// values (Save() marshals defaults along with everything else), which
// permanently defeated the #3959 defaults fix because defaults only apply to
// an empty list (#4041). Never extend or reorder it — a byte-identical match
// is the evidence that the list carries no operator intent.
var legacyDefaultLoginPatterns = []string{
	"please log in",
	"authentication required",
	"not logged in",
	"login required",
	"session expired",
	"token expired",
	"unauthorized.*401",
	"gh auth login",
	"claude login",
	"copilot auth",
}

// IsLegacyDefaultLoginPatterns reports whether list is byte-identical (same
// entries, same order) to the pre-#3959 default login-pattern set. Exported
// because cmd/hive's legacy state-overrides migration replays a persisted
// sensing_login list over the loaded config, which is the same materialized-
// defaults hazard applyDefaults migrates in the config file itself (#4041).
func IsLegacyDefaultLoginPatterns(list []string) bool {
	return stringSlicesEqual(list, legacyDefaultLoginPatterns)
}

// stringSlicesEqual is an exact element-wise comparison (no normalization —
// migration must only ever fire on a byte-identical match).
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// HealthConfig holds the governor's health-related settings.
//
// It used to carry HealthcheckInterval and RestartCooldown as well. Both were
// DEAD (#7251): parsed, defaulted, validated, persisted and rendered in
// Settings, but never read by any runtime loop. An operator lowering
// "Healthcheck Interval" to 60s expecting faster detection got no change at
// all, while the cadence that actually governs liveness sweeps lived further
// down the same tab under "Agent Watchdog". Restart cooldowns likewise come
// from hard-coded constants in pkg/agent (tokenRestartCooldownSec,
// tlsErrorRestartCooldownSec) and the watchdog's own backoff ladder.
//
// They are removed rather than wired up: the watchdog (RFC #4665) is the real
// liveness system, and a second, half-implemented one beside it is worse than
// none. DeprecatedHealthcheckInterval survives only to carry an operator's
// explicit value forward into the watchdog probe interval — see
// migrateDeprecatedHealthSettings.
type HealthConfig struct {
	// DeprecatedHealthcheckInterval is read ONLY to migrate an explicitly
	// configured value into governor.watchdog.probe_interval_s. It is cleared
	// once migrated, so it does not survive a config re-save.
	//
	// Deprecated: set governor.watchdog.probe_interval_s instead.
	DeprecatedHealthcheckInterval int `yaml:"healthcheck_interval,omitempty"`
	// ModelLock stops the governor from automatically changing an agent's
	// model under budget pressure. This is a BUDGET behaviour, not a health
	// one, and the Settings UI now presents it on the Budget tab beside the
	// downgrade it disables; the config key is unchanged.
	ModelLock bool `yaml:"model_lock"`
}

type BudgetConfig struct {
	TotalTokens int64                       `yaml:"total_tokens"`
	USD         float64                     `yaml:"usd,omitempty" json:"usd,omitempty"`
	PeriodDays  int                         `yaml:"period_days"`
	CriticalPct int                         `yaml:"critical_pct"`
	Coins       map[string]CoinBudgetConfig `yaml:"coins,omitempty"`
}

type CoinBudgetConfig struct {
	TokensPerCoin float64 `yaml:"tokens_per_coin,omitempty" json:"tokens_per_coin,omitempty"`
	USDPerCoin    float64 `yaml:"usd_per_coin,omitempty" json:"usd_per_coin,omitempty"`
	Label         string  `yaml:"label,omitempty" json:"label,omitempty"`
	Budget        float64 `yaml:"budget,omitempty" json:"budget,omitempty"`
}

const (
	// DefaultBobTokensPerCoin and DefaultBobUSDPerCoin are operator-supplied
	// fallback values for Bob-account coin math. They are defaults only, not a
	// public authoritative Bob rate; operators should confirm their own account
	// conversion with their Bob team.
	DefaultBobTokensPerCoin = 500000
	DefaultBobUSDPerCoin    = 0.50
	DefaultBobCoinLabel     = "BC"

	BobTokensPerCoinEnvVar = "HIVE_BOB_TOKENS_PER_COIN"
	BobUSDPerCoinEnvVar    = "HIVE_BOB_USD_PER_COIN"
	BobCoinBudgetEnvVar    = "HIVE_BOB_COIN_BUDGET"
	BobUSDBudgetEnvVar     = "HIVE_BOB_USD_BUDGET"
)

func (b BudgetConfig) CoinConfig(backend string) (CoinBudgetConfig, bool) {
	if b.Coins == nil {
		return CoinBudgetConfig{}, false
	}
	key := strings.ToLower(strings.TrimSpace(backend))
	cc, ok := b.Coins[key]
	if !ok {
		for name, candidate := range b.Coins {
			if strings.EqualFold(strings.TrimSpace(name), key) {
				cc, ok = candidate, true
				break
			}
		}
		if !ok {
			return CoinBudgetConfig{}, false
		}
	}
	return cc, cc.TokensPerCoin > 0
}

func (c CoinBudgetConfig) TokensToCoins(tokens int64) float64 {
	if c.TokensPerCoin <= 0 || tokens <= 0 {
		return 0
	}
	return float64(tokens) / c.TokensPerCoin
}

func (c CoinBudgetConfig) CoinsToUSD(coins float64) float64 {
	if c.USDPerCoin <= 0 || coins <= 0 {
		return 0
	}
	return coins * c.USDPerCoin
}

func (c CoinBudgetConfig) USDToCoins(usd float64) float64 {
	if c.USDPerCoin <= 0 || usd <= 0 {
		return 0
	}
	return usd / c.USDPerCoin
}

func (c CoinBudgetConfig) LabelOrDefault() string {
	if strings.TrimSpace(c.Label) != "" {
		return strings.TrimSpace(c.Label)
	}
	return DefaultBobCoinLabel
}

// MinUsableBudgetTokens is the sanity floor for governor.budget.total_tokens.
// A fleet audit (#5508) found LIVE spokes configured with limits of 5, 50 and
// 1000 tokens — unit mistakes where the operator meant 5M/50M. A single model
// call consumes more than any of those, so the budget gate closes on the first
// kick and the spoke sits permanently budget-exhausted, doing no work, while
// the fleet view shows only a generic quiet hive.
//
// The floor is a plausibility test, not a policy minimum: it separates "a
// small budget" from "a value that cannot fund one call". Zero is exempt
// everywhere — total_tokens: 0 is the documented way to disable budget
// tracking entirely and must keep working.
const MinUsableBudgetTokens int64 = 100_000

// BudgetLimitBelowFloor reports whether a configured token limit is a likely
// unit mistake: positive, but too small to fund a single model call. Zero
// (budget tracking disabled) and negative values are NOT below-floor — zero is
// a legitimate mode and negatives are rejected by the normal range checks.
func BudgetLimitBelowFloor(totalTokens int64) bool {
	return totalTokens > 0 && totalTokens < MinUsableBudgetTokens
}

// SuggestBudgetUnitMistake renders the "did you mean" hint for a below-floor
// limit — "50" almost always means "50M". Returns "" when the value is not
// below the floor, so callers can use it as both the test and the message.
func SuggestBudgetUnitMistake(totalTokens int64) string {
	if !BudgetLimitBelowFloor(totalTokens) {
		return ""
	}
	return fmt.Sprintf("limit of %d tokens is below any usable budget (floor %d) — did you mean %dM?",
		totalTokens, MinUsableBudgetTokens, totalTokens)
}

type ModeConfig struct {
	Threshold int                `yaml:"threshold"`
	Cadences  map[string]Cadence `yaml:"cadences"`
}

// UnmarshalYAML implements custom unmarshaling for ModeConfig.
// The YAML format has threshold and agent cadences as sibling keys:
//
//	idle:
//	  threshold: 0
//	  scanner: 15m
//	  ci-maintainer: 15m
//
// This method separates "threshold" into the Threshold field and collects
// all other keys into the Cadences map.
func (m *ModeConfig) UnmarshalYAML(value *yaml.Node) error {
	var raw map[string]Cadence
	if err := value.Decode(&raw); err != nil {
		return err
	}

	m.Cadences = make(map[string]Cadence)

	const thresholdKey = "threshold"
	if v, ok := raw[thresholdKey]; ok {
		var t int
		if _, err := fmt.Sscanf(v.Interval(), "%d", &t); err != nil {
			return fmt.Errorf("invalid threshold value %q: %w", v.Interval(), err)
		}
		m.Threshold = t
	}

	for k, v := range raw {
		if k == thresholdKey {
			continue
		}
		m.Cadences[k] = v
	}

	return nil
}

// MarshalYAML produces the flat format expected by UnmarshalYAML:
// threshold as a sibling key alongside agent cadences.
func (m ModeConfig) MarshalYAML() (interface{}, error) {
	out := make(map[string]interface{})
	out["threshold"] = m.Threshold
	for k, v := range m.Cadences {
		out[k] = v
	}
	return out, nil
}

// Values for governor.labels.stage.adoption.close_superseded.
const (
	AdoptionCloseSupersededOnAdopt = "on_adopt"
	AdoptionCloseSupersededOnMerge = "on_merge"
	DefaultAdoptionCloseSuperseded = AdoptionCloseSupersededOnAdopt
)

// AdoptionCloseSupersededValues lists the accepted close_superseded values.
var AdoptionCloseSupersededValues = []string{AdoptionCloseSupersededOnAdopt, AdoptionCloseSupersededOnMerge}

// EffectiveCloseSuperseded normalises CloseSuperseded; empty or unknown
// reads as the default (on_adopt).
func (c StageAdoptionConfig) EffectiveCloseSuperseded() string {
	v := strings.ToLower(strings.TrimSpace(c.CloseSuperseded))
	for _, p := range AdoptionCloseSupersededValues {
		if v == p {
			return p
		}
	}
	return DefaultAdoptionCloseSuperseded
}
