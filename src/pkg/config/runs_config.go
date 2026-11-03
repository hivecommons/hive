package config

import (
	"fmt"
	"strings"
	"time"
)

// Long-running run defaults (hivecommons/hive#8303). Every value here is a
// documented default that the `runs:` block can override; the feature itself
// is OFF until runs.spektacular.enabled is set.
const (
	// DefaultMaxStageRetries is how many generations one hub-executed run
	// stage may spend without reaching `document_status: final` before the hub
	// executor stops retrying and raises a decision escalation. A generation is
	// spent when its one agent launch fails or exits with the document still
	// not final. The count includes the first generation: at the default of 2
	// a stage runs once, is retried once, and escalates when the second
	// generation is spent, so no third generation is ever minted (#9143).
	DefaultMaxStageRetries = 2
	// DefaultRunsMaxWorktrees caps live per-stage git worktrees on one hive.
	DefaultRunsMaxWorktrees = 8
	// DefaultRunsEngine is the planning engine used when runs.engine is unset.
	DefaultRunsEngine = "spektacular"
	// DefaultSpektacularBinary is the executable the runner shells out to when
	// runs.spektacular.binary is unset; it is resolved through PATH.
	DefaultSpektacularBinary = "spektacular"
	// DefaultSpektacularPollS is how often, in seconds, the runner calls the
	// status verb for each active stage lease.
	DefaultSpektacularPollS = 30
	// DefaultSpektacularHubExecutorBackend is the fallback agent CLI used when no
	// hub default backend can be inferred.
	DefaultSpektacularHubExecutorBackend = "copilot"
	// DefaultSpektacularHubExecutorIdentity owns stages claimed by the hub.
	DefaultSpektacularHubExecutorIdentity = "hive-spek"
	// DefaultSpektacularHubExecutorTimeoutSeconds bounds one agent turn.
	DefaultSpektacularHubExecutorTimeoutSeconds = 1800
	// DefaultSpektacularHubExecutorMaxConcurrent bounds simultaneous hub-authored
	// Spektacular stages.
	DefaultSpektacularHubExecutorMaxConcurrent = 1
	// DefaultSpektacularRecheckInterval is the default continuous convergence
	// cadence for campaigns that opt in without a per-campaign override.
	DefaultSpektacularRecheckInterval = 168 * time.Hour
	// DefaultSpektacularMaxDeltaTasks caps how many changed/new plan tasks a
	// recheck imports into the planner in one generation.
	DefaultSpektacularMaxDeltaTasks = 50
	// DefaultSpektacularDiscoveryTimeoutS bounds each outward discovery pass.
	DefaultSpektacularDiscoveryTimeoutS = 30
	// DefaultSpektacularDiscoveryMaxItems caps one configured discovery source.
	DefaultSpektacularDiscoveryMaxItems = 20
	// DefaultSpektacularDiscoveryMaxTotalItems caps all discovery evidence for a revision.
	DefaultSpektacularDiscoveryMaxTotalItems = 100

	// DefaultRunsWaitTimeoutSeconds is how long a run checkpoint may wait for
	// owner approval before escalation. It also floors how far a held plan
	// lease is extended so the hold does not expire out from under the owner.
	DefaultRunsWaitTimeoutSeconds = 3600
	// DefaultRunsWaitSeverity is the escalation severity for a checkpoint that
	// waited past DefaultRunsWaitTimeoutSeconds.
	DefaultRunsWaitSeverity = "decision"
	// RunImplementCheckpointMinACMM is the ACMM level a hive must reach before
	// runs.checkpoints.implement: false is honoured. Below it the implement
	// checkpoint keeps blocking regardless of config, so a young hive cannot
	// disable the last gate in front of code changes.
	RunImplementCheckpointMinACMM = 5

	// DefaultTriageMinBodyChars is the minimum useful issue body size before
	// the run triage pass asks the reporter for more detail.
	DefaultTriageMinBodyChars = 80
	// DefaultClarityGateThreshold is the clarity score (0-100) below which
	// the clarity gate parks an issue and asks its author for acceptance
	// criteria instead of letting an agent claim it.
	DefaultClarityGateThreshold = 60
)

