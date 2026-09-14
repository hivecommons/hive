// Governor configuration: GovernorConfig, explain/caveman modes, provider
// budget probing, provider/account rotation, advisory settings, and
// mode-threshold defaults and scaling.
package config

import (
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

// Agent explain modes (AgentConfig.ExplainMode / HIVE_EXPLAIN_MODE).
//
// Agents are told to act rather than narrate — see the "Output Rules — Terse
// Mode" block in every agent policy and the EXECUTE, DO NOT NARRATE suffix the
// agent manager appends on inference backends. That instruction exists to stop
// weak models answering a kick with a plan for someone else to run instead of
// running it, which is a real, observed failure and must be preserved. The cost
// of preserving it is that an operator debugging an agent has no visibility
// into WHY it chose what it chose (#3887, split from #3878).
//
// Explain mode buys that visibility back without relaxing the rule: the agent
// still acts first, and the explanation rides ALONGSIDE the tool calls on
// EXPLAIN-prefixed lines rather than replacing them.
const (
	// ExplainModeOff disables explanation. As an explicit per-agent value it
	// also overrides a hive-wide default, which "" (unset) does not.
	ExplainModeOff = "off"
	// ExplainModeBrief asks for one short EXPLAIN line per tool call.
	ExplainModeBrief = "brief"
	// ExplainModeFull adds an end-of-turn EXPLAIN block covering the goal as
	// understood, alternatives rejected, and what would change the decision.
	ExplainModeFull = "full"
)

// ValidExplainModes are the accepted explain_mode values. "" means "inherit the
// hive-wide default"; it is valid on an agent but is not a mode in itself.
var ValidExplainModes = map[string]bool{
	"":               true,
	ExplainModeOff:   true,
	ExplainModeBrief: true,
	ExplainModeFull:  true,
}

// ExplainModeEnvVar is the deployment environment variable holding the
// hive-wide default for agents that leave explain_mode unset. It lets an
// operator debugging a misbehaving fleet turn explanation on for every agent at
// once without editing (and later having to unedit) each agent's config — the
// same shape as HIVE_TMUX_HISTORY_LIMIT and HIVE_TMUX_PANE_WIDTH.
//
// It remains supported, but it is no longer the ONLY place the default lives:
// see GovernorConfig.ExplainMode. Hive also injects the RESOLVED mode into
// every agent process under this same name.
const ExplainModeEnvVar = "HIVE_EXPLAIN_MODE"

// ExplainLinePrefix marks a line the agent emitted as debugging explanation
// rather than as ordinary output. It is the whole reason explanation can be a
// SEPARATE stream without new capture infrastructure: agent logs are tmux pane
// scrapes, so there is no second channel to write to, but a stable, greppable
// prefix lets the full-log endpoint (and `grep`) split explanation from work
// after the fact. Chosen to be ASCII, unlikely to collide with tool output, and
// stable — the filter and the prompt must agree on it forever.
const ExplainLinePrefix = "EXPLAIN:"

// ValidateExplainMode reports whether v is an accepted explain_mode value.
func ValidateExplainMode(v string) bool { return ValidExplainModes[v] }

// Explain-mode default sources reported by ExplainModeDefaultSource. Both are
// safe to show in the dashboard and to log.
const (
	// ExplainModeSourceConfig means governor.explain_mode set the default.
	ExplainModeSourceConfig = "config"
	// ExplainModeSourceEnv means the ExplainModeEnvVar environment variable
	// set the default because governor.explain_mode is unset.
	ExplainModeSourceEnv = "env:" + ExplainModeEnvVar
)

// ResolveExplainModeDefault returns the hive-wide default explain mode applied
// to agents that leave explain_mode unset.
//
// Governor config wins over the environment, with the env var kept as the
// fallback for hives that already set it — the same precedence, and for the
// same reason, as the backup encryption key (see BackupConfig.ResolveKey).
// HIVE_EXPLAIN_MODE is set on the DEPLOYMENT, and a hosted spoke owner has no
// deployment-env access, so a knob that lives only in the environment is
// unreachable for exactly the operators who go looking for it and find nothing
// (#4712). Putting it in governor config puts it in the dashboard's Settings →
// Governor form, next to the other hive-wide agent defaults.
//
// An unrecognized value in either place resolves to off, matching
// resolveExplainMode's rule that a typo degrades to today's behaviour rather
// than to a mode nobody asked for.
func (g GovernorConfig) ResolveExplainModeDefault() string {
	mode, _ := g.resolveExplainModeDefault()
	return mode
}

// ExplainModeDefaultSource reports WHERE the hive-wide default came from —
// ExplainModeSourceConfig, ExplainModeSourceEnv, or "" when neither sets one
// (so agents fall through to off). The dashboard shows this so an operator can
// tell a default they set from one the deployment set for them, which is the
// question that produced #4712 in the first place.
func (g GovernorConfig) ExplainModeDefaultSource() string {
	_, source := g.resolveExplainModeDefault()
	return source
}

func (g GovernorConfig) resolveExplainModeDefault() (mode, source string) {
	if v := strings.TrimSpace(g.ExplainMode); v != "" {
		return normalizeExplainMode(v), ExplainModeSourceConfig
	}
	if v := strings.TrimSpace(os.Getenv(ExplainModeEnvVar)); v != "" {
		return normalizeExplainMode(v), ExplainModeSourceEnv
	}
	return ExplainModeOff, ""
}

// normalizeExplainMode maps any value to one the kick path can act on,
// collapsing typos and unknown modes to off.
func normalizeExplainMode(v string) string {
	switch v {
	case ExplainModeBrief, ExplainModeFull:
		return v
	default:
		return ExplainModeOff
	}
}

// ValidCavemanModes are the accepted caveman_mode values. "" means "caveman
// disabled"; it is valid on an agent but is not a mode in itself.
var ValidCavemanModes = map[string]bool{
	"":       true,
	"lite":   true,
	"full":   true,
	"ultra":  true,
	"wenyan": true,
}

// ValidateCavemanMode reports whether v is an accepted caveman_mode value.
func ValidateCavemanMode(v string) bool { return ValidCavemanModes[v] }

// ReasoningEffortsByBackend lists the reasoning-effort values each CLI
// backend accepts, for the backends that expose an effort control at all:
// codex takes `-c model_reasoning_effort="<v>"` and agy takes `--effort <v>`.
// Backends absent here have no effort flag; a configured effort is ignored
// for them rather than breaking their launch command.
var ReasoningEffortsByBackend = map[string][]string{
	"codex": {"minimal", "low", "medium", "high", "xhigh"},
	"agy":   {"low", "medium", "high"},
}

// ValidateReasoningEffort reports whether effort is settable for backend:
// empty is always valid (the backend's own default), otherwise the backend
// must have an effort control and the value must be one it accepts. Rejecting
// at set time keeps the failure at the dashboard, not hours later on the kick
// path — the same rationale as ValidateBackend.
func ValidateReasoningEffort(backend, effort string) error {
	if effort == "" {
		return nil
	}
	accepted, ok := ReasoningEffortsByBackend[backend]
	if !ok {
		return fmt.Errorf("backend %s has no reasoning-effort control", backend)
	}
	for _, v := range accepted {
		if v == effort {
			return nil
		}
	}
	return fmt.Errorf("invalid reasoning effort %q for backend %s (accepted: %s)",
		effort, backend, strings.Join(accepted, ", "))
}

type GovernorConfig struct {
	Modes         map[string]ModeConfig `yaml:"modes"`
	EvalIntervalS int                   `yaml:"eval_interval_s"`
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
	// Watchdog configures the per-agent self-healing reconciler (RFC #4665).
	// Zero value = enabled with the RFC defaults; see pkg/config/watchdog.go
	// for why defaults resolve lazily instead of via applyDefaults.
	Watchdog WatchdogConfig      `yaml:"watchdog,omitempty"`
	Health   HealthConfig        `yaml:"health"`
	Budget   BudgetConfig        `yaml:"budget"`
	Logging  LoggingConfig       `yaml:"logging"`
	LiteLLM  LiteLLMConfig       `yaml:"litellm"`
	VLLM     InferenceAuthConfig `yaml:"vllm"`
	LLMD     InferenceAuthConfig `yaml:"llm-d"`
	// Bob holds the IBM bobshell CLI backend's API-key location. Required for
	// agents with backend "bob": bobshell's browser SSO flow cannot complete in
	// a headless pod.
	Bob BobConfig `yaml:"bob"`
	// Backup holds the location of the self-service backup encryption key.
	// It records a PATH only, never the key value: hosted spoke owners have no
	// deployment-env access, so the key has to be settable through this
	// (governor) config surface rather than only through HIVE_BACKUP_KEY.
	Backup     BackupConfig     `yaml:"backup,omitempty" json:"backup,omitempty"`
	Trajectory TrajectoryConfig `yaml:"trajectory"`
	Replan     ReplanConfig     `yaml:"replan"`
	// Gateways is the list of named model gateways (OpenAI-compatible endpoints
	// like OpenRouter, a LiteLLM proxy, vLLM, or llm-d). An agent routes through
	// a gateway by naming it as its backend. When empty, a single implicit
	// gateway named "litellm" is synthesized from the legacy LiteLLM block above
	// so existing hives keep working with zero config change. See ResolveGateway.
	Gateways []GatewayConfig `yaml:"gateways"`
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

	// Rotation configures automatic provider failover when a provider's
	// subscription or credit is exhausted. See RFC #3958.
	Rotation RotationConfig `yaml:"rotation,omitempty" json:"rotation,omitempty"`

	// ProviderBudget tunes the response to a PROVIDER spending-limit refusal
	// (#4294) — the gateway declining to spend more money, as distinct from the
	// hive's own token Budget above. See ProviderBudgetConfig.
	ProviderBudget ProviderBudgetConfig `yaml:"provider_budget,omitempty" json:"provider_budget,omitempty"`
}

// ProviderBudgetConfig tunes how long the hive keeps agent kicks suspended
// after the inference provider refuses on a spending limit (#4294).
//
// There is exactly one knob because there is exactly one judgement call: how
// much wasted spend to accept in exchange for noticing sooner that the
// provider's window has reset. The hive cannot observe the reset passively —
// the suppression that saves the money also withholds the inference calls that
// would reveal the money is available again — so it periodically lets one
// cycle's kicks through as a probe.
type ProviderBudgetConfig struct {
	// ProbeIntervalS is how long a spend rebuff suppresses kicks before one
	// cycle is allowed through to test whether the provider is serving again.
	// Default 1800 (30 min).
	//
	// Shorter probes recover faster after a reset but burn a run each time on a
	// provider that is still clipped; longer probes waste less and notice later.
	// 30 minutes costs at most ~48 rebuffed runs across a day-long clip — set
	// against the field report's entire cadence firing all day. The probe is
	// how hive stays agnostic to WHEN the provider resets: reset schedules are
	// not knowable (the field report's window rolled at the key's creation time
	// of day, not at midnight — #4294), so hive never predicts one; it just
	// retries cheaply until the provider serves again.
	ProbeIntervalS int `yaml:"probe_interval_s,omitempty" json:"probe_interval_s,omitempty"`
}

// defaultProviderBudgetProbeIntervalS is the spend-rebuff probe interval when
// unset: 30 minutes.
const defaultProviderBudgetProbeIntervalS = 1800

// EffectiveProbeInterval returns the spend-rebuff probe interval, applying the
// default when unset. A negative or zero value means "unset" rather than
// "never probe": never probing is the deadlock this knob exists to prevent, so
// it is not a reachable configuration.
func (p ProviderBudgetConfig) EffectiveProbeInterval() time.Duration {
	if p.ProbeIntervalS > 0 {
		return time.Duration(p.ProbeIntervalS) * time.Second
	}
	return defaultProviderBudgetProbeIntervalS * time.Second
}

// RotationConfig configures automatic provider failover (RFC #3958). When a
// provider's subscription or credit is exhausted, agents are rotated onto a
// different provider at the same capability tier.
type RotationConfig struct {
	// Enabled turns the rotation loop on. Default false — opt-in.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// ThresholdPct is the usage percentage at which a subscription provider
	// is considered exhausted (0–100). Default 85.
	ThresholdPct int `yaml:"threshold_pct,omitempty" json:"threshold_pct,omitempty"`
	// Providers maps provider name → its class config.
	Providers map[string]ProviderRotationConfig `yaml:"providers,omitempty" json:"providers,omitempty"`
	// HighVolumeCadenceS: agents with a cadence at or below this value (seconds)
	// are high-volume and must NEVER rotate onto subscription providers.
	// Default 1800 (30 min). Protects weekly subscription budgets.
	HighVolumeCadenceS int `yaml:"high_volume_cadence_s,omitempty" json:"high_volume_cadence_s,omitempty"`
	// AgentTiers maps agent name → capability tier ("T1","T2","T3").
	// Rotation stays within tiers; drops only when nothing has headroom.
	AgentTiers map[string]string `yaml:"agents,omitempty" json:"agents,omitempty"`
}

// ProviderRotationConfig describes one provider in the rotation set.
type ProviderRotationConfig struct {
	// Class is "subscription" or "metered".
	Class string `yaml:"class" json:"class"`
	// Backends lists which hive backend names front this provider
	// (e.g. ["claude","pi"] for anthropic; ["litellm"] when litellm fronts deepseek).
	Backends []string `yaml:"backends,omitempty" json:"backends,omitempty"`
	// MonthlyAllowance states the plan's included request count per calendar
	// month for providers metered that way (github/Copilot premium requests,
	// #6980). Their documented usage endpoints report only consumption, never
	// the entitlement, so a remaining percentage exists only when the operator
	// states the allowance here. 0 (unset) leaves the reading an explicit
	// unknown rather than a guessed one.
	MonthlyAllowance int `yaml:"monthly_allowance,omitempty" json:"monthly_allowance,omitempty"`
}

// defaultRotationThresholdPct is the exhaustion threshold when unset.
const defaultRotationThresholdPct = 85

// defaultHighVolumeCadenceS is the high-volume cadence cutoff when unset.
const defaultHighVolumeCadenceS = 1800

// EffectiveThreshold returns the exhaustion threshold pct, defaulting to 85.
func (r RotationConfig) EffectiveThreshold() int {
	if r.ThresholdPct > 0 {
		return r.ThresholdPct
	}
	return defaultRotationThresholdPct
}

// EffectiveHighVolumeCadenceS returns the high-volume cadence cutoff in
// seconds, defaulting to 1800 (30 minutes).
func (r RotationConfig) EffectiveHighVolumeCadenceS() int {
	if r.HighVolumeCadenceS > 0 {
		return r.HighVolumeCadenceS
	}
	return defaultHighVolumeCadenceS
}

// AdvisoryConfig controls the advisory digest's size and the lifecycle of the
// beads behind it.
//
// Two operator complaints motivate it: advisory beads never closed once filed,
// so healed findings accumulated in the digest forever; and every finding was
// rendered, so a repo owner opening the digest met dozens of items with no
// indication of which few mattered.
type AdvisoryConfig struct {
	// MaxFindings caps how many findings the digest renders, chosen by severity
	// then recency across all agents.
	//
	// 0 means UNSET and resolves to defaultAdvisoryMaxFindings on load — a
	// plain int cannot tell "the operator asked for zero" from "the key is
	// absent", and defaulting is the behavior an untouched hive must get. The
	// way to lift the cap is therefore ShowAll, not max_findings: 0, which
	// would silently revert to 10 on the next config reload.
	MaxFindings int `yaml:"max_findings" json:"max_findings"`
	// ShowAll bypasses MaxFindings entirely — the opt-in for owners who want
	// the full list, and the ONLY supported way to render an uncapped digest.
	ShowAll bool `yaml:"show_all" json:"show_all"`
	// StalenessDays is how long an open advisory bead may go without being
	// re-reported before the hive auto-closes it. Default
	// defaultAdvisoryStalenessDays.
	StalenessDays int `yaml:"staleness_days" json:"staleness_days"`
	// PRAutoClose retires an advisory finding when a merged PR's title is
	// close enough to the finding's title. *bool so absent is distinct from an
	// explicit false; default true.
	PRAutoClose *bool `yaml:"pr_autoclose,omitempty" json:"pr_autoclose,omitempty"`
	// UpdateIntervalS throttles how often, in seconds, the digest comment on
	// the pinned advisory issue is refreshed (#4820). 0 (or absent) means
	// UNSET and keeps today's behavior: a post attempt every governor eval
	// cycle (~60s at the default cadence). Operators raise it to reduce
	// GitHub API writes and notification churn on watched repos.
	//
	// The raw value is stored as written so hive.yaml round-trips byte-for-
	// byte; consumers resolve it through UpdateInterval, which clamps into
	// [MinAdvisoryUpdateIntervalS, MaxAdvisoryUpdateIntervalS]. The max
	// exists for the hub's wedged-digest alarm: its staleness threshold
	// (90 min) must stay comfortably above every healthy configured cadence
	// so a user-lengthened interval never false-alarms as a wedge — pinned by
	// TestAdvisoryStaleThresholdCoversMaxUpdateInterval in pkg/hub.
	UpdateIntervalS int `yaml:"update_interval_s,omitempty" json:"update_interval_s,omitempty"`
	// Target selects where the digest comment lives: AdvisoryTargetGitHub
	// (the pinned advisory issue on the primary repo — the default, and the
	// only behavior before this key existed) or AdvisoryTargetLinear (one
	// comment on the Linear issue named by LinearIssue, rewritten each cycle
	// with the same body the GitHub comment would get). Empty means UNSET
	// and resolves to GitHub through ResolvedTarget; an unknown value fails
	// closed at post time rather than silently falling back to GitHub.
	Target string `yaml:"target,omitempty" json:"target,omitempty"`
	// LinearIssue is the Linear issue identifier (e.g. "ONB-123") that hosts
	// the digest when Target is AdvisoryTargetLinear. Required for that
	// target: an empty value logs an error naming this key and skips the
	// post — the digest is never redirected to GitHub without being asked.
	// Authentication reuses governor.work_source.linear.api_key.
	LinearIssue string `yaml:"linear_issue,omitempty" json:"linear_issue,omitempty"`
}

// Advisory digest targets accepted by AdvisoryConfig.Target.
const (
	AdvisoryTargetGitHub = "github"
	AdvisoryTargetLinear = "linear"
)

// ResolvedTarget returns Target with the unset default applied: an empty
// string is GitHub, because that is what every hive did before the key
// existed. Any other value is returned trimmed and lower-cased so a caller
// can reject what it does not recognize instead of guessing.
func (a AdvisoryConfig) ResolvedTarget() string {
	t := strings.ToLower(strings.TrimSpace(a.Target))
	if t == "" {
		return AdvisoryTargetGitHub
	}
	return t
}

// PRAutoCloseEnabled resolves AdvisoryConfig.PRAutoClose with its default (on).
func (a AdvisoryConfig) PRAutoCloseEnabled() bool {
	return a.PRAutoClose == nil || *a.PRAutoClose
}

// Bounds for AdvisoryConfig.UpdateIntervalS. Exported because the dashboard
// PUT validates against them and pkg/hub pins the invariant that its
// advisory-staleness threshold exceeds the maximum allowed posting cadence
// (so a healthy slow digest never reads as wedged).
const (
	// MinAdvisoryUpdateIntervalS floors a configured interval at 30s: below
	// the ~60s eval cycle the throttle is meaningless, and a typo like 3
	// would silently disable the setting.
	MinAdvisoryUpdateIntervalS = 30
	// MaxAdvisoryUpdateIntervalS caps the interval at one hour. The hub flags
	// a digest as wedged when its last successful post is older than 90
	// minutes; capping the healthy cadence at 60 minutes keeps every allowed
	// interval comfortably inside that threshold.
	MaxAdvisoryUpdateIntervalS = 3600
)

// UpdateInterval resolves UpdateIntervalS to the effective posting throttle.
// 0 means no throttle — post every eval cycle, exactly the pre-#4820 behavior
// — and a set value is clamped into [MinAdvisoryUpdateIntervalS,
// MaxAdvisoryUpdateIntervalS]. Negative values are treated as unset rather
// than clamped up, so garbage cannot silently slow a hive down.
func (a AdvisoryConfig) UpdateInterval() time.Duration {
	if a.UpdateIntervalS <= 0 {
		return 0
	}
	s := a.UpdateIntervalS
	if s < MinAdvisoryUpdateIntervalS {
		s = MinAdvisoryUpdateIntervalS
	}
	if s > MaxAdvisoryUpdateIntervalS {
		s = MaxAdvisoryUpdateIntervalS
	}
	return time.Duration(s) * time.Second
}

// Default governor mode thresholds, in queue items (actionable issues + open
// PRs). These are the per-repo BASE values: a hive watching one repo surges at
// 20 items, and EffectiveThreshold scales them for hives watching more.
//
// They live here rather than in pkg/governor because two callers need the same
// answer — the governor, which decides the mode, and the dashboard gauge, which
// tells the operator which numbers produced it. Those were independently
// duplicated constants before #3498; scaling one and not the other would have
// made the gauge disagree with the governor it describes.
const (
	DefaultThresholdSurge = 20
	DefaultThresholdBusy  = 10
	DefaultThresholdQuiet = 2
)

// Threshold scaling curves (GovernorConfig.ThresholdScaling).
const (
	// CadenceScopeAggregate keeps the historical governor behavior: one hive
	// mode is computed from the aggregate actionable issue+PR queue.
	CadenceScopeAggregate = "aggregate"
	// CadenceScopePerRepo computes a separate mode for each successfully
	// scanned repo from that repo's own actionable issue+PR queue.
	CadenceScopePerRepo = "per_repo"
)

// ValidCadenceScopes are the accepted cadence_scope values. "" means "unset",
// which resolves to aggregate.
var ValidCadenceScopes = map[string]bool{
	"":                    true,
	CadenceScopeAggregate: true,
	CadenceScopePerRepo:   true,
}

// ValidateCadenceScope reports whether v is an accepted cadence mode scope.
func ValidateCadenceScope(v string) bool { return ValidCadenceScopes[v] }

// CadenceScopeMode returns the configured cadence mode scope with its default
// applied. Unset means aggregate to preserve the historical one-mode cadence
// behavior for existing hives.
func (g GovernorConfig) CadenceScopeMode() string {
	switch g.CadenceScope {
	case CadenceScopePerRepo:
		return CadenceScopePerRepo
	default:
		return CadenceScopeAggregate
	}
}

const (
	// ThresholdScalingLinear multiplies the base threshold by the repo count.
	// This is the default, and it is exactly equivalent to comparing PER-REPO
	// queue pressure against the base thresholds — the mode ladder then means
	// the same thing on a 3-repo hive as on a 39-repo one. The issue considered
	// normalizing the queue instead (queue / repos) and rejected it because it
	// changes the number the dashboard displays; scaling the thresholds reaches
	// the same outcome while leaving the displayed queue depth alone.
	ThresholdScalingLinear = "linear"
	// ThresholdScalingSqrt multiplies by ceil(sqrt(repos)) instead — a gentler
	// curve for hives whose queue depth does not grow linearly with repo count
	// (many small, quiet repos alongside a few busy ones). It reaches SURGE
	// sooner than linear does.
	ThresholdScalingSqrt = "sqrt"
	// ThresholdScalingNone disables scaling: the base thresholds are used as
	// absolute queue depths, which is the behavior from before #3498.
	ThresholdScalingNone = "none"
)

// ThresholdSourcePack marks GovernorConfig.Modes thresholds as seeded by an
// ACMM pack rather than typed by an operator (#4037). It is written only by the
// pack-apply paths and cleared by the operator threshold-write path.
const ThresholdSourcePack = "pack"

// ThresholdsArePackSeeded reports whether the explicit thresholds in Modes were
// written by a pack apply. Anything other than ThresholdSourcePack — including
// the empty value every pre-#4037 hive has — reads as operator-owned.
func (g GovernorConfig) ThresholdsArePackSeeded() bool {
	return g.ThresholdsSource == ThresholdSourcePack
}

// ValidThresholdScalings are the accepted threshold_scaling values. "" means
// "unset", which resolves to linear.
var ValidThresholdScalings = map[string]bool{
	"":                     true,
	ThresholdScalingLinear: true,
	ThresholdScalingSqrt:   true,
	ThresholdScalingNone:   true,
}

// ValidateThresholdScaling reports whether v is an accepted scaling curve.
func ValidateThresholdScaling(v string) bool { return ValidThresholdScalings[v] }

// ThresholdScalingMode returns the configured scaling curve with its default
// applied. Unset means linear: the whole point of #3498 is that a hive which
// says nothing gets thresholds matched to its size.
//
// An unrecognized value also resolves to linear rather than silently disabling
// scaling — config load rejects bad values outright, so reaching this with one
// means the value came from a path that skipped validation, and defaulting to
// the documented behavior beats defaulting to "off" for reasons nobody can see.
func (g GovernorConfig) ThresholdScalingMode() string {
	switch g.ThresholdScaling {
	case ThresholdScalingSqrt:
		return ThresholdScalingSqrt
	case ThresholdScalingNone:
		return ThresholdScalingNone
	default:
		return ThresholdScalingLinear
	}
}

// ScaleThreshold applies a scaling curve to a base threshold for a hive
// watching repoCount repos.
//
// repoCount is clamped to at least 1, so a hive with no repos: list — or one
// that watches a single repo via primary_repo — gets the unscaled base rather
// than a threshold of zero, which would put every non-empty queue in SURGE.
func ScaleThreshold(base, repoCount int, scaling string) int {
	if base <= 0 {
		return base
	}
	if repoCount < 1 {
		repoCount = 1
	}
	switch scaling {
	case ThresholdScalingNone:
		return base
	case ThresholdScalingSqrt:
		return base * int(math.Ceil(math.Sqrt(float64(repoCount))))
	default:
		return base * repoCount
	}
}

// EffectiveThreshold resolves the queue depth at which modeName engages, for a
// hive watching repoCount repos.
//
// Resolution, in order:
//
//  1. An OPERATOR-SET explicit governor.modes.<mode>.threshold greater than
//     zero wins and is returned UNSCALED. An operator who hand-tuned a number
//     meant that number, and #3498 is explicit that hand-tuned hives see no
//     behavior change: scaling a hand-tuner's `surge: 300` on a 39-repo hive
//     would produce 11700.
//  2. A PACK-SEEDED explicit threshold (governor.thresholds_source: pack) is
//     treated as the per-repo BASE and scaled, exactly like the built-in
//     defaults (#4037). This is what makes scaling reach a pack-applied hive —
//     the normal path — while keeping each level's own tuning, since L3's
//     15/10/3 and L4-L6's 10/5/2 stay distinct bases rather than collapsing
//     onto one default.
//  3. Otherwise the base default for surge/busy/quiet, scaled by repo count.
//  4. Any other mode name has no threshold (0). computeMode only ladders over
//     surge/busy/quiet — idle is the fallthrough — so a threshold on `idle` or
//     on a custom mode has never been consulted.
//
// A threshold of exactly zero in config counts as UNSET, matching the existing
// thresholdFor behavior: mode entries frequently exist only to carry cadences,
// and a literal zero would put every non-empty queue in that mode.
//
// WHY AN ABSENT MARKER MEANS "OPERATOR-SET". Every hive that applied a pack
// before #4037 has seeded thresholds and no marker, and reading those as
// pack-seeded would multiply them by the repo count the first time the new code
// ran — a silent, potentially large mode-ladder change on upgrade. Reading them
// as operator-owned instead keeps those hives exactly as they are; re-applying
// the level stamps the marker and turns scaling on as an explicit, operator-
// initiated act. Fail-quiet on upgrade, opt-in to the new behavior.
func (g GovernorConfig) EffectiveThreshold(modeName string, repoCount int) int {
	packSeeded := g.ThresholdsArePackSeeded()
	if mode, ok := g.Modes[modeName]; ok && mode.Threshold > 0 && !packSeeded {
		return mode.Threshold
	}

	var base int
	switch modeName {
	case "surge":
		base = DefaultThresholdSurge
	case "busy":
		base = DefaultThresholdBusy
	case "quiet":
		base = DefaultThresholdQuiet
	default:
		// Unknown mode: no threshold, and a pack-seeded value on it is still
		// not a threshold the ladder consults.
		return 0
	}

	// A pack-seeded value replaces the built-in base for this mode, then scales
	// the same way. A pack that seeds only some modes leaves the rest on the
	// built-in bases, which is the behavior the packs already rely on.
	if packSeeded {
		if mode, ok := g.Modes[modeName]; ok && mode.Threshold > 0 {
			base = mode.Threshold
		}
	}

	return ScaleThreshold(base, repoCount, g.ThresholdScalingMode())
}

// AttributionTrailerEnabled reports whether the visible attribution trailer on
// hive-created PRs/issues is on for this hive. Default ON: a hive that says
// nothing gets the trailer; set `governor.attribution_trailer: false` to hide
// it. The audit-log entry for each creation is unconditional and is NOT gated
// by this.
func (g *GovernorConfig) AttributionTrailerEnabled() bool {
	return g.AttributionTrailer == nil || *g.AttributionTrailer
}

// CadenceIsOperatorOwned reports whether an operator explicitly set the
// cadence for agent in the given governor mode, which makes it immune to pack
// reconciliation — the same contract ModelIsOperatorOwned provides for models.
func (g *GovernorConfig) CadenceIsOperatorOwned(mode, agent string) bool {
	return g.CadenceOwners[mode][agent] == FieldOwnerOperator
}

// ClaimCadenceOwnership marks the (mode, agent) cadence as operator-owned so
// no subsequent pack apply reconciles it back to the pack default.
func (g *GovernorConfig) ClaimCadenceOwnership(mode, agent string) {
	if g.CadenceOwners == nil {
		g.CadenceOwners = make(map[string]map[string]string)
	}
	if g.CadenceOwners[mode] == nil {
		g.CadenceOwners[mode] = make(map[string]string)
	}
	g.CadenceOwners[mode][agent] = FieldOwnerOperator
}

// ReleaseCadenceOwnership drops the ownership marker for (mode, agent). Called
// when the cadence entry itself is removed (e.g. the agent left the roster) so
// a stale claim cannot outlive the value it protected.
func (g *GovernorConfig) ReleaseCadenceOwnership(mode, agent string) {
	owners, ok := g.CadenceOwners[mode]
	if !ok {
		return
	}
	delete(owners, agent)
	if len(owners) == 0 {
		delete(g.CadenceOwners, mode)
	}
}
