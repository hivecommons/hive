package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type GovernorConfig struct {
	Modes                      map[string]ModeConfig `yaml:"modes"`
	EvalIntervalS              int                   `yaml:"eval_interval_s"`
	EvalIntervalMaxS           int                   `yaml:"eval_interval_max_s,omitempty"`
	EvalIntervalWebhookS       int                   `yaml:"eval_interval_webhook_s,omitempty"`
	ConserveIntervalMultiplier int                   `yaml:"conserve_interval_multiplier,omitempty"`
	OptionalSweepEveryNCycles  int                   `yaml:"optional_sweep_every_n_cycles,omitempty"`
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
	// KickLimits caps the issue and PR lists in every kick prompt
	// (hivecommons/hive#7368). Absent = defaults (100 issues, 50 PRs).
	KickLimits KickLimitsConfig `yaml:"kick_limits,omitempty" json:"kick_limits,omitempty"`
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
}

// FleetReportConfig controls upstream fleet self-reporting.
type FleetReportConfig struct {
	// FileUpstream is the explicit opt-in to create/comment on hivecommons/hive.
	// The zero value is dry-run, which still surfaces the would-file reports on
	// the dashboard for operator review.
	FileUpstream bool `yaml:"file_upstream,omitempty" json:"file_upstream,omitempty"`
}

type FleetConfig struct {
	RunWaitAmberSeconds int64 `yaml:"run_wait_amber_seconds,omitempty" json:"run_wait_amber_seconds,omitempty"`
}

func (f FleetReportConfig) DryRun() bool { return !f.FileUpstream }

type LabelsConfig struct {
	Exempt []string `yaml:"exempt"`
	// AutoMerge is the label a merger/owner queue action applies to a PR.
	// Configurable because the label has to live in someone else's
	// repository, where the local convention already exists: Prow-style
	// projects have used `lgtm` for exactly this decision for years, and a
	// hive that hard-codes its own name either collides with that or forces
	// every managed repo to grow a second label meaning the same thing.
	// Defaults to DefaultAutoMergeLabel.
	AutoMerge string `yaml:"automerge"`
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