// RunsConfig tunes long-running runs (spec, plan, implement stages on a lease).
// The zero value keeps every existing behaviour: leases still advance only
// through the API, and nothing shells out to Spektacular.
type RunsConfig struct {
	// Engine selects the planning engine (ADR-0021). Empty means
	// DefaultRunsEngine; an unregistered name fails validation.
	Engine string `yaml:"engine,omitempty" json:"engine,omitempty"`
	// Checkpoints controls which run boundaries wait for owner approval.
	Checkpoints RunCheckpointConfig `yaml:"checkpoints,omitempty" json:"checkpoints,omitempty"`
	// WaitTimeoutSeconds is how long a checkpoint may wait before escalation.
	// Zero or negative means DefaultRunsWaitTimeoutSeconds.
	WaitTimeoutSeconds int `yaml:"wait_timeout_seconds,omitempty" json:"wait_timeout_seconds,omitempty"`
	// WaitSeverity is the escalation severity for timed-out checkpoints.
	// Empty means DefaultRunsWaitSeverity.
	WaitSeverity string `yaml:"wait_severity,omitempty" json:"wait_severity,omitempty"`
	// MaxStageRetries bounds the generations one hub-executed stage may spend
	// before the hub executor escalates. Zero or negative means
	// DefaultMaxStageRetries.
	MaxStageRetries int `yaml:"max_stage_retries,omitempty" json:"max_stage_retries,omitempty"`
	// MaxWorktrees caps live per-stage git worktrees. Zero or negative means
	// DefaultRunsMaxWorktrees.
	MaxWorktrees int `yaml:"max_worktrees,omitempty" json:"max_worktrees,omitempty"`
	// Triage configures the optional fix/spec/clarify decision made before
	// direct-fix dispatch. Default off.
	Triage TriageConfig `yaml:"triage,omitempty" json:"triage,omitempty"`
	// Spektacular configures the stage runner that polls Spektacular's status
	// verb and advances the lease on final.
	Spektacular SpektacularConfig `yaml:"spektacular,omitempty" json:"spektacular,omitempty"`
	// External holds the external-execution engine bindings: the report-only
	// Flue binding pilot and the OMP workbench host (#8361). Default off.
	External ExternalRunsConfig `yaml:"external,omitempty" json:"external,omitempty"`
}

// RunCheckpointConfig preserves the rollout invariant: an absent key keeps the
// historical hold-gated behavior, while explicit false lets a runner advance.
type RunCheckpointConfig struct {
	Spec      *bool `yaml:"spec,omitempty" json:"spec,omitempty"`
	Plan      *bool `yaml:"plan,omitempty" json:"plan,omitempty"`
	Implement *bool `yaml:"implement,omitempty" json:"implement,omitempty"`
}

// CheckpointBlocks reports whether the named stage boundary waits for owner
// approval. An unknown stage blocks: the safe answer for a name this build
// does not recognise is to keep the gate.
func (r RunsConfig) CheckpointBlocks(stage string) bool {
	switch strings.TrimSpace(strings.ToLower(stage)) {
	case "spec":
		return runCheckpointBoolDefaultTrue(r.Checkpoints.Spec)
	case "plan":
		return runCheckpointBoolDefaultTrue(r.Checkpoints.Plan)
	case "implement":
		return runCheckpointBoolDefaultTrue(r.Checkpoints.Implement)
	default:
		return true
	}
}

// runCheckpointBoolDefaultTrue treats an unset checkpoint key as enabled.
func runCheckpointBoolDefaultTrue(v *bool) bool {
	return v == nil || *v
}

// EffectiveWaitTimeoutSeconds returns the configured checkpoint wait budget or
// the default.
func (r RunsConfig) EffectiveWaitTimeoutSeconds() int {
	if r.WaitTimeoutSeconds > 0 {
		return r.WaitTimeoutSeconds
	}
	return DefaultRunsWaitTimeoutSeconds
}

// EffectiveWaitSeverity returns the configured escalation severity or the
// default.
func (r RunsConfig) EffectiveWaitSeverity() string {
	if s := strings.TrimSpace(strings.ToLower(r.WaitSeverity)); s != "" {
		return s
	}
	return DefaultRunsWaitSeverity
}

// TriageConfig controls the optional incoming-issue run triage pass.
type TriageConfig struct {
	// Enabled turns the pass on. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// SpecLabels force a spec run when present. Empty uses the documented defaults.
	SpecLabels []string `yaml:"spec_labels,omitempty" json:"spec_labels,omitempty"`
	// FixLabels force direct-fix dispatch when present. Empty uses the documented defaults.
	FixLabels []string `yaml:"fix_labels,omitempty" json:"fix_labels,omitempty"`
	// MinBodyChars asks for clarification when the trimmed body is shorter.
	// Zero or negative means DefaultTriageMinBodyChars.
	MinBodyChars int `yaml:"min_body_chars,omitempty" json:"min_body_chars,omitempty"`
	// ClarifyComment controls posting the one-shot clarification comment. Nil defaults true.
	ClarifyComment *bool `yaml:"clarify_comment,omitempty" json:"clarify_comment,omitempty"`
	// ClarityGate parks a human-filed issue whose clarity score is below the
	// threshold, asking its author concise questions before any agent claims
	// it. Independent of Enabled. Default off.
	ClarityGate ClarityGateConfig `yaml:"clarity_gate,omitempty" json:"clarity_gate,omitempty"`
	// PriorArt parks a human-filed feature / RFC / epic issue when something
	// that looks like it already exists (a merged PR, a closed issue, a doc,
	// a setting, a knowledge bead, a code symbol) scores at or above the
	// threshold, and asks the author to confirm the gap before any agent
	// claims it. Independent of Enabled. Default on (hivecommons/hive#13575).
	PriorArt PriorArtGateConfig `yaml:"prior_art,omitempty" json:"prior_art,omitempty"`
}

// PriorArtGateConfig is runs.triage.prior_art (hivecommons/hive#13575).
type PriorArtGateConfig struct {
	// Enabled turns the gate on. Nil defaults true; set false to turn it off.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Threshold is the confidence (0-100) the best finding needs before the
	// gate parks the issue. Zero or negative means
	// DefaultPriorArtGateThreshold; values above 100 are read as 100.
	Threshold int `yaml:"threshold,omitempty" json:"threshold,omitempty"`
	// MaxFindings caps the findings listed in the park comment. Zero or
	// negative means DefaultPriorArtGateMaxFindings.
	MaxFindings int `yaml:"max_findings,omitempty" json:"max_findings,omitempty"`
	// Kinds are the issue kinds the gate judges, matched against labels
	// (case-insensitive, `kind/feature` and `feature` both match "feature")
	// and against a leading `[RFC]` / `RFC:` / `Epic:` title tag. Empty
	// means DefaultPriorArtGateKinds. Bug reports are never gated.
	Kinds []string `yaml:"kinds,omitempty" json:"kinds,omitempty"`
	// MaxParksPerCycle caps how many issues one enumeration cycle may park,
	// so turning the gate on against a backlog never floods a repo with
	// comments. Zero or negative means DefaultPriorArtGateMaxParksPerCycle.
	MaxParksPerCycle int `yaml:"max_parks_per_cycle,omitempty" json:"max_parks_per_cycle,omitempty"`
	// BotCloseTTL is how long a parked issue filed by a clanker (another
	// hive, a contributor CLI, an external bot — `author/clanker` or a bot
	// login) waits for a reply before the resume sweep closes it as
	// `already-exists`. Human-filed issues are never auto-closed. Zero
	// means DefaultPriorArtGateBotCloseTTL; negative disables auto-close.
	BotCloseTTL time.Duration `yaml:"bot_close_ttl,omitempty" json:"bot_close_ttl,omitempty"`
}

const (
	// DefaultPriorArtGateThreshold is deliberately high: the gate parks an
	// issue on its author, so a false positive costs a person a round trip.
	// 70 means the best finding covers roughly three quarters of the
	// issue's weighted key terms, including at least one from the title.
	DefaultPriorArtGateThreshold = 70
	// DefaultPriorArtGateMaxFindings bounds the park comment.
	DefaultPriorArtGateMaxFindings = 5
	// DefaultPriorArtGateMaxParksPerCycle bounds one cycle's parks.
	DefaultPriorArtGateMaxParksPerCycle = 5
	// DefaultPriorArtGateBotCloseTTL is how long a clanker-filed park waits
	// for a reply before it is closed as already existing.
	DefaultPriorArtGateBotCloseTTL = 72 * time.Hour
)

// DefaultPriorArtGateKinds are the issue kinds the gate judges when
// runs.triage.prior_art.kinds is empty.
var DefaultPriorArtGateKinds = []string{"feature", "rfc", "epic", "enhancement"}

// IsEnabled is Enabled with the nil-means-true default applied.
func (g PriorArtGateConfig) IsEnabled() bool {
	return g.Enabled == nil || *g.Enabled
}

// EffectiveThreshold is Threshold with the default and the 100 cap applied.
func (g PriorArtGateConfig) EffectiveThreshold() int {
	switch {
	case g.Threshold <= 0:
		return DefaultPriorArtGateThreshold
	case g.Threshold > 100:
		return 100
	default:
		return g.Threshold
	}
}

// EffectiveMaxFindings is MaxFindings with the default applied.
func (g PriorArtGateConfig) EffectiveMaxFindings() int {
	if g.MaxFindings <= 0 {
		return DefaultPriorArtGateMaxFindings
	}
	return g.MaxFindings
}

// EffectiveKinds is Kinds with the default applied, lower-cased and trimmed.
func (g PriorArtGateConfig) EffectiveKinds() []string {
	src := g.Kinds
	if len(src) == 0 {
		src = DefaultPriorArtGateKinds
	}
	out := make([]string, 0, len(src))
	for _, k := range src {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// EffectiveMaxParksPerCycle is MaxParksPerCycle with the default applied.
func (g PriorArtGateConfig) EffectiveMaxParksPerCycle() int {
	if g.MaxParksPerCycle <= 0 {
		return DefaultPriorArtGateMaxParksPerCycle
	}
	return g.MaxParksPerCycle
}

// EffectiveBotCloseTTL is BotCloseTTL with the default applied; zero
// (auto-close off) when BotCloseTTL is negative.
func (g PriorArtGateConfig) EffectiveBotCloseTTL() time.Duration {
	switch {
	case g.BotCloseTTL == 0:
		return DefaultPriorArtGateBotCloseTTL
	case g.BotCloseTTL < 0:
		return 0
	default:
		return g.BotCloseTTL
	}
}

// ClarityGateConfig is runs.triage.clarity_gate (hivecommons/hive#13577).
type ClarityGateConfig struct {
	// Enabled turns the gate on. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Threshold is the minimum clarity score (0-100) an issue needs to stay
	// in the agent queue. Zero or negative means DefaultClarityGateThreshold;
	// values above 100 are read as 100.
	Threshold int `yaml:"threshold,omitempty" json:"threshold,omitempty"`
}

// EffectiveThreshold is Threshold with the default and the 100 cap applied.
func (g ClarityGateConfig) EffectiveThreshold() int {
	switch {
	case g.Threshold <= 0:
		return DefaultClarityGateThreshold
	case g.Threshold > 100:
		return 100
	default:
		return g.Threshold
	}
}

// SpektacularConfig is the opt-in for the Spektacular stage runner.
type SpektacularConfig struct {
	// Enabled turns the poll loop on. Default false: with it off, no
	// Spektacular process is ever started by Hive.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Binary is the spektacular executable path. Empty means
	// DefaultSpektacularBinary resolved through PATH.
	Binary string `yaml:"binary,omitempty" json:"binary,omitempty"`
	// PollIntervalS is the seconds between status calls per active stage.
	// Zero or negative means DefaultSpektacularPollS.
	PollIntervalS int `yaml:"poll_interval_s,omitempty" json:"poll_interval_s,omitempty"`
	// HubExecutor optionally lets the hub claim and execute unclaimed spec/plan
	// stages itself. The zero value is enabled when Spektacular is enabled.
	HubExecutor SpektacularHubExecutorConfig `yaml:"hub_executor,omitempty" json:"hub_executor,omitempty"`
	// Interview controls Spektacular clarification/interview steps. Empty and
	// "human" make the hub pause for dashboard answers; "auto" preserves the
	// previous headless self-answering behavior.
	Interview string `yaml:"interview,omitempty" json:"interview,omitempty"`
	// Recheck configures continuous convergence revisions. Default off.
	Recheck SpektacularRecheckConfig `yaml:"recheck,omitempty" json:"recheck,omitempty"`
}

// SpektacularHubExecutorConfig configures the hub-resident Spektacular executor.
type SpektacularHubExecutorConfig struct {
	Enabled        *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Backend        string `yaml:"backend,omitempty" json:"backend,omitempty"`
	Model          string `yaml:"model,omitempty" json:"model,omitempty"`
	TimeoutSeconds int    `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	Identity       string `yaml:"identity,omitempty" json:"identity,omitempty"`
	MaxConcurrent  int    `yaml:"max_concurrent,omitempty" json:"max_concurrent,omitempty"`
}

// SpektacularRecheckConfig controls cadence-driven Spek revision campaigns.
type SpektacularRecheckConfig struct {
	// Sources is an opt-in list of exact discovery documents; empty disables
	// exact-document discovery I/O.
	Sources []SpektacularDiscoverySource `yaml:"sources,omitempty" json:"sources,omitempty"`
	// DiscoveryProxy is the relay egress proxy. Direct network fallback is forbidden.
	DiscoveryProxy string `yaml:"discovery_proxy,omitempty" json:"discovery_proxy,omitempty"`
	// EgressAllowlist contains exact HTTPS hostnames permitted for exact-document sources.
	EgressAllowlist []string `yaml:"egress_allowlist,omitempty" json:"egress_allowlist,omitempty"`
	// Enabled turns scheduler-owned recheck cadence on. Manual recheck requires
	// this too unless the request supplies force=true.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// DefaultInterval is the cadence for opted-in campaigns without their own
	// interval. Zero means DefaultSpektacularRecheckInterval.
	DefaultInterval time.Duration `yaml:"default_interval,omitempty" json:"default_interval,omitempty"`
	// MaxDeltaTasks caps imported changed/new tasks. Zero or negative means
	// DefaultSpektacularMaxDeltaTasks.
	MaxDeltaTasks int `yaml:"max_delta_tasks,omitempty" json:"max_delta_tasks,omitempty"`
	// Discovery configures opt-in outward evidence gathered before recheck spec.
	Discovery SpektacularRecheckDiscoveryConfig `yaml:"discovery,omitempty" json:"discovery,omitempty"`
}

// SpektacularRecheckDiscoveryConfig is the bounded, declared-source-only
// outward discovery step for recheck spec revisions.
type SpektacularRecheckDiscoveryConfig struct {
	Enabled       bool                                `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Sources       []SpektacularRecheckDiscoverySource `yaml:"sources,omitempty" json:"sources,omitempty"`
	Timeout       time.Duration                       `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	MaxTotalItems int                                 `yaml:"max_total_items,omitempty" json:"max_total_items,omitempty"`
}

// SpektacularRecheckDiscoverySource declares one operator-approved source.
type SpektacularRecheckDiscoverySource struct {
	Kind            string `yaml:"kind,omitempty" json:"kind,omitempty"`
	Name            string `yaml:"name,omitempty" json:"name,omitempty"`
	URLOrRepo       string `yaml:"url_or_repo,omitempty" json:"url_or_repo,omitempty"`
	AllowPrerelease bool   `yaml:"allow_prerelease,omitempty" json:"allow_prerelease,omitempty"`
	MaxItems        int    `yaml:"max_items,omitempty" json:"max_items,omitempty"`
}

// RegisteredRunsEngines reports the planning engine names that are registered.
// pkg/planengine installs it, since that package imports pkg/config and the
// reverse import would be a cycle. While it is nil only DefaultRunsEngine is
// known, so validation still fails closed.
var RegisteredRunsEngines func() []string

// EngineOrDefault returns the normalised engine name or the default.
func (r RunsConfig) EngineOrDefault() string {
	if e := strings.TrimSpace(strings.ToLower(r.Engine)); e != "" {
		return e
	}
	return DefaultRunsEngine
}

// ValidateEngine rejects a runs.engine naming an unregistered engine. There is
// no fallback to another engine.
func (r RunsConfig) ValidateEngine() error {
	name := r.EngineOrDefault()
	known := []string{DefaultRunsEngine}
	if RegisteredRunsEngines != nil {
		known = RegisteredRunsEngines()
	}
	for _, k := range known {
		if k == name {
			return nil
		}
	}
	return fmt.Errorf("runs.engine: unknown engine %q (registered: %s)", r.Engine, strings.Join(known, ", "))
}

// MaxStageRetriesOrDefault returns the configured retry budget or the default.
func (r RunsConfig) MaxStageRetriesOrDefault() int {
	if r.MaxStageRetries <= 0 {
		return DefaultMaxStageRetries
	}
	return r.MaxStageRetries
}

func (t TriageConfig) EffectiveSpecLabels() []string {
	if len(t.SpecLabels) > 0 {
		return t.SpecLabels
	}
	return []string{"kind/feature", "Epic", "architecture discussion"}
}

func (t TriageConfig) EffectiveFixLabels() []string {
	if len(t.FixLabels) > 0 {
		return t.FixLabels
	}
	return []string{"kind/bug", "good first issue"}
}

func (t TriageConfig) EffectiveMinBodyChars() int {
	if t.MinBodyChars > 0 {
		return t.MinBodyChars
	}
	return DefaultTriageMinBodyChars
}

func (t TriageConfig) ShouldClarifyComment() bool {
	return t.ClarifyComment == nil || *t.ClarifyComment
}

func (r RunsConfig) EffectiveMaxWorktrees() int {
	if r.MaxWorktrees > 0 {
		return r.MaxWorktrees
	}
	return DefaultRunsMaxWorktrees
}

// BinaryOrDefault returns the configured executable or the default name.
func (s SpektacularConfig) BinaryOrDefault() string {
	if b := strings.TrimSpace(s.Binary); b != "" {
		return b
	}
	return DefaultSpektacularBinary
}

// PollInterval returns the configured poll cadence or the default.
func (s SpektacularConfig) PollInterval() time.Duration {
	if s.PollIntervalS <= 0 {
		return time.Duration(DefaultSpektacularPollS) * time.Second
	}
	return time.Duration(s.PollIntervalS) * time.Second
}

func (s SpektacularConfig) HubExecutorEnabled() bool {
	if !s.Enabled {
		return false
	}
	return s.HubExecutor.Enabled == nil || *s.HubExecutor.Enabled
}

// Validate rejects interview values other than empty, "auto" or "human" so a
// typo is reported instead of silently selecting human.
func (s SpektacularConfig) Validate() error {
	switch strings.TrimSpace(strings.ToLower(s.Interview)) {
	case "", "auto", "human":
		return nil
	default:
		return fmt.Errorf("runs.spektacular.interview: invalid value %q (must be auto or human)", s.Interview)
	}
}

func (s SpektacularConfig) InterviewMode() string {
	switch strings.TrimSpace(strings.ToLower(s.Interview)) {
	case "auto":
		return "auto"
	default:
		return "human"
	}
}

func (h SpektacularHubExecutorConfig) BackendOrDefault(fallback string) string {
	if b := strings.TrimSpace(h.Backend); b != "" {
		return b
	}
	if b := strings.TrimSpace(fallback); b != "" {
		return b
	}
	return DefaultSpektacularHubExecutorBackend
}

func (h SpektacularHubExecutorConfig) IdentityOrDefault() string {
	if id := strings.TrimSpace(h.Identity); id != "" {
		return id
	}
	return DefaultSpektacularHubExecutorIdentity
}

func (h SpektacularHubExecutorConfig) Timeout() time.Duration {
	if h.TimeoutSeconds > 0 {
		return time.Duration(h.TimeoutSeconds) * time.Second
	}
	return time.Duration(DefaultSpektacularHubExecutorTimeoutSeconds) * time.Second
}

func (h SpektacularHubExecutorConfig) MaxConcurrentOrDefault() int {
	if h.MaxConcurrent > 0 {
		return h.MaxConcurrent
	}
	return DefaultSpektacularHubExecutorMaxConcurrent
}

// DefaultRecheckInterval returns the configured continuous convergence cadence.
func (s SpektacularConfig) DefaultRecheckInterval() time.Duration {
	if s.Recheck.DefaultInterval <= 0 {
		return DefaultSpektacularRecheckInterval
	}
	return s.Recheck.DefaultInterval
}

// MaxDeltaTasks returns the configured cap for new/changed recheck tasks.
func (s SpektacularConfig) MaxDeltaTasks() int {
	if s.Recheck.MaxDeltaTasks <= 0 {
		return DefaultSpektacularMaxDeltaTasks
	}
	return s.Recheck.MaxDeltaTasks
}

func (s SpektacularConfig) DiscoveryTimeout() time.Duration {
	if s.Recheck.Discovery.Timeout <= 0 {
		return time.Duration(DefaultSpektacularDiscoveryTimeoutS) * time.Second
	}
	return s.Recheck.Discovery.Timeout
}

func (s SpektacularConfig) DiscoveryMaxTotalItems() int {
	if s.Recheck.Discovery.MaxTotalItems <= 0 {
		return DefaultSpektacularDiscoveryMaxTotalItems
	}
	return s.Recheck.Discovery.MaxTotalItems
}

func (s SpektacularRecheckDiscoverySource) EffectiveMaxItems() int {
	if s.MaxItems <= 0 {
		return DefaultSpektacularDiscoveryMaxItems
	}
	return s.MaxItems
}
