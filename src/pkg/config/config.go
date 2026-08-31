package config

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"log/slog"
	"math"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/pkg/resolve"
	"gopkg.in/yaml.v3"
)

// saveMu serializes all Config.Save() calls process-wide. Multiple goroutines
// persist config concurrently — the mutex-guarded pause callback, the async
// dashboard PersistFunc, the ACMM-level saver, HTTP mutation handlers. Each
// does yaml.Marshal(c) + write. Without serialization, two Save() calls race:
// the one that finishes writing LAST wins the file, and if it marshaled a
// staler snapshot (e.g. before a later pause committed to c.Agents), that
// pause is silently lost. Pausing 7 agents in quick succession reliably left
// only the last 1-2 on the PVC. Serializing every Save() closes the race.
var saveMu sync.Mutex

type Config struct {
	Project       ProjectConfig          `yaml:"project"`
	Policies      PoliciesConfig         `yaml:"policies"`
	Agents        map[string]AgentConfig `yaml:"agents"`
	Governor      GovernorConfig         `yaml:"governor"`
	GitHub        GitHubConfig           `yaml:"github"`
	GitLab        GitLabConfig           `yaml:"gitlab,omitempty"`
	Gitea         GiteaConfig            `yaml:"gitea,omitempty"`
	Notifications NotificationsConfig    `yaml:"notifications"`
	Dashboard     DashboardConfig        `yaml:"dashboard"`
	Data          DataConfig             `yaml:"data"`
	Knowledge     KnowledgeConfig        `yaml:"knowledge"`
	Hub           HubConfig              `yaml:"hub"`
	HiveID        string                 `yaml:"hive_id"`
	ACMMLevel     *int                   `yaml:"acmm_level,omitempty" json:"acmm_level"`
	Variables     VariablesConfig        `yaml:"variables,omitempty"`
	// OTel configures standards-based OTLP trace export. It is the preferred
	// operator-facing block; Tracing is retained as a legacy alias.
	OTel    OTelConfig `yaml:"otel,omitempty" json:"otel,omitempty"`
	Tracing OTelConfig `yaml:"tracing,omitempty"`
	// Triggers is an additive list of CEL-based declarative agent triggers.
	// Default empty → existing label/governor triggering is unchanged.
	Triggers []TriggerRule `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	// Hooks is an additive list of operator-declared state-triggered hooks
	// (RFC #4001): `on: <transition>` → `action: <vetted action>`. Default
	// empty → no hooks fire and behavior is byte-identical to before.
	//
	// This list is OPERATOR-ONLY by construction: it is config, so writing it
	// requires the same authz and carries the same layer provenance as any
	// other config write, and there is deliberately no runtime registration
	// API. Nothing agent-writable can reach it — an agent able to register
	// hooks on its own transitions would have an escalation path.
	Hooks      []HookRule       `yaml:"hooks,omitempty" json:"hooks,omitempty"`
	Mint       MintConfig       `yaml:"mint,omitempty"`
	Ioscan     IoscanConfig     `yaml:"ioscan,omitempty" json:"ioscan,omitempty"`
	Classifier ClassifierConfig `yaml:"classifier,omitempty" json:"classifier,omitempty"`
	Planning   PlanningConfig   `yaml:"planning,omitempty" json:"planning,omitempty"`
	Intent     IntentConfig     `yaml:"intent,omitempty" json:"intent,omitempty"`
	Escalation EscalationConfig `yaml:"escalation,omitempty" json:"escalation,omitempty"`
	Retro      RetroConfig      `yaml:"retro,omitempty" json:"retro,omitempty"`
	Review     ReviewConfig     `yaml:"review,omitempty" json:"review,omitempty"`
	AutoMerge  AutoMergeConfig  `yaml:"auto_merge,omitempty" json:"auto_merge,omitempty"`
	// AgentSandbox configures the phase-1 credential-free sandbox runner. It is
	// disabled by default and agents must opt in individually.
	AgentSandbox AgentSandboxConfig `yaml:"agent_sandbox,omitempty" json:"agent_sandbox,omitempty"`
	// Convergence toggles the convergence-driven admission surfaces
	// (kubestellar/hive#3845 follow-ons). Default off → zero behaviour change.
	Convergence ConvergenceConfig `yaml:"convergence,omitempty" json:"convergence,omitempty"`

	// RemovedAgents are agent names an operator deliberately deleted. It is a
	// TOMBSTONE list, and it exists because deletion had no durable record
	// anywhere: the delete handlers dropped the agent from the in-memory map
	// and re-saved the overlay, but an agent lives in THREE places — the
	// ConfigMap seed, /data/agent-configs/<name>.yaml, and the dashboard
	// overlay — and Load() UNIONS all three via MergeAgentOverrides, which
	// only ever adds. So the next config reload (fsnotify, observed ~36s after
	// the delete on a live hive) re-materialized the agent, and even after
	// that the next ApplyPack re-created it from the ACMM pack. That is the
	// reported "I deleted brainstorm and guide and they always come back".
	//
	// A tombstone is scoped to the agent NAME and persists indefinitely,
	// including across ACMM level changes. The alternative — clearing
	// tombstones on a level change so a higher pack can reintroduce the agent
	// — was rejected: an operator who deletes `guide` is expressing "I do not
	// want this agent", not "I do not want it at this level", and silently
	// resurrecting it during an unrelated level bump is the same class of
	// silent revert as the bug itself. Re-adding the agent explicitly (the
	// Governor grid's add, the agent CRUD create, or an import) clears the
	// tombstone, which is the one unambiguous signal that the operator changed
	// their mind. A genuinely NEW pack agent — one never deleted here — is
	// unaffected and is still added on a level increase.
	RemovedAgents []string `yaml:"removed_agents,omitempty" json:"removed_agents,omitempty"`

	SourcePath string `yaml:"-" json:"-"`
}

// DefaultOTelServiceName is the OTLP resource service.name used when the
// operator does not set otel.service_name.
const DefaultOTelServiceName = "hive"

// OTelConfig configures OpenTelemetry trace export. It is additive and OFF by
// default: a config with no `otel:` block (or enabled:false) yields a
// zero-overhead no-op tracer. When enabled, spans are exported over OTLP/HTTP
// to Endpoint (or the standard OTEL_EXPORTER_OTLP_ENDPOINT env var when
// Endpoint is empty).
type OTelConfig struct {
	// Enabled turns OTLP trace export on. Default false — the zero value keeps
	// every existing config a no-op with no exporter and no network activity.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Endpoint is the OTLP/HTTP collector endpoint (host:port or full URL).
	// When empty, the exporter falls back to OTEL_EXPORTER_OTLP_ENDPOINT.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	// Headers are optional static OTLP/HTTP headers, commonly used for collector
	// authentication. Values should come from env interpolation, not literals.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	// ServiceName is recorded as resource service.name. Empty defaults to "hive".
	ServiceName string `yaml:"service_name,omitempty" json:"service_name,omitempty"`
	// Insecure disables TLS for OTLP/HTTP. Leave false for https collectors.
	Insecure bool `yaml:"insecure,omitempty" json:"insecure,omitempty"`
	// SampleRatio is the head-based sampling ratio in [0.0, 1.0]. The zero
	// value is treated as "sample everything" (1.0) so an operator who only
	// sets enabled:true gets full traces; set explicitly to sample less.
	SampleRatio float64 `yaml:"sample_ratio,omitempty" json:"sample_ratio,omitempty"`
}

// ServiceNameOrDefault returns a valid resource service.name.
func (o OTelConfig) ServiceNameOrDefault() string {
	if name := strings.TrimSpace(o.ServiceName); name != "" {
		return name
	}
	return DefaultOTelServiceName
}

// IsZero reports whether the block contains no operator-provided settings.
func (o OTelConfig) IsZero() bool {
	return !o.Enabled && o.Endpoint == "" && len(o.Headers) == 0 && o.ServiceName == "" && !o.Insecure && o.SampleRatio == 0
}

// EffectiveOTel returns the preferred otel block, falling back to the legacy
// tracing block so existing configs continue to work unchanged.
func (c *Config) EffectiveOTel() OTelConfig {
	if c == nil {
		return OTelConfig{}
	}
	if !c.OTel.IsZero() {
		return c.OTel
	}
	return c.Tracing
}

// TriggerRule is one CEL-based declarative agent trigger. When Expr (a CEL
// expression over the normalized event, exposed as `event`) evaluates true for
// an incoming event, Agent is kicked. Priority orders competing rules (higher
// first). This is additive: an empty Triggers list preserves the existing
// label/governor triggering behavior. Evaluation is handled by pkg/celtrigger,
// which fails closed on malformed expressions.
type TriggerRule struct {
	Name     string `yaml:"name" json:"name"`
	Expr     string `yaml:"expr" json:"expr"`
	Agent    string `yaml:"agent" json:"agent"`
	Priority int    `yaml:"priority,omitempty" json:"priority,omitempty"`
}

// HookRule is one operator-declared state-triggered hook (RFC #4001). When the
// transition named by On durably commits — and the optional When predicate
// matches — Action is performed with Params.
//
// The transition and action vocabularies are CLOSED sets owned by pkg/hooks,
// and validation FAILS CLOSED: an unknown transition or action rejects the
// whole hook list rather than skipping the rule, so an operator never ends up
// with a hook they believe is armed and which silently never fires.
//
// There is deliberately no `exec`/`script` action. Hooks may observe and
// notify freely, but every mutating action goes through an existing audited
// API. Arbitrary code execution on a state transition is a separate RFC with
// its own sandbox story.
type HookRule struct {
	// Name identifies the hook in logs and audit entries. Required, unique.
	Name string `yaml:"name" json:"name"`
	// On is the transition to attach to, e.g. "review_rejected". Must be in
	// the pkg/hooks transition catalog.
	On string `yaml:"on" json:"on"`
	// Action is the vetted action: notify, pause, annotate, enqueue-approval.
	Action string `yaml:"action" json:"action"`
	// Params carries action-specific settings (notify's title/message/
	// priority, pause's agent/reason, annotate's note, enqueue-approval's
	// kind/summary).
	Params map[string]string `yaml:"params,omitempty" json:"params,omitempty"`
	// When is an optional CEL predicate over the transition payload, exposed
	// as `t` (e.g. `t.agent == "reviewer"`). Empty means always fire. Compiled
	// by the same fail-closed engine as `triggers:`.
	When string `yaml:"when,omitempty" json:"when,omitempty"`
	// RateLimitPerMinute caps firings of this hook. Zero uses the package
	// default; there is no unlimited setting, so a flapping transition cannot
	// become a notification storm.
	RateLimitPerMinute int `yaml:"rate_limit_per_minute,omitempty" json:"rate_limit_per_minute,omitempty"`
}

// MintConfig configures the OIDC token mint service (pkg/mint). It is additive
// and DISABLED by default: an absent `mint:` block, or Enabled=false, leaves
// existing behavior byte-identical. When enabled, the mint issues short-lived
// scoped JWTs (a Workload Identity Federation broker) that downstream cloud/
// registry WIF providers can trust via Issuer + JWKS.
type MintConfig struct {
	// Enabled turns the mint service on. Default false (deny).
	Enabled bool `yaml:"enabled,omitempty"`
	// KeyPath is the PEM path of the signing key. If the file is absent the
	// mint generates one and persists it with 0600 perms. Required when enabled.
	KeyPath string `yaml:"key_path,omitempty"`
	// Issuer is the `iss` claim and the identity WIF providers are configured to
	// trust (typically the mint's public URL). Required when enabled.
	Issuer string `yaml:"issuer,omitempty"`
	// MaxTTLSeconds bounds a minted token's lifetime. 0 uses the package default
	// (15m). The value is clamped to the package hard cap (1h) regardless.
	MaxTTLSeconds int `yaml:"max_ttl_seconds,omitempty"`
}

// AgentSandboxConfig controls the podman-rootless sandbox launcher. The top-
// level block is a global gate; per-agent config can opt specific agents in.
type AgentSandboxConfig struct {
	Enabled      bool     `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Image        string   `yaml:"image,omitempty" json:"image,omitempty"`
	EnvAllowlist []string `yaml:"env_allowlist,omitempty" json:"env_allowlist,omitempty"`
	NetworkMode  string   `yaml:"network_mode,omitempty" json:"network_mode,omitempty"`
	TimeoutS     int      `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
	WorkspaceDir string   `yaml:"workspace_dir,omitempty" json:"workspace_dir,omitempty"`
}

// AgentSandboxOverride is the per-agent sandbox opt-in block.
type AgentSandboxOverride struct {
	Enabled      *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Image        string   `yaml:"image,omitempty" json:"image,omitempty"`
	EnvAllowlist []string `yaml:"env_allowlist,omitempty" json:"env_allowlist,omitempty"`
	NetworkMode  string   `yaml:"network_mode,omitempty" json:"network_mode,omitempty"`
	TimeoutS     int      `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
}

// IoscanConfig gates the pkg/ioscan input/output security scanner (prompt-
// injection + secret/dangerous-directive detection). It is additive and, per
// audit rec #7 (F11, CWE-693), now ENABLED by default: untrusted external text
// (issue/PR titles, labels, bodies) is scanned before it is injected into an
// agent kick, and only text the input block policy trips (Critical, or High-
// severity injection) is redacted/annotated — benign text passes through
// byte-identically. The scanner is pure (no I/O, no network) and enforcement
// fails safe: a scan is only ever advisory to the caller, never a reason to
// crash a kick. Defaulting on is therefore safe — a running hive sees no change
// for ordinary titles, only genuine injection payloads are withheld.
type IoscanConfig struct {
	// Enabled turns input/output scanning on. Pointer so an omitted `enabled:`
	// key (nil) is distinguishable from an explicit false: nil DEFAULTS ON
	// (fail-safe — scan by default), while an operator can still opt out with an
	// explicit `enabled: false`. Read via IsEnabled(), never dereferenced raw.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// FailMode controls what the kick path does with Critical injection
	// findings. Empty/"open" preserves the historical behavior: redact the
	// offending text and continue the kick. "closed" blocks the kick and records
	// an ioscan_fail_closed audit entry. Read via FailClosed().
	FailMode string `yaml:"fail_mode,omitempty" json:"fail_mode,omitempty"`
	// Canaries plants per-kick exfiltration markers in agent prompts and scans
	// agent-visible egress for leaks. Default false: existing hives see no
	// canary behavior until explicitly enabled.
	Canaries bool `yaml:"canaries,omitempty" json:"canaries,omitempty"`
	// Classifier enables the optional LLM-judge semantic prompt-injection
	// classifier. It defaults off so hives that only use deterministic rules
	// make no model calls and see zero behavior change.
	Classifier IoscanClassifierConfig `yaml:"classifier,omitempty" json:"classifier,omitempty"`
}

// IoscanClassifierConfig controls the optional model-based semantic injection
// classifier that runs after deterministic ioscan redaction.
type IoscanClassifierConfig struct {
	Enabled        bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Model          string  `yaml:"model,omitempty" json:"model,omitempty"`
	WarnThreshold  float64 `yaml:"warn_threshold,omitempty" json:"warn_threshold,omitempty"`
	BlockThreshold float64 `yaml:"block_threshold,omitempty" json:"block_threshold,omitempty"`
}

// IsEnabled reports whether input/output scanning is active. Absent (nil)
// defaults to true (fail-safe): scanning is on unless an operator explicitly
// sets `enabled: false`.
func (c IoscanConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

const ioscanFailModeClosed = "closed"

// FailClosed reports whether Critical injection findings should block the
// whole kick instead of only redacting the offending untrusted text. The default
// is fail-open for backward compatibility.
func (c IoscanConfig) FailClosed() bool {
	return strings.EqualFold(c.FailMode, ioscanFailModeClosed)
}

// PlanningConfig gates the Phase 4 planning entry points that fire automatically
// (as opposed to the explicit dashboard "Plan this issue" click, which is always
// available). Today that is the `plan`/`epic` label trigger: an actionable issue
// carrying one of those labels auto-mints an epic and requests decomposition.
type PlanningConfig struct {
	// PlanFromLabel enables the label trigger. Pointer so an omitted key is
	// distinguishable from an explicit false: when nil, the trigger falls back to
	// an ACMM-level gate (on at L5+), so mature hives get it without extra config
	// while low-maturity hives stay advisory-only. An explicit value overrides the
	// ACMM gate in either direction — but see the note below: even an explicit
	// true is a no-op below L5, because the architect that decomposes the minted
	// epics is not scheduled there.
	PlanFromLabel *bool `yaml:"plan_from_label,omitempty" json:"plan_from_label,omitempty"`
}

// RetroConfig gates the post-completion retro lane. It is off by
// default: an absent `retro:` block or `enabled: false` yields zero behavior
// change. When enabled, the lane periodically scans done/closed beads that have
// a closed/merged PR association, reconstructs a compact trajectory from local
// ledgers, and files rule-based findings as advisory beads. analysis_model is
// additionally opt-in; when empty, no model is called and only deterministic
// phase-1 behavior runs.
type RetroConfig struct {
	Enabled             bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	ScanIntervalS       int    `yaml:"scan_interval_s,omitempty" json:"scan_interval_s,omitempty"`
	MaxFixAttempts      int    `yaml:"max_fix_attempts,omitempty" json:"max_fix_attempts,omitempty"`
	MaxKicks            int    `yaml:"max_kicks,omitempty" json:"max_kicks,omitempty"`
	LongStallDays       int    `yaml:"long_stall_days,omitempty" json:"long_stall_days,omitempty"`
	RecentClosedWindowS int    `yaml:"recent_closed_window_s,omitempty" json:"recent_closed_window_s,omitempty"`
	AnalysisModel       string `yaml:"analysis_model,omitempty" json:"analysis_model,omitempty"`
}

// planFromLabelMinACMM is the lowest ACMM level at which the label trigger fires
// by default. It matches planning.PlanningMinACMMLevel: the architect agent that
// decomposes the minted epics only has a cadence at L5 (4h) and L6 (15m), so
// below L5 a minted epic would sit in decompose_pending forever. It is duplicated
// here (rather than imported) to avoid a config→planning import cycle.
const planFromLabelMinACMM = 5

// PlanFromLabelEnabled reports whether the label trigger should fire, given the
// hive's ACMM level. The trigger is OFF by default and must be explicitly
// enabled (`plan_from_label: true`): the label path pipes a raw issue body into
// the architect's kick prompt with no per-kick review, so a maintainer merely
// labeling an attacker's issue would otherwise auto-fire attacker-controlled
// text into the highest-autonomy agent. Making it opt-in forces an operator to
// consciously accept that. When explicitly enabled, planning's own
// PlanIssuesFromLabels still applies a hard L5+ no-op (the architect that
// decomposes epics only has a cadence at L5/L6), so enabling it below L5 is
// inert — defense in depth that cannot be overridden away.
func (p PlanningConfig) PlanFromLabelEnabled(acmmLevel int) bool {
	if p.PlanFromLabel != nil {
		return *p.PlanFromLabel && acmmLevel >= planFromLabelMinACMM
	}
	return false
}

// ClassifierConfig makes the tier-classification keyword lists (pkg/classify)
// config-driven and dashboard-visible, mirroring how per-agent lane_keywords
// drive lane routing. Both fields are optional: when a list is empty, the
// classifier keeps its built-in default for that tier, so an absent
// `classifier:` block is byte-for-byte the old hardcoded behavior. Wired via
// classify.SetTierKeywords from cmd/hive.
type ClassifierConfig struct {
	// SimpleKeywords are title substrings that classify an issue as Tier
	// "Simple" (→ haiku). Empty keeps the built-in default set.
	SimpleKeywords []string `yaml:"simple_keywords,omitempty" json:"simple_keywords,omitempty"`
	// ComplexSignals are title substrings that classify an issue as Tier
	// "Complex" (→ opus). Empty keeps the built-in default set.
	ComplexSignals []string `yaml:"complex_signals,omitempty" json:"complex_signals,omitempty"`
}

// IntentConfig controls intent-verification reporting and merge-gate
// enforcement. The zero value is report-only with built-in path patterns, so an
// absent intent: block preserves existing merge eligibility behavior.
type IntentConfig struct {
	Enforce               bool     `yaml:"enforce,omitempty" json:"enforce,omitempty"`
	AlignmentModel        string   `yaml:"alignment_model,omitempty" json:"alignment_model,omitempty"`
	TestPathPatterns      []string `yaml:"test_path_patterns,omitempty" json:"test_path_patterns,omitempty"`
	DocsPathPatterns      []string `yaml:"docs_path_patterns,omitempty" json:"docs_path_patterns,omitempty"`
	GuardrailPathPatterns []string `yaml:"guardrail_path_patterns,omitempty" json:"guardrail_path_patterns,omitempty"`
	FeatureSignals        []string `yaml:"feature_signals,omitempty" json:"feature_signals,omitempty"`
}

// VariablesConfig declares operator-defined ${VAR} substitutions and the trust
// policy for resolvers that execute code or do network I/O. It drives the
// pluggable resolve engine (pkg/resolve) used by both config-load and per-kick
// template substitution. Absent (the default) means env-only substitution,
// byte-identical to hive's legacy behavior.
type VariablesConfig struct {
	// Security gates script/http resolvers. Honored ONLY from the trusted config
	// seed — the dashboard overlay's Security block is discarded on load so a
	// user-editable overlay can never enable code execution or network access.
	Security VarSecurityConfig `yaml:"security,omitempty"`
	// Defs maps a variable name (used as ${name}) to its definition.
	Defs map[string]VarDef `yaml:"defs,omitempty"`
}

// VarSecurityConfig is the resolver trust model. Defaults are deny.
type VarSecurityConfig struct {
	AllowExec     bool     `yaml:"allow_exec,omitempty"`
	AllowHTTP     bool     `yaml:"allow_http,omitempty"`
	HTTPAllowlist []string `yaml:"http_allowlist,omitempty"`
	ExecTimeoutS  int      `yaml:"exec_timeout_s,omitempty"`
	HTTPTimeoutS  int      `yaml:"http_timeout_s,omitempty"`

	// AllowGitHubPrompt gates the GitHub-repo prompt-source feature (agents may
	// source their kick prompt from a repo). Like AllowExec/AllowHTTP it is honored
	// ONLY from the trusted config seed — the dashboard overlay's Security block is
	// discarded on load, so a user-editable overlay can never enable this or widen
	// the allowlist. Default false (deny).
	AllowGitHubPrompt bool `yaml:"allow_github_prompt,omitempty"`
	// GitHubPromptAllowlist is the set of "owner/repo" slugs an agent's
	// prompt_source may read from. Required (non-empty) for the feature to work:
	// an empty allowlist denies all repos even when AllowGitHubPrompt is true. This
	// bounds the blast radius to repos the operator explicitly trusts, so the
	// feature cannot be used to read every repo the App happens to be installed on.
	GitHubPromptAllowlist []string `yaml:"github_prompt_allowlist,omitempty"`
}

// PromptSourceConfig describes a GitHub repo location to pull an agent's kick
// prompt from. Owner/Repo/Path are required; Ref (branch/tag/SHA) is optional
// and defaults to the repo's default branch.
type PromptSourceConfig struct {
	// Type selects the source kind. Only "github" is currently supported; an
	// empty type defaults to "github" when a repo is set.
	Type  string `yaml:"type,omitempty" json:"type,omitempty"`
	Owner string `yaml:"owner,omitempty" json:"owner,omitempty"`
	Repo  string `yaml:"repo,omitempty" json:"repo,omitempty"`
	Path  string `yaml:"path,omitempty" json:"path,omitempty"`
	Ref   string `yaml:"ref,omitempty" json:"ref,omitempty"`
}

// Slug returns the "owner/repo" identifier used for allowlist matching, or ""
// when owner or repo is unset.
func (p *PromptSourceConfig) Slug() string {
	if p == nil || p.Owner == "" || p.Repo == "" {
		return ""
	}
	return p.Owner + "/" + p.Repo
}

// IsSet reports whether this prompt source is fully specified (owner, repo, and
// path all present). A partially-filled source is treated as unset so a kick
// falls back to the inline template rather than erroring.
func (p *PromptSourceConfig) IsSet() bool {
	return p != nil && p.Owner != "" && p.Repo != "" && p.Path != ""
}

// DefinitionSourceConfig describes a GitHub repo location to pull a WHOLE agent
// definition (the portable AgentDefinition YAML) from, keeping the agent "live"
// so edits on the repo propagate on the next reload/kick. It is the whole-agent
// analogue of PromptSourceConfig. Owner/Repo/Path are required; Ref is optional
// and defaults to the repo's default branch.
//
// Security: re-applying a live definition NEVER changes security-sensitive or
// seed-only fields (the resolver trust policy, token scopes, etc.). Only the
// operator-safe presentation/behavior fields are merged — see pkg/defsrc for the
// exact allowed-field boundary. The fetch is gated to the same seed-only repo
// allowlist as prompt_source (VarSecurityConfig.GitHubPromptAllowlist), so a
// user-writable dashboard overlay can neither widen the allowlist nor point an
// agent at an arbitrary repo the App happens to be installed on.
type DefinitionSourceConfig struct {
	// Type selects the source kind. Only "github" is currently supported; an
	// empty type defaults to "github" when a repo is set.
	Type  string `yaml:"type,omitempty" json:"type,omitempty"`
	Owner string `yaml:"owner,omitempty" json:"owner,omitempty"`
	Repo  string `yaml:"repo,omitempty" json:"repo,omitempty"`
	Path  string `yaml:"path,omitempty" json:"path,omitempty"`
	Ref   string `yaml:"ref,omitempty" json:"ref,omitempty"`
	// URL is the human-facing source URL the operator pasted in the import UI
	// (e.g. the github.com blob URL). Informational only — Owner/Repo/Path/Ref
	// are authoritative for fetching. Kept so the UI can round-trip it.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
}

// Slug returns the "owner/repo" identifier used for allowlist matching, or ""
// when owner or repo is unset.
func (d *DefinitionSourceConfig) Slug() string {
	if d == nil || d.Owner == "" || d.Repo == "" {
		return ""
	}
	return d.Owner + "/" + d.Repo
}

// IsSet reports whether this definition source is fully specified (owner, repo,
// and path all present). A partially-filled source is treated as unset so an
// agent keeps its baked definition rather than erroring.
func (d *DefinitionSourceConfig) IsSet() bool {
	return d != nil && d.Owner != "" && d.Repo != "" && d.Path != ""
}

// VarDef is one operator-declared variable. `default` uses a pointer so an
// explicit empty-string default is distinguishable from "no default".
type VarDef struct {
	Type    string            `yaml:"type,omitempty"`  // env|static|script|http
	Scope   string            `yaml:"scope,omitempty"` // config|template|both (default template)
	Default *string           `yaml:"default,omitempty"`
	Value   string            `yaml:"value,omitempty"`   // static
	Env     string            `yaml:"env,omitempty"`     // env source var name
	Command []string          `yaml:"command,omitempty"` // script argv
	URL     string            `yaml:"url,omitempty"`     // http
	Headers map[string]string `yaml:"headers,omitempty"` // http
}

// DocSourceConfigYAML describes an external document to import as knowledge.
type DocSourceConfigYAML struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url,omitempty"`
	FilePath string `yaml:"file_path,omitempty"`
	Layer    string `yaml:"layer"`
}

type KnowledgeConfig struct {
	Enabled         bool                  `yaml:"enabled"`
	Engine          string                `yaml:"engine"`
	Layers          []KnowledgeLayer      `yaml:"layers"`
	Vaults          []VaultConfig         `yaml:"vaults"`
	GitSources      []GitSourceConfigYAML `yaml:"git_sources"`
	Documents       []DocSourceConfigYAML `yaml:"documents"`
	Curator         KnowledgeCurator      `yaml:"curator"`
	Primer          KnowledgePrimer       `yaml:"primer"`
	BeadSynthesizer BeadSynthesizerConfig `yaml:"bead_synthesizer"`
}

// BeadSynthesizerConfig controls automatic synthesis of completed beads into wiki facts.
// Enabled defaults to true when knowledge is enabled; set to false to opt out.
type BeadSynthesizerConfig struct {
	Enabled          *bool            `yaml:"enabled,omitempty"`
	Schedule         string           `yaml:"schedule"`
	MinConfidence    float64          `yaml:"min_confidence"`
	TargetLayer      string           `yaml:"target_layer"`
	MaxFactsPerCycle int              `yaml:"max_facts_per_cycle"`
	VaultPath        string           `yaml:"vault_path"`
	RetentionPolicy  *RetentionPolicy `yaml:"retention_policy"`
}

// RetentionPolicy controls intelligent bead lifecycle management.
type RetentionPolicy struct {
	MaxBeads               int  `yaml:"max_beads"`
	ArchiveAfterSynthDays  int  `yaml:"archive_after_synth_days"`
	HighPriorityRetainDays int  `yaml:"high_priority_retain_days"`
	PreserveWithDeps       bool `yaml:"preserve_with_deps"`
}

// IsEnabled returns whether bead synthesis is enabled (defaults to true).
func (b BeadSynthesizerConfig) IsEnabled() bool {
	if b.Enabled == nil {
		return true
	}
	return *b.Enabled
}

// GitSourceConfigYAML describes a remote git repo (or subdirectory) to index
// as a knowledge source. Any layer level can have git sources.
type GitSourceConfigYAML struct {
	Name    string `yaml:"name"`
	URL     string `yaml:"url"`
	Branch  string `yaml:"branch,omitempty"`
	Subpath string `yaml:"subpath,omitempty"`
	Layer   string `yaml:"layer"`
}

// VaultConfig describes a file-based Obsidian vault to auto-connect on startup.
type VaultConfig struct {
	Name      string `yaml:"name"`
	Path      string `yaml:"path"`
	AutoIndex bool   `yaml:"auto_index"`
	GitSync   bool   `yaml:"git_sync"`
}

type KnowledgeLayer struct {
	Type   string `yaml:"type"`
	Path   string `yaml:"path,omitempty"`
	URL    string `yaml:"url,omitempty"`
	Shared bool   `yaml:"shared"`
}

type KnowledgeCurator struct {
	Schedule             string   `yaml:"schedule"`
	ExtractFrom          []string `yaml:"extract_from"`
	AutoPromoteThreshold float64  `yaml:"auto_promote_threshold"`
}

type KnowledgePrimer struct {
	MaxFacts      int      `yaml:"max_facts"`
	Priority      []string `yaml:"priority"`
	MergeStrategy string   `yaml:"merge_strategy"`
}

type ProjectConfig struct {
	Org         string   `yaml:"org"`
	Name        string   `yaml:"name"`
	Repos       []string `yaml:"repos"`
	AIAuthor    string   `yaml:"ai_author"`
	PrimaryRepo string   `yaml:"primary_repo"`
	OpenPRs     *bool    `yaml:"open_prs,omitempty"`
	// Forge selects the source forge for this project: "github" (default) or
	// "gitlab". It is additive — an absent value means GitHub, so existing
	// GitHub-only configs are unaffected. Use ForgeKind() to read it with the
	// default applied.
	Forge string `yaml:"forge,omitempty"`
	// IssueFilter gates which open issues agents may initiate work on: the
	// require_labels allow-list ("only work issues labeled X"). The exclude
	// polarity is Governor.Labels.Exempt, which wins on conflict. Absent/empty
	// = no filtering, the pre-existing behavior. See IssueFilterConfig.
	IssueFilter IssueFilterConfig `yaml:"issue_filter,omitempty"`
}

const (
	// ForgeGitHub is the default forge kind (GitHub / GHE).
	ForgeGitHub = "github"
	// ForgeGitLab selects the GitLab forge (gitlab.com or self-managed).
	ForgeGitLab = "gitlab"
	// ForgeGitea selects the Gitea/Forgejo forge (self-managed or Codeberg).
	ForgeGitea = "gitea"
)

// ForgeKind returns the configured forge kind, defaulting to ForgeGitHub when
// unset so existing GitHub-only configs keep working unchanged.
func (p *ProjectConfig) ForgeKind() string {
	if p.Forge == "" {
		return ForgeGitHub
	}
	return p.Forge
}

// PRsAllowed returns whether agents may open pull requests. Defaults to true.
func (p *ProjectConfig) PRsAllowed() bool {
	if p.OpenPRs != nil {
		return *p.OpenPRs
	}
	return true
}

type PoliciesConfig struct {
	Repo         string        `yaml:"repo"`
	Branch       string        `yaml:"branch"`
	Path         string        `yaml:"path"`
	PollInterval time.Duration `yaml:"poll_interval"`
	LocalDir     string        `yaml:"local_dir"`
}

// StatsDisplayEntry defines a single metric to show in the agent's sidebar/detail view.
type StatsDisplayEntry struct {
	Key        string `yaml:"key" json:"key"`
	Label      string `yaml:"label" json:"label"`
	Source     string `yaml:"source" json:"source"`
	Field      string `yaml:"field" json:"field"`
	Style      string `yaml:"style" json:"style"`
	TrendField string `yaml:"trend_field,omitempty" json:"trendField,omitempty"`
	Target     int    `yaml:"target,omitempty" json:"target,omitempty"`
	// Desc is a one-line explanation of what the stat verifies, rendered
	// as a hover tooltip in the dashboard (health checks especially).
	Desc string `yaml:"desc,omitempty" json:"desc,omitempty"`
}

// ChannelConfig declares a trigger channel for an agent.
type ChannelConfig struct {
	Type     string            `yaml:"type" json:"type"`
	Enabled  *bool             `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Events   []string          `yaml:"events,omitempty" json:"events,omitempty"`
	Patterns []string          `yaml:"patterns,omitempty" json:"patterns,omitempty"`
	Schedule string            `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	Match    map[string]string `yaml:"match,omitempty" json:"match,omitempty"`
	Repos    []string          `yaml:"repos,omitempty" json:"repos,omitempty"`
}

// IsEnabled returns whether this channel is active (defaults to true).
func (c *ChannelConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// ToolRule is a single allow/deny rule for a tool pattern.
type ToolRule struct {
	Pattern string `yaml:"pattern" json:"pattern"`
	Action  string `yaml:"action" json:"action"`
	Reason  string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// ToolsConfig declares what tools an agent can use.
type ToolsConfig struct {
	Preset string     `yaml:"preset,omitempty" json:"preset,omitempty"`
	Rules  []ToolRule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// ConnectionAuth describes how to authenticate to a connection.
type ConnectionAuth struct {
	Type   string `yaml:"type" json:"type"`
	EnvVar string `yaml:"env_var,omitempty" json:"env_var,omitempty"`
	File   string `yaml:"file,omitempty" json:"file,omitempty"`
}

// ConnectionConfig declares an external service integration for an agent.
type ConnectionConfig struct {
	Name    string            `yaml:"name" json:"name"`
	Type    string            `yaml:"type" json:"type"`
	URI     string            `yaml:"uri,omitempty" json:"uri,omitempty"`
	Auth    *ConnectionAuth   `yaml:"auth,omitempty" json:"auth,omitempty"`
	EnvName string            `yaml:"env_name,omitempty" json:"env_name,omitempty"`
	Options map[string]string `yaml:"options,omitempty" json:"options,omitempty"`
}

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

type AgentConfig struct {
	ID           string `yaml:"id" json:"id,omitempty"`
	Backend      string `yaml:"backend" json:"backend,omitempty"`
	Model        string `yaml:"model" json:"model,omitempty"`
	BeadsDir     string `yaml:"beads_dir" json:"beads_dir,omitempty"`
	Enabled      bool   `yaml:"enabled" json:"enabled,omitempty"`
	Replicas     int    `yaml:"replicas,omitempty" json:"replicas,omitempty"`
	ReplicaOf    string `yaml:"-" json:"replicaOf,omitempty"`
	ReplicaIndex int    `yaml:"-" json:"replicaIndex,omitempty"`
	ReplicaCount int    `yaml:"-" json:"replicaCount,omitempty"`
	// Paused persists an operator pause across restarts/upgrades. Without
	// this, every pod restart rebuilt agents un-paused (Go zero value), so
	// an operator pause was silently undone on the next upgrade.
	Paused      bool `yaml:"paused" json:"paused,omitempty"`
	ClearOnKick bool `yaml:"clear_on_kick" json:"clear_on_kick"`
	CLIPinned   bool `yaml:"cli_pinned" json:"cli_pinned,omitempty"`

	// ModelOwner / BackendOwner record WHO last set Model / Backend. An ACMM
	// pack owns these fields until an operator changes them in the Governor
	// grid; from that point the operator owns them and ApplyPack must not
	// reconcile them back to the pack value.
	//
	// Without this, a pack re-apply — which happens on EVERY hive restart
	// (cmd/hive/main.go "merging pack updates") — rewrote Model back to the
	// pack default, so an operator's model choice silently reverted on the
	// next restart. That is the "I've deleted those 4 times, they always come
	// back" report: the revert was a restart, not a propagation delay.
	//
	// Stored as a string owner rather than a bool so "never set" (empty),
	// "pack-owned" and "operator-owned" stay distinguishable across upgrades
	// of hives whose config predates this field.
	ModelOwner      string `yaml:"model_owner" json:"model_owner,omitempty"`
	BackendOwner    string `yaml:"backend_owner" json:"backend_owner,omitempty"`
	StaleTimeout    int    `yaml:"stale_timeout" json:"stale_timeout,omitempty"`
	RestartStrategy string `yaml:"restart_strategy" json:"restart_strategy,omitempty"`
	LaunchCmd       string `yaml:"launch_cmd" json:"launch_cmd,omitempty"`
	DisplayName     string `yaml:"display_name" json:"display_name,omitempty"`
	Description     string `yaml:"description" json:"description,omitempty"`

	// Phase 2: config-driven agent behavior fields
	Role           string   `yaml:"role" json:"role,omitempty"`
	SortOrder      int      `yaml:"sort_order" json:"sort_order,omitempty"`
	Emoji          string   `yaml:"emoji" json:"emoji,omitempty"`
	Color          string   `yaml:"color" json:"color,omitempty"`
	Aliases        []string `yaml:"aliases" json:"aliases,omitempty"`
	LaneKeywords   []string `yaml:"lane_keywords" json:"lane_keywords,omitempty"`
	DetectKeywords []string `yaml:"detect_keywords" json:"detect_keywords,omitempty"`
	KickTemplate   string   `yaml:"kick_template" json:"kick_template,omitempty"`
	// PromptSource, when set, sources the agent's kick prompt from a GitHub repo
	// instead of (or in addition to) an inline KickTemplate. It is resolved live
	// at kick time via the hive's GitHub App token, with graceful fallback to the
	// baked/inline template when the repo is unreachable. The user-writable
	// dashboard overlay may set this, but the fetch is gated to a seed-only repo
	// allowlist (see VarSecurityConfig.GitHubPromptAllowlist), so a compromised
	// overlay cannot read arbitrary repos the App is installed on.
	PromptSource *PromptSourceConfig `yaml:"prompt_source,omitempty" json:"prompt_source,omitempty"`
	// DefinitionSource, when set, keeps the WHOLE agent linked to a GitHub repo:
	// the portable AgentDefinition YAML at owner/repo/path@ref is re-fetched on
	// reload and its operator-safe fields are merged over the baked agent, so
	// edits on the repo propagate without a redeploy. It never overrides
	// security-sensitive/seed-only fields (see pkg/defsrc for the field boundary)
	// and is gated to the same seed-only repo allowlist as PromptSource, so a
	// user-writable overlay can neither widen the allowlist nor escalate an
	// agent's privileges via a live definition. On fetch failure the last-known-good
	// baked definition is kept — a live agent is never blanked or crashed.
	DefinitionSource *DefinitionSourceConfig `yaml:"definition_source,omitempty" json:"definition_source,omitempty"`
	IncludeRepos     *bool                   `yaml:"include_repos" json:"include_repos,omitempty"`
	MetricsCollector string                  `yaml:"metrics_collector" json:"metrics_collector,omitempty"`
	BeadRole         string                  `yaml:"bead_role" json:"bead_role,omitempty"`
	StatsDisplay     []StatsDisplayEntry     `yaml:"stats_display" json:"stats_display,omitempty"`
	ACMMLevels       []int                   `yaml:"acmm_levels" json:"acmm_levels,omitempty"`
	Mode             string                  `yaml:"mode" json:"mode,omitempty"`
	// Converse opts this agent into the orthogonal `converse` capability
	// (#4492): posting comments on issues and PRs, and leaving PR reviews,
	// independently of Mode. It does not grant issue creation, editing,
	// relabelling, pushing or merging — those stay on the Mode ladder.
	//
	// A POINTER so "unset" stays distinguishable from "explicitly false" across
	// pack re-apply and the dashboard overlay, the same reason ModelOwner
	// exists. Unset means off: `converse` is opt-in at every ACMM level, so an
	// existing hive that says nothing behaves exactly as it did.
	//
	// The shape this exists for is `mode: ADVISORY` + `converse: true` — an
	// agent that can reply on a thread it was mentioned in but cannot file,
	// edit or relabel anything.
	Converse    *bool  `yaml:"converse,omitempty" json:"converse,omitempty"`
	OnDemand    bool   `yaml:"on_demand" json:"on_demand,omitempty"`
	CavemanMode string `yaml:"caveman_mode" json:"caveman_mode,omitempty"`
	// ExplainMode opts this agent into emitting EXPLAIN-prefixed reasoning
	// lines alongside its tool calls, so an operator debugging "why did it do
	// that" has something to read (#3887). Off by default because the
	// explanation costs tokens on every kick.
	//
	// TRI-STATE, and the distinction matters: "" means "inherit the hive-wide
	// default" (HIVE_EXPLAIN_MODE), while "off" is an explicit per-agent
	// opt-OUT that survives an operator turning explanation on fleet-wide.
	// Valid values: "" | off | brief | full — see ExplainMode* constants.
	ExplainMode string `yaml:"explain_mode,omitempty" json:"explain_mode,omitempty"`
	// Sandbox opts this agent into phase-1 sandbox execution when the global
	// agent_sandbox.enabled gate is also true.
	Sandbox *AgentSandboxOverride `yaml:"sandbox,omitempty" json:"sandbox,omitempty"`

	// Channels declares how this agent gets triggered (kick, webhook, discord, schedule, bead).
	// When nil/empty, the agent uses governor timer kicks by default (implicit kick channel).
	Channels []ChannelConfig `yaml:"channels,omitempty" json:"channels,omitempty"`

	// Tools declares what tools this agent can use. When nil, the existing Mode field governs.
	Tools *ToolsConfig `yaml:"tools,omitempty" json:"tools,omitempty"`

	// Connections declares external service integrations (MCP servers, APIs, knowledge sources).
	Connections []ConnectionConfig `yaml:"connections,omitempty" json:"connections,omitempty"`

	// Skills names reusable "how to do X" skills to resolve out of the hive's
	// skill registry (pkg/skillreg, loaded from the host-local skills directory)
	// and inject into this agent's kick context. Names are resolved at kick
	// time, so editing a skill file takes effect on the next kick without a
	// restart. An unknown name is skipped, not fatal: a typo degrades the kick
	// rather than blocking the agent.
	//
	// This is deliberately host-local rather than per-repo. Hive agents work
	// over the GitHub API and have no guaranteed per-repo checkout, so a
	// repo-declared skills directory would resolve to nothing on most kicks;
	// the registry directory is the same kind of operator-managed volume as
	// /data/policies and is present on every hive host.
	Skills []string `yaml:"skills,omitempty" json:"skills,omitempty"`

	// Managed is true for agents loaded from the overlay directory (not base config).
	Managed bool `yaml:"-" json:"managed"`

	// clearOnKickSet tracks whether YAML explicitly set clear_on_kick to false
	clearOnKickSet bool
	// enabledSet tracks whether YAML explicitly set enabled (distinguishes
	// "not specified" from "enabled: false").
	enabledSet bool
	// name is the YAML map key, set during config load
	name string
}

// Name returns the human-readable YAML key for this agent.
func (a *AgentConfig) Name() string {
	return a.name
}

// IsReplica reports whether this agent was materialized from another agent's
// replicas setting rather than declared directly in YAML.
func (a AgentConfig) IsReplica() bool { return a.ReplicaOf != "" }

// BaseName returns the declared agent name whose config/prompt this agent uses.
func (a AgentConfig) BaseName() string {
	if a.ReplicaOf != "" {
		return a.ReplicaOf
	}
	if a.name != "" {
		return a.name
	}
	return a.ID
}

// HasChannel returns true if the agent has a channel of the given type.
func (a *AgentConfig) HasChannel(t string) bool {
	for _, ch := range a.Channels {
		if ch.Type == t {
			return true
		}
	}
	return false
}

// ChannelsOfType returns all channels matching the given type.
func (a *AgentConfig) ChannelsOfType(t string) []ChannelConfig {
	var result []ChannelConfig
	for _, ch := range a.Channels {
		if ch.Type == t {
			result = append(result, ch)
		}
	}
	return result
}

// UsesGovernorKick returns true when the agent should receive governor timer kicks.
// This is true when no channels are declared (implicit kick) or when an explicit
// kick channel is present.
func (a *AgentConfig) UsesGovernorKick() bool {
	if len(a.Channels) == 0 {
		return true
	}
	return a.HasChannel("kick")
}

// ShouldIncludeRepos returns whether the repos section should be appended to kicks.
// Defaults to true for all agents except those with IncludeRepos explicitly set to false.
func (a *AgentConfig) ShouldIncludeRepos() bool {
	if a.IncludeRepos != nil {
		return *a.IncludeRepos
	}
	return true
}

// SandboxEnabled reports whether an agent's per-agent sandbox block opts it in
// under the global phase-1 gate.
func (a *AgentConfig) SandboxEnabled(global AgentSandboxConfig) bool {
	if !global.Enabled || a == nil || a.Sandbox == nil || a.Sandbox.Enabled == nil {
		return false
	}
	return *a.Sandbox.Enabled
}

// AgentSandboxGateWarnings reports the sandbox misconfigurations that the
// two-gate opt-in otherwise makes SILENT. Empty means nothing to say.
//
// SandboxEnabled (above) requires BOTH agent_sandbox.enabled AND a per-agent
// sandbox.enabled: true. That second gate is deliberate — a sandboxed agent
// runs a completely different execution model (no tmux CLI at all; every kick
// is a podman run against the primary repo), and startSandboxKickLocked in
// pkg/agent has NO fallback to tmux: an agent flipped on without a resolvable
// image fails every kick outright. So the gate is not something to quietly
// collapse.
//
// What it must not do is stay invisible. The dashboard's Security tab writes
// only the GLOBAL flag (handleGovernorSecurity), and it is the only sandbox
// control the UI offers — so an owner can turn "agent sandbox" on, be told the
// setting was updated, and have every agent keep running unconfined. The
// response's own sandboxedAgents field stays 0 and nothing explains why.
//
// #4918 is what that silence costs. An agent doing correct work on an assigned
// third-party repo ran that repo's test suite, a hook escaped its stubs, and
// `rpm-ostree kargs` reached the operator's real deployment. An operator who
// had flipped the sandbox toggle on would reasonably believe they were covered.
//
// These are WARNINGS, not validation errors: every state described here is a
// legal config, and refusing to boot on it would turn a diagnostic into an
// outage.
func AgentSandboxGateWarnings(cfg *Config) []string {
	if cfg == nil || !cfg.AgentSandbox.Enabled {
		// The sandbox is off globally. That is the documented default and the
		// posture the docs describe; it is not a misconfiguration.
		return nil
	}

	var optedIn, noImage []string
	for name, a := range cfg.Agents {
		if !a.SandboxEnabled(cfg.AgentSandbox) {
			continue
		}
		optedIn = append(optedIn, name)
		if strings.TrimSpace(a.SandboxImage(cfg.AgentSandbox)) == "" {
			noImage = append(noImage, name)
		}
	}
	sort.Strings(optedIn)
	sort.Strings(noImage)

	var out []string
	if len(optedIn) == 0 {
		out = append(out, fmt.Sprintf(
			"agent_sandbox.enabled is true but NO agent is opted in, so the sandbox is inert and all %d agent(s) still run unconfined on the tmux path — "+
				"the per-agent gate is separate: set `sandbox: {enabled: true}` on each agent that should be sandboxed (#4918)",
			len(cfg.Agents)))
		return out
	}
	if len(noImage) > 0 {
		out = append(out, fmt.Sprintf(
			"agent(s) %s are sandbox-opted-in but resolve no sandbox image; sandboxed kicks have no tmux fallback and will fail outright — "+
				"set agent_sandbox.image (or the per-agent sandbox.image)",
			strings.Join(noImage, ", ")))
	}
	if len(optedIn) < len(cfg.Agents) {
		out = append(out, fmt.Sprintf(
			"agent_sandbox.enabled is true but only %d of %d agent(s) are opted in (%s); the rest still run unconfined on the tmux path (#4918)",
			len(optedIn), len(cfg.Agents), strings.Join(optedIn, ", ")))
	}
	return out
}

// SandboxImage returns the per-agent image override, then the global default.
func (a *AgentConfig) SandboxImage(global AgentSandboxConfig) string {
	if a != nil && a.Sandbox != nil && a.Sandbox.Image != "" {
		return a.Sandbox.Image
	}
	return global.Image
}

// SandboxEnvAllowlist returns the per-agent env allowlist when set; otherwise
// the global allowlist. Credentials are still filtered by pkg/sandbox.
func (a *AgentConfig) SandboxEnvAllowlist(global AgentSandboxConfig) []string {
	if a != nil && a.Sandbox != nil && len(a.Sandbox.EnvAllowlist) > 0 {
		return append([]string(nil), a.Sandbox.EnvAllowlist...)
	}
	return append([]string(nil), global.EnvAllowlist...)
}

// SandboxNetworkMode returns the per-agent network override, then the global
// sandbox network mode. Empty lets the executor choose its safe default.
func (a *AgentConfig) SandboxNetworkMode(global AgentSandboxConfig) string {
	if a != nil && a.Sandbox != nil && a.Sandbox.NetworkMode != "" {
		return a.Sandbox.NetworkMode
	}
	return global.NetworkMode
}

// SandboxTimeoutS returns the per-agent timeout override, then the global
// timeout. Non-positive values let the executor use its default.
func (a *AgentConfig) SandboxTimeoutS(global AgentSandboxConfig) int {
	if a != nil && a.Sandbox != nil && a.Sandbox.TimeoutS > 0 {
		return a.Sandbox.TimeoutS
	}
	return global.TimeoutS
}

// GetBeadRole returns the bead role, defaulting to "worker".
func (a *AgentConfig) GetBeadRole() string {
	if a.BeadRole != "" {
		return a.BeadRole
	}
	return "worker"
}

// GetSortOrder returns the sort order. Supervisor-role agents default to 0 (first).
func (a *AgentConfig) GetSortOrder() int {
	if a.SortOrder != 0 {
		return a.SortOrder
	}
	if a.BeadRole == "supervisor" {
		return 0
	}
	return 100
}

func (a *AgentConfig) UnmarshalYAML(value *yaml.Node) error {
	type plain AgentConfig
	if err := value.Decode((*plain)(a)); err != nil {
		return err
	}
	// Check if clear_on_kick / enabled were explicitly present in YAML
	for i := 0; i < len(value.Content)-1; i += 2 {
		switch value.Content[i].Value {
		case "clear_on_kick":
			a.clearOnKickSet = true
		case "enabled":
			a.enabledSet = true
		}
	}
	return nil
}

// Ownership markers for AgentConfig.ModelOwner / BackendOwner.
const (
	// FieldOwnerPack marks a value written by an ACMM pack. Pack-owned values
	// are reconciled to the current pack on every apply.
	FieldOwnerPack = "pack"
	// FieldOwnerOperator marks a value an operator chose in the Governor grid.
	// Operator-owned values are never overwritten by a pack apply.
	FieldOwnerOperator = "operator"
)

// ModelIsOperatorOwned reports whether an operator explicitly chose this
// agent's model, which makes it immune to pack reconciliation.
func (a AgentConfig) ModelIsOperatorOwned() bool {
	return a.ModelOwner == FieldOwnerOperator
}

// BackendIsOperatorOwned reports whether an operator explicitly chose this
// agent's backend (the grid's "method" column).
func (a AgentConfig) BackendIsOperatorOwned() bool {
	return a.BackendOwner == FieldOwnerOperator
}

// EnabledExplicitlySet returns true when the user's YAML explicitly set the
// "enabled" field (allowing us to distinguish "not specified" from "enabled: false").
func (a *AgentConfig) EnabledExplicitlySet() bool {
	return a.enabledSet
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

// WorkSourceConfig selects where hive reads work items (Step 01 of the loop).
// Absent or type="" defaults to GitHub Issues — backward-compatible for all
// existing hives.
type WorkSourceConfig struct {
	// Type selects the work source: "" | "github" | "github_projects" | "linear" | "jira"
	Type string `yaml:"type" json:"type"`
	// GitHubProjects configures the GitHub Projects v2 adapter.
	GitHubProjects GitHubProjectsSourceConfig `yaml:"github_projects,omitempty" json:"github_projects,omitempty"`
	// Linear configures the Linear GraphQL adapter.
	Linear LinearSourceConfig `yaml:"linear,omitempty" json:"linear,omitempty"`
	// Jira configures the Jira Cloud REST v3 adapter.
	Jira JiraSourceConfig `yaml:"jira,omitempty" json:"jira,omitempty"`
}

// IsZero reports whether no work source has been configured at all: no type
// and no adapter-specific settings. Used by the dashboard-overlay reload to
// decide whether the overlay carries an operator-set work source.
func (w WorkSourceConfig) IsZero() bool {
	return w.Type == "" &&
		reflect.DeepEqual(w.GitHubProjects, GitHubProjectsSourceConfig{}) &&
		reflect.DeepEqual(w.Linear, LinearSourceConfig{}) &&
		reflect.DeepEqual(w.Jira, JiraSourceConfig{})
}

// GitHubProjectsSourceConfig configures the GitHub Projects v2 work source.
type GitHubProjectsSourceConfig struct {
	ProjectNumber  int      `yaml:"project_number" json:"project_number"`
	Org            string   `yaml:"org,omitempty" json:"org,omitempty"`
	States         []string `yaml:"states,omitempty" json:"states,omitempty"`
	PriorityField  string   `yaml:"priority_field,omitempty" json:"priority_field,omitempty"`
	IterationField string   `yaml:"iteration_field,omitempty" json:"iteration_field,omitempty"`
	DefaultRepo    string   `yaml:"default_repo,omitempty" json:"default_repo,omitempty"`
}

// LinearSourceConfig configures the Linear GraphQL work source.
type LinearSourceConfig struct {
	APIKey     string                   `yaml:"api_key,omitempty" json:"api_key,omitempty"`
	Teams      []LinearTeamSourceConfig `yaml:"teams,omitempty" json:"teams,omitempty"`
	HoldLabels []string                 `yaml:"hold_labels,omitempty" json:"hold_labels,omitempty"`
	// AssignedOnly narrows enumeration to issues assigned/delegated to the
	// installed Linear agent app (RFC #4492 Part 2, component E). Opt-in and
	// fail-closed: it requires the agent to be connected (the app user id is
	// learned at install time), and worksource construction errors when it is
	// set without an install rather than silently enumerating everything.
	AssignedOnly bool `yaml:"assigned_only,omitempty" json:"assigned_only,omitempty"`
	// SessionAgent names the hive agent that receives Linear agent sessions
	// (delegations and mentions). When empty and exactly one agent is
	// configured, that agent is used; otherwise session events are
	// acknowledged with an error activity naming the missing config.
	SessionAgent string `yaml:"session_agent,omitempty" json:"session_agent,omitempty"`
}

// LinearTeamSourceConfig maps one Linear team to the GitHub repo agents work in.
type LinearTeamSourceConfig struct {
	Key      string                      `yaml:"key" json:"key"`
	Repo     string                      `yaml:"repo" json:"repo"`
	States   []string                    `yaml:"states,omitempty" json:"states,omitempty"`
	Projects []LinearProjectSourceConfig `yaml:"projects,omitempty" json:"projects,omitempty"`
	Cycles   string                      `yaml:"cycles,omitempty" json:"cycles,omitempty"`
}

type LinearProjectSourceConfig struct {
	Name string `yaml:"name" json:"name"`
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty"`
}

// JiraSourceConfig configures the Jira Cloud REST v3 work source.
type JiraSourceConfig struct {
	BaseURL     string   `yaml:"base_url" json:"base_url"`
	Email       string   `yaml:"email" json:"email"`
	APIToken    string   `yaml:"api_token,omitempty" json:"api_token,omitempty"`
	ProjectKeys []string `yaml:"project_keys,omitempty" json:"project_keys,omitempty"`
	JQL         string   `yaml:"jql,omitempty" json:"jql,omitempty"`
	Repo        string   `yaml:"repo,omitempty" json:"repo,omitempty"`
	HoldLabels  []string `yaml:"hold_labels,omitempty" json:"hold_labels,omitempty"`
}

// ProjectObservabilityBackendRef names references an agent may place in managed
// project configuration. Values are identifiers only: EndpointEnv is an
// environment-variable NAME and CredentialSecret is a Kubernetes-style
// "secret-name/key" reference, never a literal endpoint or credential.
type ProjectObservabilityBackendRef struct {
	EndpointEnv      string `yaml:"endpoint_env,omitempty" json:"endpoint_env,omitempty"`
	CredentialSecret string `yaml:"credential_secret,omitempty" json:"credential_secret,omitempty"`
}

// ProjectObservabilityConfig is the operator-confirmed target stack for the
// managed project's telemetry and operations agents. Empty means detect and
// report only; it never authorizes an exporter to send data off-box.
type ProjectObservabilityConfig struct {
	OpenSource []string                                  `yaml:"open_source,omitempty" json:"open_source,omitempty"`
	KubeNative []string                                  `yaml:"kube_native,omitempty" json:"kube_native,omitempty"`
	Commercial []string                                  `yaml:"commercial,omitempty" json:"commercial,omitempty"`
	References map[string]ProjectObservabilityBackendRef `yaml:"references,omitempty" json:"references,omitempty"`
}

// PromptSection renders the confirmed managed-project targets without exposing
// any secret values. The result is injected only into the telemetry and
// operations policy templates through ${PROJECT_OBSERVABILITY}.
func (p ProjectObservabilityConfig) PromptSection() string {
	var b strings.Builder
	b.WriteString("MANAGED-PROJECT OBSERVABILITY TARGETS (operator-confirmed):\n")
	writeFamily := func(label string, values []string) {
		if len(values) == 0 {
			b.WriteString("  " + label + ": (none configured)\n")
			return
		}
		b.WriteString("  " + label + ": " + strings.Join(values, ", ") + "\n")
	}
	writeFamily("open source", p.OpenSource)
	writeFamily("kube-native", p.KubeNative)
	writeFamily("commercial", p.Commercial)
	if len(p.OpenSource)+len(p.KubeNative)+len(p.Commercial) == 0 {
		b.WriteString("  No backend is confirmed. Detect the existing stack and report recommendations only; do not add an exporter or external data flow.\n")
	}
	if len(p.References) > 0 {
		b.WriteString("  safe references (names only):\n")
		keys := make([]string, 0, len(p.References))
		for name := range p.References {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			ref := p.References[name]
			parts := make([]string, 0, 2)
			if ref.EndpointEnv != "" {
				parts = append(parts, "endpoint env="+ref.EndpointEnv)
			}
			if ref.CredentialSecret != "" {
				parts = append(parts, "credential secret="+ref.CredentialSecret)
			}
			if len(parts) > 0 {
				b.WriteString("    " + name + ": " + strings.Join(parts, ", ") + "\n")
			}
		}
	}
	return strings.TrimSpace(b.String())
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

// RepoCount returns the number of repos this hive watches, for threshold
// scaling. A hive with an empty repos: list still watches at least its primary
// repo, so the floor is 1.
func (p ProjectConfig) RepoCount() int {
	if n := len(p.Repos); n > 0 {
		return n
	}
	return 1
}

// AttributionTrailerEnabled reports whether the visible attribution trailer on
// hive-created PRs/issues is on for this hive. Default ON: a hive that says
// nothing gets the trailer; set `governor.attribution_trailer: false` to hide
// it. The audit-log entry for each creation is unconditional and is NOT gated
// by this.
func (g *GovernorConfig) AttributionTrailerEnabled() bool {
	return g.AttributionTrailer == nil || *g.AttributionTrailer
}

// GatewayConfig is one named, OpenAI-compatible model gateway. A hive may
// configure several at once (e.g. OpenRouter for public models plus a private
// LiteLLM proxy for internal ones); each agent picks one by naming it as its
// backend. Secrets are referenced by env-var NAME or file PATH only — never
// inlined — matching the LiteLLM/inference key-handling rule elsewhere.
type GatewayConfig struct {
	// Name is the unique gateway id an agent names as its backend (e.g.
	// "openrouter", "corp-litellm"). Must be non-empty and unique per hive.
	Name string `yaml:"name" json:"name"`
	// Kind drives preset defaults + labeling: openrouter | litellm | vllm |
	// llm-d | watsonx | custom. Purely descriptive at runtime (all are
	// OpenAI-compatible); the endpoint is what actually routes. The one
	// exception is "watsonx", which additionally needs an IAM-minted bearer
	// (not the raw key) and a project_id header — see ProjectID below and the
	// watsonx token minter (pkg/watsonx).
	Kind string `yaml:"kind" json:"kind,omitempty"`
	// Endpoint is the OpenAI-compatible base URL, e.g. https://openrouter.ai/api/v1.
	// For a watsonx gateway this is the model-gateway base
	// https://<region>.ml.cloud.ibm.com/ml/gateway — hive appends /v1/models and
	// /v1/chat/completions to reach watsonx's OpenAI-compatible surface
	// (.../ml/gateway/v1/models, .../ml/gateway/v1/chat/completions).
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	// APIKeyEnv / APIKeyFile resolve this gateway's key (env var NAME and/or file
	// PATH — never the value). Empty is allowed for keyless endpoints (some vLLM).
	// For watsonx this resolves the IBM Cloud API key that is exchanged for a
	// short-lived IAM token; the raw key is never sent as the bearer.
	APIKeyEnv  string `yaml:"api_key_env" json:"api_key_env,omitempty"`
	APIKeyFile string `yaml:"api_key_file" json:"api_key_file,omitempty"`
	// DefaultModel is used when an agent routed through this gateway selects none.
	DefaultModel string `yaml:"default_model" json:"default_model,omitempty"`
	// CABundle is an optional PEM path for a private CA (never disables verify).
	CABundle string `yaml:"ca_bundle" json:"ca_bundle,omitempty"`
	// ProjectID is the watsonx project (or space) id an OpenAI client does not
	// send but watsonx requires for billing/limits. It is sent as the
	// X-IBM-Project-ID header on outbound requests to a watsonx gateway. Only
	// meaningful for Kind == watsonx; omitempty keeps existing gateways
	// byte-identical in hive.yaml. Not a secret (an identifier, not a
	// credential), so unlike the key it is stored inline.
	ProjectID string `yaml:"project_id,omitempty" json:"project_id,omitempty"`
	// Region is the watsonx region slug (e.g. us-south, eu-de, jp-tok) the UI
	// preset uses to build the endpoint template. Purely a convenience for the
	// preset; Endpoint is authoritative. Only meaningful for Kind == watsonx.
	Region string `yaml:"region,omitempty" json:"region,omitempty"`
	// KeyName is an optional, human-chosen LABEL for this gateway's configured
	// key ("Team inference key", "andy personal", …). It is safe-to-show
	// metadata, NOT a secret — it records WHICH key a gateway is set to use so
	// managers can tell keys apart without ever seeing the value. Mirrors the
	// bob key's KeyName (#3596/#3598), now generalized to every gateway kind
	// (litellm / openrouter / watsonx / vllm). omitempty keeps hive.yaml on
	// existing gateways (which never recorded a name) byte-identical on
	// round-trip; an absent name is a normal, backwards-compatible state the
	// dashboard renders as "(unnamed)" rather than an error.
	KeyName string `yaml:"key_name,omitempty" json:"key_name,omitempty"`
}

// gatewayKind values.
const (
	GatewayKindOpenRouter = "openrouter"
	GatewayKindGroq       = "groq"
	GatewayKindLiteLLM    = "litellm"
	GatewayKindVLLM       = "vllm"
	GatewayKindLLMD       = "llm-d"
	GatewayKindWatsonx    = "watsonx"
	GatewayKindCustom     = "custom"

	// legacyLiteLLMGatewayName is the name of the implicit gateway synthesized
	// from the legacy Governor.LiteLLM block when no gateways are configured. It
	// matches the historical "litellm" agent backend so existing agents route
	// unchanged.
	legacyLiteLLMGatewayName = "litellm"
)

// ResolvedGateways returns the effective gateway list: the explicitly-configured
// Gateways when present, otherwise a single implicit gateway synthesized from the
// legacy LiteLLM block (only if that block has an endpoint). This is what lets a
// hive with no `gateways:` and a classic `litellm:` block keep working, while a
// hive that lists gateways gets exactly those.
func (g GovernorConfig) ResolvedGateways() []GatewayConfig {
	if len(g.Gateways) > 0 {
		return g.Gateways
	}
	if g.LiteLLM.Endpoint == "" {
		return nil
	}
	return []GatewayConfig{{
		Name:         legacyLiteLLMGatewayName,
		Kind:         GatewayKindLiteLLM,
		Endpoint:     g.LiteLLM.Endpoint,
		APIKeyEnv:    g.LiteLLM.APIKeyEnv,
		APIKeyFile:   g.LiteLLM.APIKeyFile,
		DefaultModel: g.LiteLLM.DefaultModel,
		CABundle:     g.LiteLLM.CABundle,
	}}
}

// ResolveGateway looks up a gateway by name in the resolved list. An empty name
// returns the FIRST resolved gateway (the default), so an inference agent that
// names no specific gateway routes through the default one. Returns nil if no
// gateway matches (or none are configured).
func (g GovernorConfig) ResolveGateway(name string) *GatewayConfig {
	gws := g.ResolvedGateways()
	if len(gws) == 0 {
		return nil
	}
	if name == "" {
		gw := gws[0]
		return &gw
	}
	for i := range gws {
		if strings.EqualFold(gws[i].Name, name) {
			gw := gws[i]
			return &gw
		}
	}
	return nil
}

// ResolveLiteLLMInferenceKey resolves the API key an agent should present when
// its inference routes through the legacy "litellm" backend. It MUST agree with
// the key the entitlement/probe path validates (dashboard gateways.go/cost.go/
// openrouter.go, which use ResolveGateway(name).ResolveAPIKey()) — otherwise a
// key rotation performed via the Model Gateways tab updates only the gateway key
// file, entitlement passes, but inference keeps sending the stale legacy key and
// 401s.
//
// Resolution rule:
//   - When an EXPLICIT `gateways:` block is configured, resolve the key from the
//     gateway matching this backend (its own api_key_file — the file the Model
//     Gateways tab writes), exactly as entitlement does. One source, no drift.
//   - When NO explicit gateways are configured, fall back to the legacy
//     Governor.LiteLLM resolver, which consults the k8s Secret mount and PVC copy
//     in addition to api_key_file. The synthetic gateway from ResolvedGateways
//     lacks that multi-location fallback, so we must not use it here — preserving
//     today's behavior for classic single-`litellm:`-block hives.
//
// backend is the agent's backend name (typically "litellm"); it selects which
// explicit gateway to consult.
func (g GovernorConfig) ResolveLiteLLMInferenceKey(backend string) string {
	if len(g.Gateways) > 0 {
		if gw := g.ResolveGateway(backend); gw != nil {
			return gw.ResolveAPIKey()
		}
	}
	return g.LiteLLM.ResolveAPIKey()
}

// ResolveAPIKey returns this gateway's key value, preferring the env var when
// set and falling back to the file. Returns "" when neither yields a value
// (a keyless endpoint). Mirrors LiteLLMConfig key resolution.
// secretFileRoots are the ONLY directories an api_key_file may live under.
//
//   - /secrets      — the read-only Kubernetes Secret projection
//   - /data/secrets — the PVC-backed dir the dashboard writes UI-entered keys to
//
// Anything else is refused. See SecretFilePathAllowed.
//
// A package var, not a const slice, so tests can point it at a t.TempDir()
// (production never reassigns it). Use SetSecretFileRootsForTest.
var secretFileRoots = []string{"/secrets", WritableSecretsDir}

// SetSecretFileRootsForTest overrides the allowed secret-file roots and returns
// a function restoring the previous value. Intended for tests, which must write
// key files into a t.TempDir() rather than the real /secrets.
//
// This is not a security hole: it is compiled into the binary but never called
// outside tests, and anyone able to call it already has in-process code
// execution — at which point they can read the files directly.
func SetSecretFileRootsForTest(roots ...string) func() {
	prev := secretFileRoots
	secretFileRoots = roots
	return func() { secretFileRoots = prev }
}

// AllowSecretFileRoot registers an ADDITIONAL directory whose files may be read
// as a secret, returning a function that removes it again.
//
// This exists so a component that WRITES key files keeps its write location and
// this package's READ gate in lockstep. pkg/dashboard writes per-gateway keys to
// its own gatewaySecretsDir — a package var tests repoint at a temp dir — and
// without this the two seams disagree: the dashboard writes a key it can then
// never read back. Callers register the same directory they write to, so the
// gate stays a real confinement rather than something each test disables.
func AllowSecretFileRoot(dir string) func() {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return func() {}
	}
	prev := secretFileRoots
	secretFileRoots = append(append([]string(nil), secretFileRoots...), dir)
	return func() { secretFileRoots = prev }
}

// SecretFilePathAllowed reports whether p is a path hive may read a secret from.
//
// SECURITY (audit N8, CWE-200/918): api_key_file is attacker-controllable — the
// gateway upsert stores whatever absolute path it is given, and the save-time
// probe then reads that file and ships its contents to the gateway endpoint as
// an `Authorization: Bearer` header. Without confinement that is an arbitrary
// file read wired directly to an arbitrary outbound request: /data/secrets/*.pem
// (the GitHub App key), /proc/self/environ, token caches.
//
// The check is a prefix test against secretFileRoots, applied AFTER resolving
// symlinks, so a symlink inside an allowed directory cannot point out of it. A
// path that does not exist yet is still validated lexically — the dashboard
// legitimately writes a key file before anything reads it.
func SecretFilePathAllowed(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	clean := filepath.Clean(p)
	// Resolve symlinks when the path (or its parent) exists, so a link planted
	// inside an allowed root cannot escape it. EvalSymlinks fails on a
	// not-yet-created file, in which case the lexical check below still applies.
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = resolved
	} else if resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(clean)); err == nil {
		clean = filepath.Join(resolvedDir, filepath.Base(clean))
	}
	for _, root := range secretFileRoots {
		rootClean := filepath.Clean(root)
		// Resolve the ROOT too. On macOS /var is a symlink to /private/var, so a
		// resolved path under a temp dir would never prefix-match an unresolved
		// root — the comparison has to be symlink-resolved on both sides or it
		// is inconsistent.
		if resolvedRoot, err := filepath.EvalSymlinks(rootClean); err == nil {
			rootClean = resolvedRoot
		}
		if clean == rootClean {
			continue // the directory itself is not a key file
		}
		// The separator suffix keeps "/data/secrets-evil" from matching the
		// "/data/secrets" root.
		if strings.HasPrefix(clean, rootClean+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (gw GatewayConfig) ResolveAPIKey() string {
	if gw.APIKeyEnv != "" {
		if v := os.Getenv(gw.APIKeyEnv); v != "" {
			return v
		}
	}
	if gw.APIKeyFile != "" {
		// SECURITY (audit N8): confine the read to the managed secrets dirs. This
		// gate lives HERE, not only in the HTTP handler, so every caller is
		// covered — including a path that reached hive.yaml some other way (a
		// hand-edited config, an older build, a restored backup).
		if !SecretFilePathAllowed(gw.APIKeyFile) {
			return ""
		}
		if b, err := os.ReadFile(gw.APIKeyFile); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// TrajectoryConfig governs the trajectory-review lane: a periodic,
// second-model check that reads each running agent's recent transcript and
// scores whether the sequence of actions is still working toward the agent's
// assigned intent (its last kick), pausing the agent when it diverges. This
// is defense against trajectory-level goal drift — individually-innocuous
// steps that assemble toward an unauthorized outcome — which action-level
// gating cannot see. On by default; the lane no-ops when no reviewer endpoint
// resolves, so "on" is safe even without inference set up.
//
// The reviewer needs any OpenAI-compatible /v1/chat/completions endpoint — a
// LiteLLM gateway, a vLLM server, or an llm-d front. It does NOT drive an
// interactive CLI: it makes one stateless request per agent per cycle and
// reads back a strict-JSON verdict, which the CLIs (stateful tmux sessions)
// cannot provide.
type TrajectoryConfig struct {
	// Enabled turns the review lane on. Pointer so an omitted key defaults to
	// enabled (applyDefaults sets it), while an explicit "false" disables it.
	Enabled *bool `yaml:"enabled"`
	// Endpoint is the reviewer's OpenAI-compatible base URL (LiteLLM, vLLM, or
	// llm-d — anything serving /v1/chat/completions). Empty → fall back to the
	// governor LiteLLM endpoint. Storing it here lets the reviewer target a
	// cheap local model independent of the agents' inference config.
	Endpoint string `yaml:"endpoint"`
	// APIKeyEnv / APIKeyFile resolve the reviewer endpoint's key (env var NAME
	// or file PATH; never the value). Empty → fall back to the LiteLLM key.
	// A bare vLLM server needs no key; a gateway usually does.
	APIKeyEnv  string `yaml:"api_key_env"`
	APIKeyFile string `yaml:"api_key_file"`
	// IntervalS is how often (seconds) the lane evaluates running agents.
	// It runs off the governor tick, so the effective floor is the governor
	// eval interval; a value below that reviews every tick. 0 → default.
	IntervalS int `yaml:"interval_s"`
	// Model is the reviewer model id. Empty → the governor LiteLLM
	// default_model. A cheap-but-capable model is appropriate; a tiny model
	// will emit weaker verdicts (the lane fails open, so it degrades toward
	// "catches less," not "false-pauses").
	Model string `yaml:"model"`
	// TranscriptLines caps how many trailing transcript lines are sent to the
	// reviewer. 0 → default. Bounds token cost and keeps the review focused
	// on recent behavior.
	TranscriptLines int `yaml:"transcript_lines"`
	// OnDivergence is the action taken when a trajectory is judged divergent:
	// "pause" (stop the agent and alert — default) or "alert" (notify only,
	// leave the agent running). Any other value is treated as "alert".
	OnDivergence string `yaml:"on_divergence"`
	// ExemptAgents are never reviewed (e.g. advisory-only agents that open no
	// PRs and touch no infrastructure).
	ExemptAgents []string `yaml:"exempt_agents"`
}

// ReplanConfig governs the Phase 3 stall-replan lane: a periodic check that
// finds approved plans whose sub-tasks have stopped progressing and re-kicks the
// architect to revise them, bounded by a per-plan replan cap. It runs off the
// governor tick on its own cadence (like the trajectory lane). On by default; it
// is a no-op when there are no approved plans, so "on" is always safe.
type ReplanConfig struct {
	// Enabled turns the stall-replan lane on. Pointer so an omitted key defaults
	// to enabled, while an explicit "false" disables it.
	Enabled *bool `yaml:"enabled"`
	// IntervalS is how often (seconds) the lane scans for stalled plans. It runs
	// off the governor tick, so the effective floor is the governor eval
	// interval. 0 → default (30m).
	IntervalS int `yaml:"interval_s"`
	// StallThresholdS is how long (seconds) a plan may go without any child
	// progressing before it is considered stalled. 0 → default (6h).
	StallThresholdS int `yaml:"stall_threshold_s"`
	// MaxReplans caps replans per plan before the lane stops and escalates to a
	// human. 0 → default (5).
	MaxReplans int `yaml:"max_replans"`
}

// IsEnabled reports whether the stall-replan lane is on. Default is ON: a nil
// Enabled (key omitted) counts as enabled, only an explicit false disables it.
func (r ReplanConfig) IsEnabled() bool {
	return r.Enabled == nil || *r.Enabled
}

// IsEnabled reports whether the trajectory-review lane is on. Default is ON:
// a nil Enabled (key omitted) counts as enabled, only an explicit false
// disables it. The lane itself no-ops when no reviewer endpoint resolves, so
// defaulting on is safe even for hives without inference configured.
func (t TrajectoryConfig) IsEnabled() bool {
	return t.Enabled == nil || *t.Enabled
}

// ResolveReviewer returns the reviewer endpoint, API key, and model, falling
// back to the governor LiteLLM block for any field the trajectory block leaves
// empty. This is what lets the reviewer target a LiteLLM gateway, a vLLM
// server, or an llm-d front interchangeably: it only needs an
// OpenAI-compatible /v1/chat/completions URL. The key value is resolved from
// a file or env var and is never stored in hive.yaml.
func (g *GovernorConfig) ResolveReviewer() (endpoint, apiKey, model string) {
	t := g.Trajectory
	endpoint = strings.TrimRight(strings.TrimSpace(t.Endpoint), "/")
	if endpoint == "" {
		endpoint = g.LiteLLM.ResolveEndpoint()
	}
	apiKey = resolveKeyFromFileThenEnv(t.APIKeyFile, t.APIKeyEnv)
	if apiKey == "" {
		apiKey = g.LiteLLM.ResolveAPIKey()
	}
	model = strings.TrimSpace(t.Model)
	if model == "" {
		model = g.LiteLLM.DefaultModel
	}
	return endpoint, apiKey, model
}

// ReviewerReady reports whether the reviewer has both an endpoint and a model
// resolved — i.e. whether enabling the lane will actually run reviews. The UI
// uses this to distinguish "on and running" from "on but not configured", so
// a safety control never silently no-ops while showing as enabled.
func (g *GovernorConfig) ReviewerReady() bool {
	endpoint, _, model := g.ResolveReviewer()
	return endpoint != "" && model != ""
}

// resolveKeyFromFileThenEnv reads a secret from a file path, then an env var
// NAME, returning "" if neither yields a value. Mirrors the LiteLLM/inference
// resolution order without pulling in defaults.
func resolveKeyFromFileThenEnv(file, env string) string {
	if file != "" {
		if data, err := os.ReadFile(file); err == nil {
			if k := strings.TrimSpace(string(data)); k != "" {
				return k
			}
		}
	}
	if env != "" {
		if k := os.Getenv(env); k != "" {
			return k
		}
	}
	return ""
}

// Discovery-auth defaults for the self-hosted inference backends. Like
// LiteLLM, hive.yaml stores only the env var NAME and/or key FILE PATH —
// never the key value itself (Config.Save() writes the expanded config
// back to disk, so a key value in YAML would be persisted in plaintext).
const (
	// DefaultVLLMAPIKeyEnv is the env var consulted for the vLLM model
	// discovery API key when governor.vllm.api_key_env is not set.
	DefaultVLLMAPIKeyEnv = "HIVE_VLLM_API_KEY"
	// DefaultLLMDAPIKeyEnv is the env var consulted for the llm-d model
	// discovery API key when governor.llm-d.api_key_env is not set.
	DefaultLLMDAPIKeyEnv = "HIVE_LLMD_API_KEY"
)

// InferenceAuthConfig holds optional /v1/models discovery auth for a
// self-hosted inference backend (vllm, llm-d). Plain vLLM/llm-d servers
// need no key, but the configured endpoint may actually be a LiteLLM
// gateway, which entitlement-filters /v1/models per API key and hides
// key-gated models from anonymous callers.
type InferenceAuthConfig struct {
	APIKeyHeader string `yaml:"api_key_header,omitempty" json:"api_key_header,omitempty"` // header NAME the key is sent in (default "Authorization")
	APIKeyEnv    string `yaml:"api_key_env" json:"api_key_env"`                           // env var NAME holding the key; never the key value
	APIKeyFile   string `yaml:"api_key_file" json:"api_key_file"`                         // path to a file holding the key
	Endpoint     string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`             // optional endpoint override for this backend
}

// ResolveAPIKey returns the backend's discovery API key using the
// resolution order: key file (api_key_file) → env var named by
// api_key_env → defaultEnv. Returns "" when no key is configured. The key
// value itself is never stored in hive.yaml.
func (c *InferenceAuthConfig) ResolveAPIKey(defaultEnv string) string {
	// SECURITY (audit N8): same confinement as GatewayConfig.ResolveAPIKey — an
	// api_key_file is only ever read from the managed secrets dirs.
	if c.APIKeyFile != "" && SecretFilePathAllowed(c.APIKeyFile) {
		if data, err := os.ReadFile(c.APIKeyFile); err == nil {
			if key := strings.TrimSpace(string(data)); key != "" {
				return key
			}
		}
	}
	if c.APIKeyEnv != "" {
		if key := os.Getenv(c.APIKeyEnv); key != "" {
			return key
		}
	}
	if defaultEnv != "" {
		return os.Getenv(defaultEnv)
	}
	return ""
}

type LoggingConfig struct {
	Dir        string `yaml:"dir"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxAgeDays int    `yaml:"max_age_days"`
	MaxBackups int    `yaml:"max_backups"`
	Compress   bool   `yaml:"compress"`
	Level      string `yaml:"level"`
}

// LiteLLM key/endpoint resolution defaults. hive.yaml stores only the env
// var NAME and/or key FILE PATH — never the key value itself. Config.Save()
// writes the expanded config back to disk, so a key value stored in YAML
// would be baked into the file in plaintext.
const (
	// DefaultLiteLLMAPIKeyEnv is the env var consulted for the LiteLLM API
	// key when api_key_env is not set in hive.yaml.
	DefaultLiteLLMAPIKeyEnv = "HIVE_LITELLM_API_KEY"
	// DefaultLiteLLMAPIKeyFile is the key file consulted when api_key_file
	// is not set. Matches the /secrets volume used for k8s Secret mounts.
	DefaultLiteLLMAPIKeyFile = "/secrets/litellm_api_key"
	// WritableSecretsDir is the PVC-backed directory where the dashboard
	// persists secret VALUES entered in the UI. Unlike /secrets (a
	// read-only Kubernetes Secret mount), /data is the hive's writable
	// persistent volume, so files written here survive pod restarts and
	// hosted users can set keys without cluster access.
	WritableSecretsDir = "/data/secrets"
	// WritableLiteLLMAPIKeyFile is where the dashboard stores an API key
	// value entered in the LiteLLM config UI. hive.yaml references it via
	// api_key_file; the key value itself never enters hive.yaml or logs.
	WritableLiteLLMAPIKeyFile = WritableSecretsDir + "/litellm_api_key"
	// LiteLLMEndpointEnv overrides governor.litellm.endpoint at runtime
	// (mirrors HIVE_VLLM_ENDPOINT / HIVE_LLMD_ENDPOINT).
	LiteLLMEndpointEnv = "HIVE_LITELLM_ENDPOINT"
)

// Bob (IBM bobshell) API-key resolution. bobshell cannot authenticate in a pod
// any other way: its default flow (W3ID SSO) opens a browser and polls a
// localhost callback port, which in a headless container cannot succeed and
// instead burns a 3-minute timeout per launch. IBM's documented remedy for
// non-interactive sessions is API-key auth. As with LiteLLM, hive.yaml stores
// only the env var NAME and/or key FILE PATH — never the key value, because
// Config.Save() round-trips the config back to disk.
const (
	// BobAPIKeyEnvVar is the env var name bobshell itself reads the key from.
	// Verified against the installed bundle (bobshell 1.0.6 bundle/bob.js):
	// `process.env.BOBSHELL_API_KEY`. This is the name injected INTO the
	// agent's environment, distinct from the hive-side variable below that
	// an operator sets on the hive pod.
	BobAPIKeyEnvVar = "BOBSHELL_API_KEY"

	// DefaultBobAPIKeyEnv is the hive-side env var consulted for the bob API
	// key when governor.bob.api_key_env is not set in hive.yaml.
	DefaultBobAPIKeyEnv = "HIVE_BOB_API_KEY"

	// DefaultBobAPIKeyFile is the key file consulted when api_key_file is not
	// set. Matches the read-only /secrets volume used for k8s Secret mounts.
	DefaultBobAPIKeyFile = "/secrets/bob_api_key"

	// WritableBobAPIKeyFile is the PVC-backed location a hosted operator can
	// write the key to without cluster access, mirroring
	// WritableLiteLLMAPIKeyFile. Referenced by path only; never by value.
	WritableBobAPIKeyFile = WritableSecretsDir + "/bob_api_key"

	// BobAuthTypeEnvVar is the env var bobshell reads to pick its auth type.
	// It is only a FALLBACK DEFAULT, not an override. From bundle/bob.js
	// (bobshell 1.0.6), the auth-dialog preselect is:
	//   let l=null,d=process.env.BOBSHELL_DEFAULT_AUTH_TYPE;
	//   d&&Object.values(fr).includes(d)&&(l=d);
	//   let c=a.findIndex(p=>e.merged.security?.auth?.selectedType
	//         ? p.value===e.merged.security.auth.selectedType
	//         : l ? p.value===l : ...)
	// i.e. a persisted security.auth.selectedType WINS over this env var.
	// An invalid value is a hard error ("Invalid value for
	// BOBSHELL_DEFAULT_AUTH_TYPE"), so it must be one of the `fr` enum values.
	BobAuthTypeEnvVar = "BOBSHELL_DEFAULT_AUTH_TYPE"

	// BobAuthTypeAPIKey selects API-key auth. It is bob's own enum constant
	// `USE_BOBSHELL="api-key"` from bundle/bob.js (the sibling value being
	// `W3ID_SSO="sso"`), so it is dictated by the vendor, not by us. Not a
	// secret — it is the literal string "api-key" and carries no credential.
	BobAuthTypeAPIKey = "api-key"

	// BobAuthMethodFlag is bobshell's `--auth-method` CLI flag. It EXISTS in
	// bobshell 1.0.6 — an earlier fix removed it after concluding from
	// `bob --help` that it did not. That conclusion was wrong: bundle/bob.js
	// registers it and then HIDES it from help output:
	//   t.option("auth-method",{type:"string",...,choices:[fr.W3ID_SSO,fr.USE_BOBSHELL]});
	//   let a=[...,"auth-method"]; a.forEach(c=>t.hide(c));
	// so it is functional but invisible to `--help | grep auth-method`.
	//
	// It is the STRONGEST control available, because it is the only input that
	// beats the persisted settings file. bob stores it as
	// globalThis.authMethodByCliArg, and the settings-normalization step reads:
	//   let n=globalThis.authMethodByCliArg||t.merged.security.auth.selectedType||r;
	// — the CLI arg is consulted FIRST, ahead of the persisted selectedType.
	// It also suppresses the write-back that would otherwise persist a
	// different value (`&&!globalThis.authMethodByCliArg`), so passing it
	// makes hive's choice authoritative without bob rewriting the shared file.
	BobAuthMethodFlag = "--auth-method"

	// BobApprovalModeFlag / BobApprovalModeYolo set bob's tool-approval policy.
	// Verified in bobshell 1.0.6 `bob --help`:
	//   --approval-mode  Set the approval mode: default (prompt for approval),
	//                    auto_edit (auto-approve edit tools), yolo
	//                    (auto-approve all tools)
	//                    [string] [choices: "default", "auto_edit", "yolo"]
	//
	// Without it bob defaults to "default" and the TUI shows
	// `Auto-approve: Off`, so an unattended agent blocks forever on the first
	// tool call — no human is attached to answer the prompt. With
	// `--approval-mode yolo` the TUI shows `Auto-approve: Full` and bob
	// executes shell/edit tools unattended (verified live on a spoke: bob
	// wrote /tmp/_tool.txt with no approval prompt).
	//
	// The named flag is preferred over the equivalent `-y`/`--yolo` boolean
	// because it states the mode at the call site.
	BobApprovalModeFlag = "--approval-mode"
	BobApprovalModeYolo = "yolo"

	// BobTrustFlag marks the agent's workspace as trusted. bobshell 1.0.6
	// otherwise renders "This folder is not trusted. Some features may be
	// disabled." and gates tool availability behind that state. Verified in
	// `bob --help`:
	//   --trust  specify trust level for the current workspace
	//
	// Passing the flag is preferred over seeding $HOME/.bob/trustedFolders.json
	// because the flag is per-launch and stateless: it needs no knowledge of
	// bob's on-disk trust schema, cannot drift when that schema changes, and
	// applies to whatever workdir the agent is launched in. The shared
	// /data/home is used by EVERY bob agent on a hive, so a seeded trust file
	// would also be a fleet-wide mutation of the kind that already caused the
	// selectedType incident (see BobAuthTypeEnvVar).
	BobTrustFlag = "--trust"

	// BobSettingsRelPath is the persisted settings file, relative to $HOME.
	// From bundle/bob.js: `as=".bob"` and
	// `getGlobalSettingsPath(){return fu.join(t.getGlobalGeminiDir(),"settings.json")}`
	// where getGlobalGeminiDir() is `path.join(os.homedir(), as)`.
	// On a hive this resolves to /data/home/.bob/settings.json — a SHARED file,
	// so one agent picking SSO at the prompt re-breaks every other bob agent.
	BobSettingsRelPath = ".bob/settings.json"

	// BobSettingsAuthKey / BobSettingsSelectedTypeKey / BobSettingsEnforcedTypeKey
	// are the nested JSON keys hive owns inside that file. Shape per bundle:
	//   e.merged.security?.auth?.selectedType   // which method is chosen
	//   e.merged.security?.auth?.enforcedType   // FILTERS the option list:
	//     e.merged.security?.auth?.enforcedType && (a=a.filter(p=>p.value===...))
	//   and validation: eEr() errors when enforcedType !== the active type.
	// Setting enforcedType to api-key leaves SSO unselectable, so a stray
	// interactive pick cannot re-break the fleet.
	BobSettingsSecurityKey     = "security"
	BobSettingsAuthKey         = "auth"
	BobSettingsSelectedTypeKey = "selectedType"
	BobSettingsEnforcedTypeKey = "enforcedType"

	// BobSettingsFileMode keeps the shared settings file group-writable: it
	// lives in /data/home/.bob (drwxrwx--- dev:node) and bob runs as agent UIDs
	// in group `node`, which must still be able to write sibling state. Making
	// it read-only is deliberately NOT done — bob calls setValue() on this file
	// during normal startup normalization, and an EACCES there is an unhandled
	// write path, so re-assertion on every launch is the safer self-heal.
	BobSettingsFileMode = 0o664

	// BobSettingsDirMode matches the observed /data/home/.bob (drwxrwx---).
	BobSettingsDirMode = 0o770

	// BobStateDirName is the per-workspace state directory bob creates inside
	// the directory it is launched in (in addition to the shared $HOME/.bob).
	// Its .bob-errors/ subdirectory is the logger target behind bob's
	// "Failed to initialize logger:" message, observed in production at
	// /data/agents/<name>/.bob/.bob-errors/errors-YYYY-MM-DD.log.
	BobStateDirName = ".bob"
)

// BobConfig configures the IBM bobshell ("bob") CLI backend. Only the
// key's LOCATION is stored — never the key itself.
type BobConfig struct {
	APIKeyEnv  string `yaml:"api_key_env" json:"api_key_env,omitempty"`   // env var NAME holding the key; default HIVE_BOB_API_KEY
	APIKeyFile string `yaml:"api_key_file" json:"api_key_file,omitempty"` // path to a file holding the key; default /secrets/bob_api_key
	// KeyName is an optional, human-chosen LABEL for the configured key
	// ("Team inference key", "andy personal", …). It is safe-to-show metadata,
	// NOT a secret — it records WHICH key a hive is set to use so managers can
	// tell keys apart without ever seeing the value (#3596/#3598). omitempty
	// keeps hive.yaml byte-identical on round-trip when unset; an absent name
	// is a normal, backwards-compatible state that the dashboard renders as
	// "(unnamed)" rather than an error.
	KeyName string `yaml:"key_name,omitempty" json:"key_name,omitempty"`
}

// ResolveAPIKey returns the bob API key, or "" when none is configured.
// Key FILES are consulted in priority order — the configured api_key_file,
// then the k8s Secret mount (DefaultBobAPIKeyFile), then the PVC file
// (WritableBobAPIKeyFile) — followed by the env var named by api_key_env and
// finally DefaultBobAPIKeyEnv. This mirrors LiteLLMConfig.ResolveAPIKey so a
// key stays working if hive.yaml is re-seeded and the api_key_file pointer is
// lost, or if the PVC copy is wiped but an admin Secret exists.
func (c *BobConfig) ResolveAPIKey() string {
	key, _ := c.resolveAPIKeyWithSource()
	return key
}

// ResolveAPIKeySource reports WHERE the key was found without exposing the
// value: "file:<path>", "env:<NAME>", or "" when unconfigured. Safe to log
// and safe to return from APIs.
func (c *BobConfig) ResolveAPIKeySource() string {
	_, source := c.resolveAPIKeyWithSource()
	return source
}

func (c *BobConfig) resolveAPIKeyWithSource() (string, string) {
	if c == nil {
		return "", ""
	}
	files := []string{c.APIKeyFile, DefaultBobAPIKeyFile, WritableBobAPIKeyFile}
	seen := map[string]bool{"": true}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		if data, err := os.ReadFile(f); err == nil {
			if key := strings.TrimSpace(string(data)); key != "" {
				return key, "file:" + f
			}
		}
	}
	// TrimSpace the env-var sources too, matching the file branch above. bob
	// does NO trimming of its own — bundle/bob.js reads
	// `process.env.BOBSHELL_API_KEY` verbatim into the Authorization header —
	// and hive delivers the value through `tmux set-environment`, which passes
	// it as a raw exec argument with no shell word-splitting to strip a stray
	// newline. So any trailing whitespace here reaches IBM's API inside the
	// header and 401s, which bob surfaces as a fallback to the SSO flow rather
	// than as an auth error. Cheap to normalize; impossible to debug from the
	// symptom.
	if c.APIKeyEnv != "" {
		if key := strings.TrimSpace(os.Getenv(c.APIKeyEnv)); key != "" {
			return key, "env:" + c.APIKeyEnv
		}
	}
	if key := strings.TrimSpace(os.Getenv(DefaultBobAPIKeyEnv)); key != "" {
		return key, "env:" + DefaultBobAPIKeyEnv
	}
	return "", ""
}

// LiteLLMConfig configures the litellm inference backend: an OpenAI-compatible
// LiteLLM proxy (remote or local) that agents reach through the hive's
// inference translator.
type LiteLLMConfig struct {
	Endpoint     string `yaml:"endpoint"`      // base URL, e.g. https://litellm.example.com
	APIKeyEnv    string `yaml:"api_key_env"`   // env var NAME holding the key; default HIVE_LITELLM_API_KEY
	APIKeyFile   string `yaml:"api_key_file"`  // path to a file holding the key; default /secrets/litellm_api_key
	DefaultModel string `yaml:"default_model"` // model used when an agent has none selected
	CABundle     string `yaml:"ca_bundle"`     // optional PEM path for a private CA (never disables verification)
	LocalProxy   bool   `yaml:"local_proxy"`   // run the bundled litellm binary as a local translator fallback
}

// ResolveAPIKey returns the LiteLLM API key. Key FILES are consulted in
// priority order — the configured api_key_file, then the k8s Secret mount
// (DefaultLiteLLMAPIKeyFile), then the dashboard-written PVC file
// (WritableLiteLLMAPIKeyFile) — followed by the env var named by
// api_key_env and finally DefaultLiteLLMAPIKeyEnv. Returns "" when no key
// is configured. The key value itself is never stored in hive.yaml.
//
// Consulting all three file locations means a key saved via the dashboard
// keeps working even if hive.yaml is reset (e.g. re-seeded from a
// ConfigMap) and the api_key_file pointer is lost, and an admin-managed
// Secret key keeps working if the PVC copy is wiped.
func (c *LiteLLMConfig) ResolveAPIKey() string {
	key, _ := c.resolveAPIKeyWithSource()
	return key
}

// ResolveAPIKeySource reports where ResolveAPIKey found the key without
// exposing the value: "file:<path>", "env:<NAME>", or "" when no key is
// configured. Safe to return from APIs (the dashboard shows it as the
// "Key detected" store).
func (c *LiteLLMConfig) ResolveAPIKeySource() string {
	_, source := c.resolveAPIKeyWithSource()
	return source
}

func (c *LiteLLMConfig) resolveAPIKeyWithSource() (string, string) {
	files := []string{c.APIKeyFile, DefaultLiteLLMAPIKeyFile, WritableLiteLLMAPIKeyFile}
	seen := map[string]bool{"": true}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		// SECURITY (audit N8): only the managed secrets dirs. The two defaults
		// appended above already satisfy this; the gate is for c.APIKeyFile,
		// which is operator/API-supplied.
		if !SecretFilePathAllowed(f) {
			continue
		}
		if data, err := os.ReadFile(f); err == nil {
			if key := strings.TrimSpace(string(data)); key != "" {
				return key, "file:" + f
			}
		}
	}
	if c.APIKeyEnv != "" {
		if key := os.Getenv(c.APIKeyEnv); key != "" {
			return key, "env:" + c.APIKeyEnv
		}
	}
	if key := os.Getenv(DefaultLiteLLMAPIKeyEnv); key != "" {
		return key, "env:" + DefaultLiteLLMAPIKeyEnv
	}
	return "", ""
}

// ResolveEndpoint returns the effective LiteLLM base URL: the
// HIVE_LITELLM_ENDPOINT env var when set, otherwise the YAML endpoint.
func (c *LiteLLMConfig) ResolveEndpoint() string {
	if ep := os.Getenv(LiteLLMEndpointEnv); ep != "" {
		return ep
	}
	return c.Endpoint
}

// Validate checks that the configured endpoint (when set) parses as an
// absolute http(s) URL.
func (c *LiteLLMConfig) Validate() error {
	if c.Endpoint == "" {
		return nil
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return fmt.Errorf("governor.litellm.endpoint %q is not a valid URL: %w", c.Endpoint, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("governor.litellm.endpoint %q must be an absolute http(s) URL", c.Endpoint)
	}
	return nil
}

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

type HealthConfig struct {
	HealthcheckInterval int  `yaml:"healthcheck_interval"`
	RestartCooldown     int  `yaml:"restart_cooldown"`
	ModelLock           bool `yaml:"model_lock"`
}

type BudgetConfig struct {
	TotalTokens int64 `yaml:"total_tokens"`
	PeriodDays  int   `yaml:"period_days"`
	CriticalPct int   `yaml:"critical_pct"`
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

type GitHubConfig struct {
	AppID              int64  `yaml:"app_id"`
	InstallationID     int64  `yaml:"installation_id"`
	DocsInstallationID int64  `yaml:"docs_installation_id"`
	KeyFile            string `yaml:"key_file"`
	Token              string `yaml:"token"`
	OAuthClientID      string `yaml:"oauth_client_id"`
	// Forge_ names the GitHub instance this hive's App and repos live on, as a
	// bare host: "github.com" or "github.ibm.com". It is the SINGLE
	// AUTHORITATIVE identity field — app_id, app_slug, api_url and base_url are
	// all derivable from it (see Forge, ResolvedAppID, forgeIdentities).
	//
	// Read it through the Forge() method, never directly: Forge() applies the
	// dual-read fallback that keeps a not-yet-backfilled spoke resolving exactly
	// as it does today. The trailing underscore exists solely because the method
	// owns the good name; the YAML key is a clean `forge`.
	//
	// Empty is valid during the migration window and means "infer from the URL
	// fields". Once the fleet is backfilled it becomes mandatory — per the
	// owner: explicitly defined on every spoke on every cluster, no empties, no
	// inheritance. The implicit empty is what disguised the 2026-07-31 damage.
	Forge_ string `yaml:"forge,omitempty"`
	// AppSlug is the GitHub App URL slug for the install link.
	// For public GitHub: "kubestellar-hive". For GHE: your app's slug.
	AppSlug string `yaml:"app_slug"`
	// APIURL is the GitHub API base URL. Defaults to DefaultGitHubAPIURL.
	// For GitHub Enterprise, set to e.g. "https://github.ibm.com/api/v3".
	APIURL string `yaml:"api_url"`
	// BaseURL is the GitHub web base URL. Defaults to DefaultGitHubBaseURL.
	// For GitHub Enterprise, set to e.g. "https://github.ibm.com".
	BaseURL string `yaml:"base_url"`
	// AppAuthoredPRs controls App-bot authorship: when enabled AND ai_author is
	// empty, agents author PRs/commits as the GitHub App bot ("<slug>[bot]") via
	// the App installation token, instead of the Copilot-login user.
	//
	// It is a *bool so absent (nil) is distinct from an explicit false. Default is
	// ON (see AppAuthoredPRsEnabled): App-bot authorship is now the fleet norm, so
	// a hive that says nothing gets it. Set `app_authored_prs: false` to opt a
	// specific hive OUT. This only affects write-capable hives — the token is
	// injected solely for agents whose tier CanPush() and only when a real App is
	// installed (m.appAuth != nil), so an App-less or advisory hive is unaffected.
	AppAuthoredPRs *bool `yaml:"app_authored_prs,omitempty"`
	// OAuthBaseURLOverride and OAuthAPIURLOverride redirect the DEVICE-FLOW LOGIN
	// endpoints away from public github.com. They are a TEST SEAM ONLY — see the
	// comment on OAuthBaseURL(). They carry the `-` yaml tag so they can never be
	// set from a hive.yaml, on the hub, or over the config API: only Go code in a
	// test can assign them.
	OAuthBaseURLOverride string `yaml:"-"`
	OAuthAPIURLOverride  string `yaml:"-"`
}

// PlaceholderAppID is the sentinel `github.app_id` written into a hive's config
// when the hive is provisioned BEFORE its real GitHub App exists — a pooled
// "available-*" placeholder, or a hive whose owner has not installed the App
// yet. Config validation requires either github.token or github.app_id, so the
// seed cannot leave app_id empty; this value fills the slot without ever naming
// a real App.
//
// It is NOT a valid GitHub App ID and must never be handed to
// github.NewAppAuth. Every test for "is a GitHub App configured?" must use
// GitHubConfig.HasApp() rather than a bare `AppID != 0` — the sentinel is
// non-zero, so a bare zero-test reports a placeholder as a real App. That is
// exactly the bug that put hosted hives into permanent CrashLoopBackOff: the
// moment installation_id became non-zero (the owner installed the App), the
// spoke committed to App auth, failed to read a PEM that was never provisioned,
// and exited before the HTTP listener bound — invisible from the dashboard.
const PlaceholderAppID int64 = 999999999

const (
	// DefaultGitHubAPIURL is the default GitHub API endpoint (public github.com).
	DefaultGitHubAPIURL = "https://api.github.com"
	// DefaultGitHubBaseURL is the default GitHub web URL (public github.com).
	DefaultGitHubBaseURL = "https://github.com"
	// DefaultGitHubAppSlug is the public Hive GitHub App slug.
	DefaultGitHubAppSlug = "kubestellar-hive"
	// DefaultOAuthClientID is the PUBLIC github.com Hive App client ID used for
	// device-flow login. Login is always github.com (see OAuthBaseURL), so this
	// is the correct client for every hive — including GHE hives, whose users
	// still sign in with a github.com identity. No secret; safe to hardcode.
	DefaultOAuthClientID = "Ov23ligE2p0gjXg6xAUf"
)

// GitLabConfig configures the GitLab forge (used when project.forge == "gitlab").
// It is entirely additive: a config that never mentions gitlab is unaffected,
// and the zero value points at public gitlab.com.
//
// The token itself is NOT stored here. Following Hive's no-hardcoded-secrets
// rule, TokenEnv names the environment variable to read the GitLab access token
// from at runtime (default GITLAB_TOKEN); the secret is never written to config.
type GitLabConfig struct {
	// URL is the GitLab instance root, e.g. "https://gitlab.com" (default) or a
	// self-managed instance "https://gitlab.example.com". The "/api/v4" path is
	// appended by the client.
	URL string `yaml:"gitlab_url,omitempty"`
	// TokenEnv is the environment variable holding the GitLab access token.
	// Defaults to DefaultGitLabTokenEnv. The token value is resolved via
	// os.Getenv at runtime and never persisted in config.
	TokenEnv string `yaml:"token_env,omitempty"`
}

const (
	// DefaultGitLabURL is the public GitLab SaaS instance root.
	DefaultGitLabURL = "https://gitlab.com"
	// DefaultGitLabTokenEnv is the default env var name for the GitLab token.
	DefaultGitLabTokenEnv = "GITLAB_TOKEN"
)

// InstanceURL returns the configured GitLab instance root, defaulting to
// DefaultGitLabURL when unset.
func (g GitLabConfig) InstanceURL() string {
	if g.URL == "" {
		return DefaultGitLabURL
	}
	return g.URL
}

// TokenEnvName returns the env var name to read the GitLab token from,
// defaulting to DefaultGitLabTokenEnv when unset.
func (g GitLabConfig) TokenEnvName() string {
	if g.TokenEnv == "" {
		return DefaultGitLabTokenEnv
	}
	return g.TokenEnv
}

// GiteaConfig configures the Gitea/Forgejo forge (used when project.forge ==
// "gitea"). Like GitLabConfig it is entirely additive: a config that never
// mentions gitea is unaffected.
//
// Gitea has no single public SaaS host, so URL has no default and must be set
// when the Gitea forge is selected. Following Hive's no-hardcoded-secrets rule,
// the token itself is NOT stored here: TokenEnv names the environment variable
// to read the Gitea access token from at runtime (default GITEA_TOKEN).
type GiteaConfig struct {
	// URL is the Gitea/Forgejo instance root, e.g. "https://codeberg.org" or a
	// self-managed "https://gitea.example.com". The "/api/v1" path is appended by
	// the client. There is no default; it must be supplied for the Gitea forge.
	URL string `yaml:"gitea_url,omitempty"`
	// TokenEnv is the environment variable holding the Gitea access token.
	// Defaults to DefaultGiteaTokenEnv. The token value is resolved via
	// os.Getenv at runtime and never persisted in config.
	TokenEnv string `yaml:"token_env,omitempty"`
}

const (
	// DefaultGiteaTokenEnv is the default env var name for the Gitea token.
	DefaultGiteaTokenEnv = "GITEA_TOKEN"
)

// InstanceURL returns the configured Gitea instance root. Unlike GitLab there is
// no public default, so this returns whatever was configured (possibly empty);
// selecting the Gitea forge with an empty URL is a configuration error surfaced
// at client construction.
func (g GiteaConfig) InstanceURL() string {
	return g.URL
}

// TokenEnvName returns the env var name to read the Gitea token from, defaulting
// to DefaultGiteaTokenEnv when unset.
func (g GiteaConfig) TokenEnvName() string {
	if g.TokenEnv == "" {
		return DefaultGiteaTokenEnv
	}
	return g.TokenEnv
}

// NOTE: two independent "forge" concerns coexist in this file and do NOT collide:
//
//   - The SPOKE FORGE SELECTOR (above): ForgeGitHub/ForgeGitLab/ForgeGitea and
//     ProjectConfig.ForgeKind() choose which forge FAMILY (GitHub, GitLab, Gitea)
//     a spoke executes against, selecting the pkg/forge adapter.
//   - The FORGE IDENTITY system (below): ForgePublic/ForgeGHEIBM, forgeIdentity
//     and GitHubConfig.Forge() derive which GitHub INSTANCE (github.com vs a
//     GitHub Enterprise host) a hive's App/repos live on.
//
// Different names, different receivers (ProjectConfig vs GitHubConfig), different
// values ("github"/"gitlab"/"gitea" vs "github.com"/"github.ibm.com"). Both are
// retained verbatim; nothing is declared twice.

// ─────────────────────────────────────────────────────────────────────────
// FORGE IDENTITY CONSTANTS
//
// A "forge" is the GitHub instance a hive's App and repos live on. Every
// identity value below is derived from the forge — never written independently
// — so the mixed states that broke seven hives on 2026-07-31 (a GHE app_id
// beside an empty, therefore-public api_url) cannot be constructed.
//
// These are the SPOKE-side well-known defaults. The hub carries the
// authoritative per-cluster registration in clusters.json, which may name a
// different App on either forge; these values are what a spoke falls back to
// when the hub has pushed nothing.
// ─────────────────────────────────────────────────────────────────────────
const (
	// ForgePublic is the canonical host label for public github.com. It is a
	// HOSTNAME, not a URL and not a sentinel word — unlike the hub's legacy
	// "public" request sentinel, it is directly comparable to HostLabel().
	ForgePublic = "github.com"

	// ForgeGHEIBM is the IBM GitHub Enterprise host. It is the only GHE forge
	// the fleet currently runs on; naming it as a constant keeps the derivation
	// table literal-free and makes a second GHE forge a one-line addition.
	ForgeGHEIBM = "github.ibm.com"
)

// forgeIdentity is the complete, indivisible identity set for one forge. The
// four fields move together or not at all — that atomicity is the entire point
// of deriving them from a single `forge` value.
type forgeIdentity struct {
	AppID   int64
	AppSlug string
	APIURL  string
	BaseURL string
}

// Forge identity values.
//
// PublicGitHubAppID/PublicGitHubAppSlug and the Enterprise* set are declared
// once, in provenance.go, alongside the rules that validate them — a second
// declaration of the same literal is exactly the drift those rules exist to
// catch. The GHEIBM* names below are aliases so the derivation table reads in
// forge terms without duplicating any value.
const (
	// GHEIBMAppID is the github.ibm.com Hive App's numeric ID.
	GHEIBMAppID = EnterpriseGitHubAppID
	// GHEIBMAppSlug is the github.ibm.com Hive App's URL slug. A GHE instance
	// hosts its own App registration and it is NOT named "kubestellar-hive";
	// falling back to the public slug builds an install link that 404s.
	GHEIBMAppSlug = EnterpriseGitHubAppSlug
	// GHEIBMBaseURL is the github.ibm.com web base URL.
	GHEIBMBaseURL = EnterpriseGitHubBaseURL
	// GHEIBMAPIURL is the github.ibm.com REST API root. GHE serves its API
	// under /api/v3 rather than on a separate api.* hostname.
	GHEIBMAPIURL = EnterpriseGitHubAPIURL
)

// forgeIdentities is the derivation table: forge host -> its complete identity
// set. This is the single source of truth that replaces four independently
// writable fields. Adding a forge means adding a row here; there is no way to
// express a half-identity.
var forgeIdentities = map[string]forgeIdentity{
	ForgePublic: {
		AppID:   PublicGitHubAppID,
		AppSlug: PublicGitHubAppSlug,
		// Empty, not DefaultGitHubAPIURL/DefaultGitHubBaseURL: the public forge's
		// canonical on-disk representation has ALWAYS been the empty string, and
		// ResolvedAPIURL/ResolvedBaseURL already map empty -> the public default.
		// Storing the URLs literally here would make the migration a visible
		// rewrite of every healthy public spoke's config rather than a no-op.
		APIURL:  "",
		BaseURL: "",
	},
	ForgeGHEIBM: {
		AppID:   GHEIBMAppID,
		AppSlug: GHEIBMAppSlug,
		APIURL:  GHEIBMAPIURL,
		BaseURL: GHEIBMBaseURL,
	},
}

// KnownForge reports whether host names a forge with a known identity set.
// An unknown forge is not an error — a hive may legitimately run against a
// third GHE instance the table does not name — but its identity cannot be
// DERIVED, so explicit fields remain load-bearing there.
func KnownForge(host string) bool {
	_, ok := forgeIdentities[normalizeForgeHost(host)]
	return ok
}

// normalizeForgeHost reduces any spelling of a forge — bare host, web URL, API
// URL, mixed case, trailing slash — to its canonical bare-host label.
func normalizeForgeHost(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimSuffix(strings.Trim(s, "/"), "/api/v3")
	host := strings.SplitN(s, "/", 2)[0]
	if host == "" || host == "api.github.com" {
		return ForgePublic
	}
	return host
}

// Forge returns the forge this hive's App and repos live on, as a bare host
// label ("github.com", "github.ibm.com").
//
// PRECEDENCE — and this is the whole design in three lines:
//
//  1. An explicit `forge:` wins. It is the single authoritative field.
//  2. Otherwise infer from the legacy URL fields via HostLabel()/IsGHE().
//  3. Empty everything means public github.com, as it always has.
//
// Step 2 is the DUAL-READ WINDOW. Until every spoke carries `forge:`, a spoke
// that has not been backfilled must resolve exactly as it does today — so this
// never returns anything a pre-migration hive would not already have resolved.
func (g GitHubConfig) Forge() string {
	if f := strings.TrimSpace(g.Forge_); f != "" {
		return normalizeForgeHost(f)
	}
	return normalizeForgeHost(g.HostLabel())
}

// ResolvedAppID returns the GitHub App ID for this hive, completing the
// resolver set alongside ResolvedAPIURL/ResolvedBaseURL/ResolvedAppSlug.
//
// An explicitly configured app_id wins — including the placeholder sentinel,
// which callers detect with IsPlaceholderApp() and must keep seeing. Only when
// app_id is unset is it derived from the forge, which is what lets a hive be
// described by `forge:` alone.
func (g GitHubConfig) ResolvedAppID() int64 {
	if g.AppID != 0 {
		return g.AppID
	}
	if id, ok := forgeIdentities[g.Forge()]; ok {
		return id.AppID
	}
	return 0
}

// ForgeIdentityMismatches reports every identity field that CONTRADICTS this
// hive's forge. Empty means the identity set is coherent.
//
// This is the check that would have caught the 2026-07-31 incident at the
// write boundary. It differs from IdentitySetIssues in the way that matters:
// IdentitySetIssues compares the four fields against EACH OTHER, so it is blind
// whenever the pushed value is the only GHE-looking field present (a GHE app_id
// beside a public slug and empty URLs produces no disagreement at all — verified).
// This compares each field against the DECLARED forge, which is an external
// anchor the pusher does not control.
//
// A field that is empty is not a mismatch: empty means "derive me".
func (g GitHubConfig) ForgeIdentityMismatches() []string {
	forge := g.Forge()
	want, known := forgeIdentities[forge]
	if !known {
		// A forge outside the table has no derivable identity to compare
		// against. Reporting mismatches here would flag every legitimate
		// third-forge hive; saying nothing keeps explicit fields authoritative
		// exactly where they still need to be.
		return nil
	}
	var out []string
	// app_id is compared only when a REAL App is configured. The placeholder
	// sentinel means "no App yet" and legitimately appears on both forges.
	if g.AppID != 0 && !g.IsPlaceholderApp() && g.AppID != want.AppID {
		out = append(out, fmt.Sprintf(
			"app_id %d does not belong to forge %s (expected %d) — an App ID from another forge returns 404 Integration not found on token creation",
			g.AppID, forge, want.AppID))
	}
	if s := strings.TrimSpace(g.AppSlug); s != "" && !strings.EqualFold(s, want.AppSlug) {
		out = append(out, fmt.Sprintf(
			"app_slug %q does not belong to forge %s (expected %q) — the App install link would 404",
			s, forge, want.AppSlug))
	}
	if u := strings.TrimSpace(g.APIURL); u != "" && normalizeForgeHost(u) != forge {
		out = append(out, fmt.Sprintf(
			"api_url %q names forge %s, not %s", u, normalizeForgeHost(u), forge))
	}
	if u := strings.TrimSpace(g.BaseURL); u != "" && normalizeForgeHost(u) != forge {
		out = append(out, fmt.Sprintf(
			"base_url %q names forge %s, not %s", u, normalizeForgeHost(u), forge))
	}
	return out
}

// IsPlaceholderApp reports whether app_id is the "no real App yet" sentinel.
func (g GitHubConfig) IsPlaceholderApp() bool {
	return g.AppID == PlaceholderAppID
}

// HasApp reports whether a REAL GitHub App is configured — a non-zero app_id
// that is not the placeholder sentinel. This is the only correct presence test
// for App auth; `AppID != 0` treats a placeholder as real.
//
// It deliberately says nothing about installation_id or the key file: those are
// separate readiness conditions (see HasUsableApp) that a hive can acquire
// later, over the heartbeat or the config API.
func (g GitHubConfig) HasApp() bool {
	return g.AppID != 0 && !g.IsPlaceholderApp()
}

// HasUsableApp reports whether App auth can actually be attempted: a real
// app_id AND an installation to mint tokens against. The key file is resolved
// separately (config value, then $GH_APP_KEY_FILE, then the on-disk fallbacks),
// so it is not part of this test.
func (g GitHubConfig) HasUsableApp() bool {
	return g.HasApp() && g.InstallationID != 0
}

// ConfiguredButUninstalled reports the half-configured state that must NEVER
// read as healthy: a real App is named but there is no installation to mint
// tokens against. Every token this hive will ever hold is App-minted, so this
// state means auth is a countdown — any client still working is riding a
// cached token that cannot be renewed. Callers use this to raise the install
// banner and fail github_auth from CONFIG truth, not from whether a residual
// token still breathes.
func (g GitHubConfig) ConfiguredButUninstalled() bool {
	return g.HasApp() && g.InstallationID == 0
}

// ResolvedAPIURL returns the configured API URL or the default for github.com.
func (g GitHubConfig) ResolvedAPIURL() string {
	if g.APIURL != "" {
		return g.APIURL
	}
	return DefaultGitHubAPIURL
}

// ResolvedBaseURL returns the base URL of the GitHub the hive's App/repos live
// on, as a full "https://host" URL. It prefers an explicit base_url, but falls
// back to the host of api_url when base_url is blank — pooled/placeholder GHE
// hives legitimately carry base_url: "" with api_url: https://github.ibm.com/api/v3,
// and resolving those to github.com breaks App-host-derived URLs (notably the
// App install link, which would point at github.com/apps/... instead of the GHE
// github.ibm.com/github-apps/... path). This mirrors HostLabel()'s base-or-api
// derivation. Login/device-flow paths deliberately do NOT use this — see
// OAuthBaseURL, which is always public github.com.
func (g GitHubConfig) ResolvedBaseURL() string {
	if g.BaseURL != "" {
		return g.BaseURL
	}
	// base_url blank: derive from api_url's host if it points at a GHE instance.
	if host := g.HostLabel(); host != DefaultGitHubBaseURL[len("https://"):] {
		return "https://" + host
	}
	return DefaultGitHubBaseURL
}

// IsGHE returns true if the hive's App/repos live on a GitHub Enterprise instance
// rather than public github.com. It looks at BOTH base_url and api_url (via
// HostLabel), so a hive carrying only a GHE api_url (base_url: "") is still
// correctly recognized as GHE — matching ResolvedBaseURL and HostLabel.
func (g GitHubConfig) IsGHE() bool {
	return g.HostLabel() != DefaultGitHubBaseURL[len("https://"):]
}

// HostLabel returns the bare GitHub hostname this hive's App/repos live on:
// "github.com" for public GitHub, or the GHE hostname (e.g. "github.ibm.com").
//
// It is the DEFINITIVE host for display and reporting, and deliberately looks at
// BOTH base_url and api_url. Many hives (notably pooled placeholders) carry an
// empty base_url but a GHE api_url (api_url: https://github.ibm.com/api/v3,
// base_url: "") — reporting the host from base_url alone renders those as
// github.com and under-counts GHE hives in the spokes table. Preferring base_url
// when set, then falling back to the api_url's host, makes the reported host
// match the GitHub the hive actually talks to.
func (g GitHubConfig) HostLabel() string {
	pick := g.BaseURL
	if pick == "" {
		pick = g.APIURL
	}
	pick = strings.TrimSpace(pick)
	pick = strings.TrimPrefix(pick, "https://")
	pick = strings.TrimPrefix(pick, "http://")
	pick = strings.TrimSuffix(strings.Trim(pick, "/"), "/api/v3")
	host := strings.SplitN(pick, "/", 2)[0]
	if host == "" || strings.EqualFold(host, "api.github.com") {
		return DefaultGitHubBaseURL[len("https://"):] // "github.com"
	}
	return host
}

// OAuthBaseURL and OAuthAPIURL are the hosts used for DEVICE-FLOW LOGIN and
// user-identity token validation. They are ALWAYS public github.com, decoupled
// from the App/repo host (ResolvedBaseURL/ResolvedAPIURL, which may be GHE).
//
// Why the split: the Hive login OAuth app (oauth_client_id, default the public
// "Ov23ligE2p0gjXg6xAUf") is a github.com app, and every user signs in with a
// github.com identity — even on a hive whose REPOS and GitHub App live on GHE.
// Pointing the device-flow endpoints at a GHE host (as ResolvedBaseURL does for
// a GHE hive) makes GitHub return "Not Found" and the dashboard shows "could not
// verify GitHub identity". Login must therefore use these fixed public hosts;
// only the App-token / repo-API paths follow the per-hive (possibly GHE) host.
// The oauth_*_url_override fields exist ONLY as a test seam. They are
// deliberately absent from every production config: a hive that sets them would
// send its users' device-flow login to a non-github.com host, which is exactly
// the misconfiguration the split above prevents. Tests point them at an
// httptest server so the login path can be exercised without touching the real
// github.com endpoints — see TestMain in pkg/dashboard, which additionally
// hard-fails any test that leaves them unset and would otherwise dial out.
func (g GitHubConfig) OAuthBaseURL() string {
	if g.OAuthBaseURLOverride != "" {
		return g.OAuthBaseURLOverride
	}
	return DefaultGitHubBaseURL
}

func (g GitHubConfig) OAuthAPIURL() string {
	if g.OAuthAPIURLOverride != "" {
		return g.OAuthAPIURLOverride
	}
	return DefaultGitHubAPIURL
}

// OAuthClientIDResolved returns the client ID for device-flow login: the
// configured oauth_client_id, or the public github.com default. Because login
// is always github.com, the default is correct for every hive — a GHE hive must
// not fall back to a GHE client ID here, or device flow against github.com fails
// with an unknown-client error. A hive that has explicitly set oauth_client_id
// to a github.com app keeps that; the default only fills a blank.
func (g GitHubConfig) OAuthClientIDResolved() string {
	if g.OAuthClientID != "" {
		return g.OAuthClientID
	}
	return DefaultOAuthClientID
}

// ResolvedAppSlug returns the app slug for this hive: the configured value,
// else the slug DERIVED from the forge, else the public default.
//
// The forge step is what stops a GHE hive that names only `forge:` from falling
// back to "kubestellar-hive" — a slug that names no App on GHE and builds an
// install link that 404s. That 404 is the failure this whole design exists to
// make unrepresentable, so the derivation belongs here rather than at call sites.
func (g GitHubConfig) ResolvedAppSlug() string {
	if g.AppSlug != "" {
		return g.AppSlug
	}
	if id, ok := forgeIdentities[g.Forge()]; ok && id.AppSlug != "" {
		return id.AppSlug
	}
	return DefaultGitHubAppSlug
}

// BotLogin returns the GitHub App bot login ("<app-slug>[bot]") when a GitHub
// App is configured (AppID set), or "" otherwise. This is the account that
// actually authors PRs and commits when the hive authenticates as an
// installation rather than a personal token.
func (g GitHubConfig) BotLogin() string {
	// Only a REAL, installed App has a bot that can author anything. A bare
	// `AppID != 0` also passes for the placeholder sentinel (PlaceholderAppID)
	// and for a real app_id that was never installed (installation_id: 0) —
	// neither can mint a token or author a PR, so neither has a bot login. Using
	// HasUsableApp() keeps EffectiveAIAuthor() empty for those, so the UI shows
	// "-" (no author) rather than a phantom "<slug>[bot]" for an App-less hive.
	if !g.HasUsableApp() {
		return ""
	}
	return g.ResolvedAppSlug() + "[bot]"
}

// EffectiveAIAuthor returns the GitHub identity agents should author PRs and
// commits as. An explicitly configured project.ai_author always wins. When
// ai_author is empty, the result depends on the opt-in github.app_authored_prs
// flag: if set, agents author as the GitHub App bot ("<slug>[bot]") — deriving
// the bot login here rather than persisting it into ai_author keeps App-bot mode
// durable across restarts (clearing ai_author is enough; nothing writes the bot
// name back into config). If the flag is NOT set, this returns "" exactly as
// before, so a hive that has not opted in behaves identically on upgrade.
func (c *Config) EffectiveAIAuthor() string {
	if c.Project.AIAuthor != "" {
		return c.Project.AIAuthor
	}
	if c.GitHub.AppAuthoredPRsEnabled() {
		return c.GitHub.BotLogin()
	}
	return ""
}

// AppAuthoredPRsEnabled reports whether App-bot authorship is on for this hive.
// The default is ON (nil == true): App-bot authorship is the fleet norm, so a
// hive that never set app_authored_prs gets it. Only an explicit
// `app_authored_prs: false` disables it. Note this is only the INTENT — the
// token is still injected solely for CanPush() tiers with a real App installed,
// so an App-less or advisory hive never actually writes regardless of this flag.
func (g GitHubConfig) AppAuthoredPRsEnabled() bool {
	if g.AppAuthoredPRs == nil {
		return true
	}
	return *g.AppAuthoredPRs
}

// AppInstallURL returns the full URL to install the GitHub App.
// For GHE: {base_url}/github-apps/{slug}/installations/new
// For github.com: https://github.com/apps/{slug}/installations/new
//
// It returns "" for a GitHub Enterprise host whose App slug is neither
// configured nor derivable from the forge-identity table.
//
// # WHY EMPTY RATHER THAN A BEST-EFFORT URL
//
// DefaultGitHubAppSlug ("kubestellar-hive") names the App registered on PUBLIC
// github.com. GitHub Enterprise hosts a SEPARATE App registry, and an enterprise
// registration is rarely given the same slug — so falling back to the default on
// a GHE host emits a link to an App that provably does not exist there, and the
// user lands on a GitHub 404. That is strictly worse than no link: a dead link
// reads as "this product is broken", and it sent a real user to
// github.ibm.com/github-apps/kubestellar-hive/installations/new.
//
// A KNOWN forge is different: forgeIdentities records the slug of the App
// actually registered on that host, so deriving from the table can never build
// the public-slug 404 above. This matters after a "Reset Forge App": the raw
// app_slug/base_url go blank while the resolved identity (what the Forge App
// tab displays) still names github.ibm.com — and returning "" here suppressed
// the install banner on exactly the hive that needed it (vllmd-13).
//
// For a GHE host OUTSIDE the table the empty string remains the honest answer
// — "this host's App slug was never recorded" — and callers render it as a
// legible message naming the missing config instead of a button that 404s.
// Public github.com keeps the default, where it is correct by construction.
func (g GitHubConfig) AppInstallURL() string {
	base := strings.TrimRight(g.ResolvedBaseURL(), "/")
	if g.IsGHE() {
		// An explicit slug wins; otherwise only the forge-identity table may
		// supply one — never DefaultGitHubAppSlug, which names no App here.
		slug := strings.TrimSpace(g.AppSlug)
		if slug == "" {
			if id, ok := forgeIdentities[g.Forge()]; ok {
				slug = id.AppSlug
			}
		}
		if slug == "" {
			return ""
		}
		return base + "/github-apps/" + slug + "/installations/new"
	}
	return base + "/apps/" + g.ResolvedAppSlug() + "/installations/new"
}

type NotificationsConfig struct {
	Ntfy    *NtfyConfig    `yaml:"ntfy,omitempty"`
	Slack   *SlackConfig   `yaml:"slack,omitempty"`
	Discord *DiscordConfig `yaml:"discord,omitempty"`
}

type NtfyConfig struct {
	Server string `yaml:"server"`
	Topic  string `yaml:"topic"`
}

type SlackConfig struct {
	Webhook string `yaml:"webhook"`
}

type DiscordConfig struct {
	Webhook   string `yaml:"webhook"`
	BotToken  string `yaml:"bot_token"`
	ChannelID string `yaml:"channel_id"`
	// AllowedUsers is an allowlist of Discord user IDs permitted to issue bot
	// COMMANDS (!kick, !pause, agent actions — anything that drives an agent).
	// SECURITY: without it, any member of the guild who can post in the channel
	// can inject prompts into the agents. When empty, command handling is
	// DISABLED (fail closed) — the bot still posts status but accepts no
	// commands — so an operator must opt in by listing the trusted user IDs.
	AllowedUsers []string `yaml:"allowed_users,omitempty"`
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
	ContributeSuspended bool   `yaml:"contribute_suspended"`
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
	ContributeTitlesMode          string   `yaml:"contribute_titles_mode,omitempty"`
	ContributeAuthorsMode         string   `yaml:"contribute_authors_mode,omitempty"`
	ContributeLabelsMode          string   `yaml:"contribute_labels_mode,omitempty"`
	ContributeAllowLabels         []string `yaml:"contribute_allow_labels"`
	ContributeDenyLabels          []string `yaml:"contribute_deny_labels"`
	ContributeDenyTitles          []string `yaml:"contribute_deny_titles"`
	ContributeDenyAuthors         []string `yaml:"contribute_deny_authors"`
	ContributeAllowModels         []string `yaml:"contribute_allow_models"`
	ContributeRejectUnknownModels bool     `yaml:"contribute_reject_unknown_models"`
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
	ContributeDelegatableRoles []string            `yaml:"contribute_delegatable_roles,omitempty"`
	DisabledRepos              []string            `yaml:"disabled_repos"`
	DisabledTiers              []string            `yaml:"disabled_tiers"`
	TierLimits                 map[string]TierRate `yaml:"tier_limits"`
	SnapshotIntervalMin        int                 `yaml:"snapshot_interval_min"`
}

// Contribute completion-cooldown defaults and clamp bounds. These live in the
// config package because both the resolver methods below and the Normalize path
// reference them; the dashboard keeps its own equal DEFAULT const
// (completedTaskCooldownHours) as the runtime fallback for hubs built without a
// Config (e.g. direct-in-test construction).
const (
	// contributeCooldownDefaultHours is the with-PR completion cooldown used when
	// ContributeCooldownHours is unset/0 — one week, matching the historical
	// hardcoded default.
	contributeCooldownDefaultHours = 168
	// contributeCooldownMinHours / contributeCooldownMaxHours clamp an
	// operator-supplied period to a sane range (one hour .. one year) so a stray
	// value cannot park an issue effectively forever or disable the cooldown by
	// rounding to zero.
	contributeCooldownMinHours = 1
	contributeCooldownMaxHours = 8760
)

// IsContributeCooldownEnabled resolves the effective on/off state of the
// post-completion cooldown. A nil pointer (unset, older config) defaults to
// ENABLED for backward compatibility; an explicit false disables it.
func (h HubConfig) IsContributeCooldownEnabled() bool {
	return h.ContributeCooldownEnabled == nil || *h.ContributeCooldownEnabled
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

type TierRate struct {
	MaxPerHour    int `yaml:"max_per_hour" json:"max_per_hour"`
	MaxPerDay     int `yaml:"max_per_day" json:"max_per_day"`
	MaxConcurrent int `yaml:"max_concurrent" json:"max_concurrent"`
}

type DashboardConfig struct {
	Port               int    `yaml:"port"`
	SnapshotDir        string `yaml:"snapshot_dir"`
	AuthToken          string `yaml:"auth_token"`
	AgentPollIntervalS int    `yaml:"agent_poll_interval_s"`
	// SnapshotFrameAncestors is the explicit set of HTTPS origins allowed to
	// embed the public, read-only /snapshot document. Empty keeps the historical
	// fail-closed framing policy (X-Frame-Options: DENY plus CSP
	// frame-ancestors 'none'). Wildcards and paths are deliberately rejected so
	// the configured value is a small, auditable origin allowlist.
	SnapshotFrameAncestors []string `yaml:"snapshot_frame_ancestors,omitempty" json:"snapshot_frame_ancestors,omitempty"`
	// AuthorizedUsers is the allowlist of GitHub usernames permitted to log in
	// to a direct-route (non-hub-proxied) spoke via the device flow. The first
	// entry is treated as the owner (read-write); the rest are granted viewers
	// (read-only) unless an explicit "username:role" suffix is given. On the
	// hub-proxied path, nginx injects X-Hive-User/X-Hive-Role and this list is
	// consulted only by the read-only Access tab, NOT for gating logins.
	// Populated on both hub-proxied hosted hives (for the Access view + hub
	// Manage Access) and standalone direct-route spokes (for device-flow authz).
	AuthorizedUsers []string `yaml:"authorized_users"`
	// AuthorizedUserNames is an OPTIONAL, purely cosmetic map from an
	// AuthorizedUsers entry's raw identity key (the same string before any
	// ":role" suffix, e.g. "ibmid:5500087VJB" or a plain GitHub login) to a
	// human-readable display name. It rides alongside AuthorizedUsers — never
	// inside it — so the identity key used for allowlist matching and grants
	// (AuthorizedRole, IsDirectRouteAuthzEnabled) is completely unaffected by
	// this field's presence, absence, or content. Delivered by the hub in the
	// same heartbeat beat as AuthorizedUsers (mirrors it 1:1) so the read-only
	// Access tab can render "Jane Doe" instead of a raw IBMid/Google/Microsoft
	// subject. A key with no entry here (or an empty value) simply has no known
	// human name yet — the UI falls back to the raw key. omitempty/nil-safe:
	// existing configs and hubs that never send this round-trip unchanged.
	AuthorizedUserNames map[string]string `yaml:"authorized_user_names,omitempty"`
	// HubProxied is true when this hive sits behind the hub's nginx auth-proxy,
	// which authenticates every request and injects trusted X-Hive-User/X-Hive-Role
	// headers (hub-reachable-cluster hosted hives). When true the hive TRUSTS those headers and
	// keeps the shared-token path enabled even if AuthorizedUsers is non-empty —
	// nginx is the gate, and the allowlist is informational (Access tab) only.
	//
	// When false (the default) a non-empty AuthorizedUsers list means this is a
	// STANDALONE direct-route spoke with no nginx in front (the heartbeat-only cluster), so it must
	// strip client-supplied identity headers and enforce per-user device-flow
	// authz itself. Decoupling these two meanings fixes hub-proxied hives being
	// wrongly forced into direct-route mode (which broke their dashboard link and
	// snapshot preview) the moment they were granted an authorized_users list.
	HubProxied bool `yaml:"hub_proxied"`
	// PublicURL is the externally reachable origin of THIS dashboard
	// (scheme + host[:port], no path), used to build OAuth redirect URIs —
	// the Linear agent install and the OpenRouter funding flow — when it
	// differs from the host the request arrived on. Precedence when a
	// callback URL is built: dashboard.public_url, then hub.dashboard_url
	// (kept for hub-hosted spokes, whose hub already knows their public
	// name), then the X-Forwarded-Proto/X-Forwarded-Host/Host of the request.
	// Set it on a hub-less hive whose dashboard is private but whose OAuth
	// callback path is published on a different public hostname, or behind
	// an ingress that rewrites the Host header on the way in (Traefik with a
	// fixed upstream Host, a Cloudflare Tunnel "HTTP Host Header"): with
	// nothing configured, the install leg and the callback leg can derive
	// different origins and the provider rejects the code exchange with
	// "redirect_uri is invalid". Validated at load time by
	// ValidateDashboardPublicURL.
	PublicURL string `yaml:"public_url,omitempty" json:"public_url,omitempty"`
}

// ValidateDashboardPublicURL validates and normalizes dashboard.public_url:
// an absolute http(s) URL naming an origin only — no path, query, fragment or
// credentials — returned with any trailing slash removed. Empty is valid and
// means "unset".
func ValidateDashboardPublicURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("dashboard.public_url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("dashboard.public_url %q: must be an absolute http:// or https:// URL", raw)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("dashboard.public_url %q: missing host", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("dashboard.public_url %q: credentials are not allowed", raw)
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("dashboard.public_url %q: must be an origin only (scheme://host[:port]) with no path, query or fragment", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

var snapshotFrameAncestorHostPattern = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\.?$`)

// ValidateSnapshotFrameAncestors validates and normalizes the /snapshot framing
// allowlist. Only exact HTTPS origins are accepted: scheme + host with optional
// port, and no path, query, fragment, credentials, or wildcard hostnames.
func ValidateSnapshotFrameAncestors(origins []string) ([]string, error) {
	normalized := make([]string, 0, len(origins))
	seen := map[string]struct{}{}
	for _, raw := range origins {
		origin := strings.TrimSpace(raw)
		if origin == "" {
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("snapshot_frame_ancestors entry %q must be an https origin", raw)
		}
		if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.Contains(u.Hostname(), "*") {
			return nil, fmt.Errorf("snapshot_frame_ancestors entry %q must be an exact https origin with no path, credentials, query, fragment, or wildcard", raw)
		}
		if !validSnapshotFrameAncestorHost(u.Hostname()) {
			return nil, fmt.Errorf("snapshot_frame_ancestors entry %q has an invalid host", raw)
		}
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return nil, fmt.Errorf("snapshot_frame_ancestors entry %q has an invalid port", raw)
			}
		}
		origin = "https://" + u.Host
		if _, ok := seen[origin]; ok {
			continue
		}
		seen[origin] = struct{}{}
		normalized = append(normalized, origin)
	}
	return normalized, nil
}

func validSnapshotFrameAncestorHost(host string) bool {
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return snapshotFrameAncestorHostPattern.MatchString(host)
}

// SnapshotFrameAncestorsCSP returns the CSP source list for /snapshot framing.
func (d DashboardConfig) SnapshotFrameAncestorsCSP() string {
	if len(d.SnapshotFrameAncestors) == 0 {
		return "'none'"
	}
	return strings.Join(d.SnapshotFrameAncestors, " ")
}

// Role strings used for direct-route spoke authorization. These mirror the
// roles the hub injects via X-Hive-Role on the proxied path so the read-only
// gating in the dashboard behaves identically on both paths.
const (
	RoleOwner = "owner"
	RoleRead  = "read"
	// RoleReadWrite is granted by the hub's Manage Access screen (see the role
	// validation in hub.handleAccessAdd, which accepts read / read-write /
	// owner). It was missing here, so splitAuthorizedEntry treated
	// "user:read-write" as an unknown suffix and folded the whole string into
	// the username — the allowlist then contained a user literally named
	// "cbrooker27:read-write", no lookup could ever match, and every login was
	// rejected as unauthorized despite the grant being present and correct.
	RoleReadWrite = "read-write"
	// RoleMerger can do everything read-write can, plus approve/queue other
	// people's PRs for Hive's auto-merge-on-green sweep. Both the dashboard
	// queue endpoint and the sweep enforce the "never your own PR" rule
	// server-side.
	RoleMerger = "merger"
)

// DefaultAutoMergeLabel is the label applied when a hive does not configure
// `governor.labels.automerge`. `lgtm` is the long-standing Prow/Kubernetes
// convention for "a second person signed off on this", which is exactly the
// decision the merger tier records, so a managed repository usually already
// has it and already understands it.
const DefaultAutoMergeLabel = "lgtm"

var roleRanks = map[string]int{
	RoleRead:      1,
	RoleReadWrite: 2,
	RoleMerger:    3,
	RoleOwner:     4,
}

// ValidRole reports whether role is one of the hive access tiers.
func ValidRole(role string) bool {
	_, ok := roleRanks[strings.ToLower(strings.TrimSpace(role))]
	return ok
}

// RoleAtLeast reports whether role includes all capabilities of tier.
func RoleAtLeast(role, tier string) bool {
	roleRank, okRole := roleRanks[strings.ToLower(strings.TrimSpace(role))]
	tierRank, okTier := roleRanks[strings.ToLower(strings.TrimSpace(tier))]
	return okRole && okTier && roleRank >= tierRank
}

// AuthorizedRole resolves a username against the spoke's authorized-users
// allowlist and returns the user's role and whether they are authorized.
//
// Each entry is either "username" or "username:role" (role = "owner" or "read").
// An entry without an explicit role defaults to "owner" for the first entry
// (the hive owner) and "read" for the rest (granted viewers). Matching is
// case-insensitive because GitHub usernames are case-insensitive, and tolerant
// of the hub's canonical identity form: a bare login and its "github:<login>"
// wire form denote the SAME user (the hub's legacy shim), so an allowlist entry
// in either form matches a session username in either form. Without this, a
// hub-delivered "github:alice:owner" entry silently failed to authorize the
// device-flow login "alice" — one more variant of the recurring
// "granted owner still gets 'owner access required'" class.
func (d DashboardConfig) AuthorizedRole(username string) (string, bool) {
	if username == "" {
		return "", false
	}
	want := identityMatchKey(username)
	for i, entry := range d.AuthorizedUsers {
		name, role := splitAuthorizedEntry(entry)
		if name == "" {
			continue
		}
		if identityMatchKey(name) != want {
			continue
		}
		if role == "" {
			if i == 0 {
				role = RoleOwner
			} else {
				role = RoleRead
			}
		}
		return role, true
	}
	return "", false
}

// identityMatchKey normalizes an identity string for allowlist comparison:
// lower-cased (GitHub logins are case-insensitive, and the pre-existing
// allowlist match always folded case), with the legacy-GitHub provider prefix
// stripped so "github:alice" and "alice" compare equal. Other providers'
// prefixes ("ibmid:", "google:", ...) are kept — those subjects are only ever
// delivered and presented in their full canonical form.
func identityMatchKey(id string) string {
	key := strings.ToLower(strings.TrimSpace(id))
	return strings.TrimPrefix(key, "github:")
}

// IsDirectRouteAuthzEnabled reports whether this hive must enforce per-user
// authorization on device-flow logins ITSELF because there is no hub nginx in
// front of it. True only for a STANDALONE spoke: it has an authorized-users
// allowlist AND is not hub-proxied.
//
// A hub-proxied hive (HubProxied=true) can also carry an allowlist — for the
// read-only Access tab and the hub's Manage Access screen — but nginx is the
// gate there, so it must NOT flip into direct-route mode (which would strip the
// trusted X-Hive-User/X-Hive-Role headers nginx injects and disable the shared
// token, breaking the dashboard link and the snapshot preview).
func (d DashboardConfig) IsDirectRouteAuthzEnabled() bool {
	return !d.HubProxied && len(d.AuthorizedUsers) > 0
}

// splitAuthorizedEntry parses a "username" or "username:role" allowlist entry.
func splitAuthorizedEntry(entry string) (name, role string) {
	entry = strings.TrimSpace(entry)
	if idx := strings.LastIndex(entry, ":"); idx >= 0 {
		name = strings.TrimSpace(entry[:idx])
		role = strings.ToLower(strings.TrimSpace(entry[idx+1:]))
		if !ValidRole(role) {
			// Unknown role suffix — treat the whole thing as a bare username so
			// a stray colon can never silently downgrade or escalate access.
			return strings.TrimSpace(entry), ""
		}
		return name, role
	}
	return entry, ""
}

type DataConfig struct {
	MetricsDir         string `yaml:"metrics_dir"`
	LogsDir            string `yaml:"logs_dir"`
	ClaudeSessionsDir  string `yaml:"claude_sessions_dir"`
	CopilotSessionsDir string `yaml:"copilot_sessions_dir"`
	BobSessionsDir     string `yaml:"bob_sessions_dir"`
	AgentsDir          string `yaml:"agents_dir"`
}

// Load reads hive.yaml, then applies config.env overrides if present.
// Precedence: hive.yaml < config.env < explicit env vars (via ${} interpolation).
func Load(path string) (*Config, error) {
	return LoadWithOverrides(path, "")
}

// LoadWithOverrides reads hive.yaml and applies a config.env override file.
// If envPath is empty, it looks for config.env next to hive.yaml, then at
// /etc/hive/config.env. Pass "-" to skip config.env entirely.
func LoadWithOverrides(path, envPath string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	expanded := expandEnvVars(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if envPath != "-" {
		if envPath == "" {
			envPath = findConfigEnv(path)
		}
		if envPath != "" {
			if err := cfg.applyConfigEnv(envPath); err != nil {
				return nil, fmt.Errorf("applying config.env %s: %w", envPath, err)
			}
		}
	}

	cfg.SourcePath = path
	cfg.applyBootstrapEnv()
	cfg.applyDefaults()

	// Merge per-agent overlay files from the agents directory.
	if cfg.Data.AgentsDir != "" {
		overlays, err := LoadAgentOverrides(cfg.Data.AgentsDir)
		if err != nil {
			return nil, fmt.Errorf("loading agent overlays: %w", err)
		}
		cfg.MergeAgentOverrides(overlays)
		// Re-apply defaults for overlay agents.
		for name := range overlays {
			cfg.ApplyAgentDefaults(name)
		}
	}

	if err := cfg.ExpandAgentReplicas(); err != nil {
		return nil, fmt.Errorf("expanding agent replicas: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

// LoadWithDashboardOverlay loads the config from path, then — in Kubernetes
// mode — re-applies the dashboard overlay's agent configs on top, mirroring
// the entrypoint's boot-time seed+overlay merge.
//
// Why this exists: the ConfigMap seed at path carries only provision-time
// agent fields. Runtime reconciliation (ApplyPack raising a hive's ACMM level
// updates kick_template/mode/model) is persisted to the dashboard overlay, NOT
// the seed. The entrypoint merges the overlay over the seed once at boot, but a
// live ConfigMap remount rewrites the seed back to its stale values and fires
// the config watcher. If the watcher reloaded the raw seed it would silently
// revert every reconciled agent field (observed: a hive raised to L5 dropped
// its scanner back to the L2/L3 advisory template at runtime). Applying the
// overlay here keeps the reload consistent with boot.
func LoadWithDashboardOverlay(path string) (*Config, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	if !IsKubernetesPod() {
		return cfg, nil
	}
	data, err := os.ReadFile(DashboardOverlayFile)
	if err != nil {
		// No overlay (or unreadable) — the seed is authoritative, as at boot.
		return cfg, nil
	}
	var overlay Config
	if err := yaml.Unmarshal([]byte(expandEnvVars(string(data))), &overlay); err != nil {
		return cfg, nil // malformed overlay: fall back to seed, don't fail the reload
	}
	// Tombstones live in the dashboard overlay because that is the only agent
	// source the dashboard can write. Adopt them BEFORE the fullness guard below
	// so a short/empty overlay (one that has no agents yet, or only carries the
	// removed_agents list) still yields the tombstone. Previously this ran AFTER
	// the guard, so on a reload the guard's early return dropped RemovedAgents to
	// empty; the ~2-min saver then rewrote every layer tombstone-free and the
	// deleted agents reappeared on an interval (#2439). Merge already skips and
	// prunes tombstoned agents, so adopting them early is safe even when we bail.
	if !overlay.OTel.IsZero() {
		cfg.OTel = overlay.OTel
		cfg.Tracing = overlay.OTel
	} else if !overlay.Tracing.IsZero() {
		cfg.Tracing = overlay.Tracing
		cfg.OTel = mergeOTelOverride(cfg.OTel, overlay.Tracing)
	}
	// Governor work source: the dashboard's PUT /api/config/governor/work-source
	// writes the whole config to the overlay, but the reload only adopted
	// OTel/Tracing, RemovedAgents and Agents from it — so a work source set
	// from the dashboard was lost on every pod restart and GET returned
	// type "" again. Adopt it here, BEFORE the fullness guard, so a short
	// overlay (no agents yet) still carries it. The whole block is copied so
	// per-adapter settings (teams, hold labels, assigned_only, …) survive with
	// the type. Nothing here touches overlay.Variables — see the security
	// invariant at the end of this function.
	if !overlay.Governor.WorkSource.IsZero() {
		cfg.Governor.WorkSource = overlay.Governor.WorkSource
	}
	if len(overlay.RemovedAgents) > 0 {
		cfg.RemovedAgents = overlay.RemovedAgents
		cfg.PruneRemovedAgents()
		// Observability (#2439): this runs on boot AND on every ~2-min config reload,
		// so keep it at DEBUG. It confirms the reload adopted the overlay's tombstones
		// BEFORE the fullness guard below — the exact ordering whose absence let the
		// deleted agents reappear on an interval.
		slog.Default().Debug("reload: adopted removed-agents from overlay",
			"hive_id", cfg.HiveID,
			"count", len(cfg.RemovedAgents),
			"agents", cfg.RemovedAgents,
		)
	}
	// Guard: the overlay must look like a full hive config (same check the
	// entrypoint and validateSaveGuard apply) before we trust its agents.
	if overlay.Project.Org == "" || len(overlay.Agents) == 0 {
		return cfg, nil
	}
	// Overlay agents win — they carry the reconciled pack-behavior fields.
	cfg.MergeAgentOverrides(overlay.Agents)
	for name := range overlay.Agents {
		cfg.ApplyAgentDefaults(name)
	}
	if err := cfg.ExpandAgentReplicas(); err != nil {
		return cfg, err
	}
	// Security invariant: cfg.Variables (resolver defs + the exec/http trust
	// policy) comes ONLY from the seed loaded above — the overlay's Variables
	// block is intentionally NOT merged. The dashboard overlay is user-writable,
	// so honoring its resolver policy would let a compromised overlay enable
	// script/http execution. Keep this true if overlay merging is ever expanded.
	return cfg, nil
}

// findConfigEnv returns the path to a config.env file, or "" if none found.
func findConfigEnv(yamlPath string) string {
	candidates := []string{
		strings.TrimSuffix(yamlPath, "hive.yaml") + "config.env",
		"/etc/hive/config.env",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// ParseEnvFile reads a flat KEY=VALUE file (# comments, blank lines skipped).
func ParseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only fd; nothing to lose on close error

	result := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		result[key] = val
	}
	return result, scanner.Err()
}

// applyConfigEnv merges flat KEY=VALUE overrides into the loaded config.
func (c *Config) applyConfigEnv(path string) error {
	env, err := ParseEnvFile(path)
	if err != nil {
		return err
	}

	if v, ok := env["PROJECT_ORG"]; ok {
		c.Project.Org = v
	}
	if v, ok := env["PROJECT_REPOS"]; ok {
		c.Project.Repos = strings.Fields(v)
	}
	if v, ok := env["PROJECT_AI_AUTHOR"]; ok {
		c.Project.AIAuthor = v
	}
	if v, ok := env["PROJECT_PRIMARY_REPO"]; ok {
		c.Project.PrimaryRepo = v
	}
	if v, ok := env["PROJECT_OPEN_PRS"]; ok {
		b := v == "true" || v == "1" || v == "yes"
		c.Project.OpenPRs = &b
	}
	if v, ok := env["AGENTS_ENABLED"]; ok {
		for _, name := range strings.Fields(v) {
			if agent, exists := c.Agents[name]; exists {
				agent.Enabled = true
				c.Agents[name] = agent
			}
		}
	}
	if v, ok := env["DASHBOARD_PORT"]; ok {
		var port int
		if _, err := fmt.Sscanf(v, "%d", &port); err == nil && port > 0 {
			c.Dashboard.Port = port
		}
	}
	if v, ok := env["DASHBOARD_AUTH_TOKEN"]; ok {
		c.Dashboard.AuthToken = v
	}
	if c.Dashboard.AuthToken == "" {
		if v, ok := env["HIVE_DASHBOARD_TOKEN"]; ok {
			c.Dashboard.AuthToken = v
		}
	}

	return nil
}

func (c *Config) applyBootstrapEnv() {
	if repo := os.Getenv("HIVE_REPO"); repo != "" {
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			if c.Project.Org == "" {
				c.Project.Org = parts[0]
			}
			if len(c.Project.Repos) == 0 {
				c.Project.Repos = []string{parts[1]}
			}
			if c.Project.PrimaryRepo == "" {
				c.Project.PrimaryRepo = parts[1]
			}
		}
	}
	// K8s deployments pass the auth token as an OS env var from a Secret.
	// applyConfigEnv only reads file-based config.env, so without this
	// the token is silently ignored and the dashboard is unauthenticated.
	if c.Dashboard.AuthToken == "" {
		if v := os.Getenv("DASHBOARD_AUTH_TOKEN"); v != "" {
			c.Dashboard.AuthToken = v
		}
	}
	if c.Dashboard.AuthToken == "" {
		if v := os.Getenv("HIVE_DASHBOARD_TOKEN"); v != "" {
			c.Dashboard.AuthToken = v
		}
	}
	// K8s-provisioned spokes receive their per-hive authorized GitHub users as a
	// comma-separated env var (owner first). This is what lets a direct-route
	// spoke reject unauthorized device-flow logins without the hub proxy.
	if len(c.Dashboard.AuthorizedUsers) == 0 {
		if v := os.Getenv("HIVE_AUTHORIZED_USERS"); v != "" {
			c.Dashboard.AuthorizedUsers = parseAuthorizedUsers(v)
		}
	}
}

// parseAuthorizedUsers splits a comma-separated authorized-users list, trimming
// whitespace and dropping empty entries. Order is preserved so the first entry
// remains the owner.
func parseAuthorizedUsers(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if u := strings.TrimSpace(p); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// expandEnvVars substitutes ${VAR} references in the raw config text. It runs
// BEFORE the YAML is parsed, so to honor an operator `variables:` block it
// first bootstrap-parses just that block from the same text, builds a
// config-scoped resolve.Registry, and delegates. With no `variables:` block the
// registry is env-only and the result is byte-identical to the legacy behavior
// (${NAME} -> os.LookupEnv(NAME), unset left literal).
func expandEnvVars(s string) string {
	reg := configRegistryFromText(s)
	return reg.Expand(context.Background(), s, resolve.ScopeConfig, nil)
}

// bootstrapVariables is a minimal view of hive.yaml used to read the
// `variables:` block before the whole document is expanded/parsed. Unknown keys
// are ignored by yaml.Unmarshal, so this is cheap and tolerant.
type bootstrapVariables struct {
	Variables VariablesConfig `yaml:"variables"`
}

// configRegistryFromText bootstrap-parses the variables block from raw config
// text and returns a config-scoped resolve.Registry. On any parse failure it
// falls back to an env-only registry (legacy behavior), never an error — config
// expansion must not fail the load.
func configRegistryFromText(raw string) *resolve.Registry {
	var bv bootstrapVariables
	if err := yaml.Unmarshal([]byte(raw), &bv); err != nil {
		return resolve.EnvOnly()
	}
	if len(bv.Variables.Defs) == 0 {
		// No custom variables — env-only, byte-identical to legacy.
		return resolve.EnvOnly()
	}
	specs, pol := bv.Variables.toResolveSpecs()
	return resolve.Build(specs, pol, nil)
}

// toResolveSpecs translates the config-level variables block into the
// resolve package's VarSpec list and Policy.
func (v VariablesConfig) toResolveSpecs() ([]resolve.VarSpec, resolve.Policy) {
	specs := make([]resolve.VarSpec, 0, len(v.Defs))
	for name, def := range v.Defs {
		spec := resolve.VarSpec{
			Name:    name,
			Type:    def.Type,
			Scope:   resolve.Scope(def.Scope),
			Value:   def.Value,
			Env:     def.Env,
			Command: def.Command,
			URL:     def.URL,
			Headers: def.Headers,
		}
		if def.Default != nil {
			spec.HasDefault = true
			spec.Default = *def.Default
		}
		specs = append(specs, spec)
	}
	pol := resolve.Policy{
		AllowExec:     v.Security.AllowExec,
		AllowHTTP:     v.Security.AllowHTTP,
		HTTPAllowlist: v.Security.HTTPAllowlist,
		ExecTimeoutS:  v.Security.ExecTimeoutS,
		HTTPTimeoutS:  v.Security.HTTPTimeoutS,
	}
	return specs, pol
}

// ResolveRegistry builds a resolve.Registry from this config's `variables:`
// block, for use at per-kick template substitution sites (scheduler, dashboard
// preview). With no variables configured it returns an env-only registry whose
// Expand — in template scope, where the runtime built-ins win and there is no
// env fallback — reproduces the previous strings.NewReplacer output exactly.
// Pass a logger to surface disabled/invalid resolver diagnostics; nil is fine.
func (c *Config) ResolveRegistry(logger *slog.Logger) *resolve.Registry {
	if len(c.Variables.Defs) == 0 {
		return resolve.EnvOnly()
	}
	specs, pol := c.Variables.toResolveSpecs()
	return resolve.Build(specs, pol, logger)
}

// GitHubPromptAllowed reports whether an agent's prompt_source pointing at the
// given "owner/repo" slug is permitted to be fetched. This mirrors the seed-only
// gating used for exec/http resolvers: it consults c.Variables.Security, which
// LoadWithDashboardOverlay guarantees comes ONLY from the trusted config seed
// (the dashboard overlay's Variables block is never merged). It returns false
// unless the feature is explicitly enabled AND the slug is on the seed-declared
// allowlist, so a user-writable overlay can never widen the set of readable repos.
func (c *Config) GitHubPromptAllowed(slug string) bool {
	if slug == "" || !c.Variables.Security.AllowGitHubPrompt {
		return false
	}
	for _, allowed := range c.Variables.Security.GitHubPromptAllowlist {
		if strings.EqualFold(strings.TrimSpace(allowed), slug) {
			return true
		}
	}
	return false
}

// GitHubDefinitionAllowed reports whether an agent's definition_source pointing
// at the given "owner/repo" slug is permitted to be fetched. A live whole-agent
// definition is at least as trusted as a live prompt (it re-applies more fields),
// so it reuses the SAME seed-only gate as GitHubPromptAllowed: the feature flag
// and repo allowlist come only from the trusted config seed, never the
// user-writable dashboard overlay (LoadWithDashboardOverlay never merges the
// overlay's Variables block). A compromised overlay therefore can neither widen
// the set of readable repos nor point a live definition at an arbitrary repo.
func (c *Config) GitHubDefinitionAllowed(slug string) bool {
	return c.GitHubPromptAllowed(slug)
}

const (
	MaxAgentReplicas = 5

	defaultDashboardPort          = 3002
	defaultAgentPollIntervalS     = 10
	defaultEvalIntervalS          = 300
	defaultPollIntervalMins       = 5
	defaultKnowledgeMaxFacts      = 25
	defaultKnowledgeEngine        = "llm-wiki"
	defaultCuratorSchedule        = "daily"
	defaultPromoteThreshold       = 0.9
	defaultSensingTTLSeconds      = 900
	defaultSensingPullbackSeconds = 900
	defaultHealthcheckIntervalS   = 300
	defaultRestartCooldownS       = 60
	defaultBudgetPeriodDays       = 7
	defaultBudgetCriticalPct      = 90
	defaultLogMaxSizeMB           = 50
	defaultLogMaxAgeDays          = 7
	defaultLogMaxBackups          = 10
	defaultLogLevel               = "info"

	// defaultAdvisoryMaxFindings is the digest's default top-N cap. Ten fits
	// in a screenful, which is the point: the digest is a "what should I look
	// at next" list, not an inventory.
	defaultAdvisoryMaxFindings = 10
	// defaultAdvisoryStalenessDays is how long an advisory bead survives
	// without being re-reported. Advisory agents re-scan far more often than
	// weekly, so a finding untouched for a week is one no agent still sees.
	defaultAdvisoryStalenessDays = 7
)

func (c *Config) applyDefaults() {
	// Repo targets are built as org + "/" + repo, so an entry that already
	// carries the org resolves to "org/org/repo" and every agent fails. Strip a
	// matching org prefix off both primary_repo and every repos entry on load.
	// primary_repo has been normalized here for a long time; repos was not,
	// which is why live configs are seen with a correct bare primary_repo next
	// to an org-qualified repos list. A mismatched org is deliberately left
	// alone so ValidateRepoTargets still rejects it rather than silently
	// retargeting the hive at a repository the owner never named.
	if c.Project.PrimaryRepo != "" && c.Project.Org != "" {
		c.Project.PrimaryRepo, _ = NormalizeRepoForOrg(c.Project.Org, c.Project.PrimaryRepo)
	}
	if len(c.Project.Repos) > 0 && c.Project.Org != "" {
		c.Project.Repos, _ = NormalizeProjectRepos(c.Project.Org, c.Project.Repos)
	}
	if c.Dashboard.Port == 0 {
		c.Dashboard.Port = defaultDashboardPort
	}
	if c.Dashboard.AgentPollIntervalS == 0 {
		c.Dashboard.AgentPollIntervalS = defaultAgentPollIntervalS
	}
	if normalized, err := ValidateSnapshotFrameAncestors(c.Dashboard.SnapshotFrameAncestors); err == nil {
		c.Dashboard.SnapshotFrameAncestors = normalized
	}
	if c.Governor.EvalIntervalS == 0 {
		c.Governor.EvalIntervalS = defaultEvalIntervalS
	}
	if c.Governor.Trajectory.IsEnabled() {
		if c.Governor.Trajectory.OnDivergence == "" {
			c.Governor.Trajectory.OnDivergence = "pause"
		}
	}
	if c.Policies.PollInterval == 0 {
		c.Policies.PollInterval = time.Duration(defaultPollIntervalMins) * time.Minute
	}
	if c.Data.MetricsDir == "" {
		c.Data.MetricsDir = "/data/metrics"
	}
	if c.Data.LogsDir == "" {
		c.Data.LogsDir = "/data/logs"
	}
	if c.Data.ClaudeSessionsDir == "" {
		c.Data.ClaudeSessionsDir = "/data/home/.claude/projects"
	}
	if c.Data.CopilotSessionsDir == "" {
		c.Data.CopilotSessionsDir = "/data/home/.copilot/session-state"
	}
	if c.Data.BobSessionsDir == "" {
		c.Data.BobSessionsDir = "/data/home/.bob"
	}
	if c.Data.AgentsDir == "" {
		c.Data.AgentsDir = "/data/agent-configs"
	}
	if c.Hub.URL == "" {
		c.Hub.URL = "https://hive.kubestellar.io"
		c.Hub.IsPublic = true
	}
	for name, agent := range c.Agents {
		agent.name = name
		if agent.ID == "" {
			agent.ID = name
		}
		if agent.Replicas == 0 {
			agent.Replicas = 1
		}
		if agent.BeadsDir == "" {
			agent.BeadsDir = fmt.Sprintf("/data/beads/%s", name)
		}
		// Default to enabled unless the user explicitly set enabled: false.
		if !agent.Enabled && !agent.enabledSet {
			agent.Enabled = true
		}
		if !agent.clearOnKickSet {
			agent.ClearOnKick = true
		}
		if agent.Role == "" {
			agent.Role = name
		}
		applyKnownAgentDefaults(name, &agent)
		c.Agents[name] = agent
	}

	if len(c.Hub.ContributeDenyTitles) == 0 {
		c.Hub.ContributeDenyTitles = []string{
			"*dependency dashboard*",
			"*renovate dashboard*",
			"epic:*",
			"epic(*",
		}
	}
	if len(c.Hub.ContributeDenyAuthors) == 0 {
		c.Hub.ContributeDenyAuthors = []string{
			"renovate[bot]",
			"dependabot[bot]",
			"mergeraptor[bot]",
		}
	}

	// Contribute filter modes: default to deny (the pre-mode behavior — the
	// *Deny* lists were always deny lists). Normalize any stored value.
	c.Hub.ContributeTitlesMode = NormalizeFilterMode(c.Hub.ContributeTitlesMode)
	c.Hub.ContributeAuthorsMode = NormalizeFilterMode(c.Hub.ContributeAuthorsMode)
	c.Hub.ContributeLabelsMode = NormalizeFilterMode(c.Hub.ContributeLabelsMode)

	// Contribute completion-cooldown period: leave 0 (== "use default") alone, but
	// clamp any explicitly-set value to [min,max] so a stray input cannot park an
	// issue effectively forever or round down to disable the cooldown.
	if c.Hub.ContributeCooldownHours != 0 {
		if c.Hub.ContributeCooldownHours < contributeCooldownMinHours {
			c.Hub.ContributeCooldownHours = contributeCooldownMinHours
		} else if c.Hub.ContributeCooldownHours > contributeCooldownMaxHours {
			c.Hub.ContributeCooldownHours = contributeCooldownMaxHours
		}
	}

	// One-time migration of the old dual label lists into the single list+mode.
	// If a legacy allow list was set (and no new label list/mode has been chosen
	// yet), adopt it as an allow filter. Otherwise the deny list (if any) stands.
	// After migration the allow list is cleared so it isn't re-applied.
	if len(c.Hub.ContributeAllowLabels) > 0 && len(c.Hub.ContributeDenyLabels) == 0 &&
		c.Hub.ContributeLabelsMode == FilterModeDeny {
		c.Hub.ContributeDenyLabels = c.Hub.ContributeAllowLabels
		c.Hub.ContributeLabelsMode = FilterModeAllow
		c.Hub.ContributeAllowLabels = nil
	}

	defaultTierLimits := map[string]TierRate{
		"newcomer":    {MaxPerHour: 3, MaxPerDay: 10, MaxConcurrent: 1},
		"contributor": {MaxPerHour: 10, MaxPerDay: 50, MaxConcurrent: 2},
		"trusted":     {MaxPerHour: 30, MaxPerDay: 200, MaxConcurrent: 5},
		"merger":      {MaxPerHour: 30, MaxPerDay: 200, MaxConcurrent: 5},
		"advisor":     {MaxPerHour: 0, MaxPerDay: 0, MaxConcurrent: 0},
	}
	if c.Hub.TierLimits == nil {
		c.Hub.TierLimits = map[string]TierRate{}
	}
	for tier, limits := range defaultTierLimits {
		if _, ok := c.Hub.TierLimits[tier]; !ok {
			c.Hub.TierLimits[tier] = limits
		}
	}

	if len(c.Governor.Labels.Exempt) == 0 {
		c.Governor.Labels.Exempt = []string{
			"nightly-tests", "LFX", "meta-tracker",
			"auto-qa-tuning-report", "adopters",
			"changes-requested", "waiting-on-author",
		}
	}
	if strings.TrimSpace(c.Governor.Labels.AutoMerge) == "" {
		c.Governor.Labels.AutoMerge = DefaultAutoMergeLabel
	}
	if len(c.Governor.Sensing.GHRatePatterns) == 0 {
		c.Governor.Sensing.GHRatePatterns = []string{
			"API rate limit exceeded",
			"secondary rate limit",
			"403.*rate limit",
			"You have exceeded a secondary rate",
			"retry-after:[[:space:]]*[0-9]",
			"gh: Resource not accessible",
			"abuse detection mechanism",
		}
	}
	if len(c.Governor.Sensing.CLIExcludePatterns) == 0 {
		c.Governor.Sensing.CLIExcludePatterns = []string{
			"You.re out of extra usage",
			"out of extra usage",
			"extra usage.*resets",
			"resets [0-9]+(:[0-9]+)?[aApP][mM]",
		}
	}
	// #4041: hives that saved their config while the pre-#3959 defaults were
	// in effect have that generic list MATERIALIZED as explicit values in
	// their persisted config (Save() marshals the whole struct, defaults
	// included), and "defaults only apply to an empty list" meant the #3959
	// fix could never reach them — a live hosted hive's quality agent flapped
	// on `(?i)copilot auth` for days with restart_count 83. A list that is
	// byte-identical to the old default set expresses no operator intent, so
	// treat it as "default": drop it and let the corrected defaults apply. A
	// list that differs in ANY way is an operator's, and stays verbatim.
	if IsLegacyDefaultLoginPatterns(c.Governor.Sensing.LoginPatterns) {
		log.Printf("[config] migrating login_patterns: persisted list matches the pre-#3959 defaults verbatim — dropping it so the corrected defaults apply (customized lists are never touched)")
		c.Governor.Sensing.LoginPatterns = nil
	}
	if len(c.Governor.Sensing.LoginPatterns) == 0 {
		// Each pattern must match the CLI's own login CHROME, never ordinary
		// English. The login-detector matches these against the agent's PANE —
		// which contains the issue bodies, PR diffs and CI logs the agent is
		// READING — so generic phrases pause an agent for merely discussing an
		// auth error. The previous defaults ("authentication required",
		// "unauthorized.*401", "session expired", "login required", "please
		// log in", "token expired") did exactly that on a live hive: a
		// ci-maintainer reading CI logs full of 401s and a supervisor
		// summarising issues across 39 repos were paused repeatedly, and the
		// two HIGHEST-cadence agents are hit hardest because the detector runs
		// at kick time, so exposure scales with kick frequency. Reproduced on
		// demand by typing "authentication required" into a healthy agent's
		// input box (unsubmitted — just rendered on the pane) and kicking it.
		c.Governor.Sensing.LoginPatterns = append([]string(nil), defaultLoginPatterns...)
	}
	if c.Governor.Sensing.TTLSeconds == 0 {
		c.Governor.Sensing.TTLSeconds = defaultSensingTTLSeconds
	}
	if c.Governor.Sensing.PullbackSeconds == 0 {
		c.Governor.Sensing.PullbackSeconds = defaultSensingPullbackSeconds
	}
	if c.Governor.Health.HealthcheckInterval == 0 {
		c.Governor.Health.HealthcheckInterval = defaultHealthcheckIntervalS
	}
	if c.Governor.Health.RestartCooldown == 0 {
		c.Governor.Health.RestartCooldown = defaultRestartCooldownS
	}
	if c.Governor.Budget.PeriodDays == 0 {
		c.Governor.Budget.PeriodDays = defaultBudgetPeriodDays
	}
	if c.Governor.Budget.CriticalPct == 0 {
		c.Governor.Budget.CriticalPct = defaultBudgetCriticalPct
	}
	if c.Governor.Logging.Dir == "" {
		c.Governor.Logging.Dir = c.Data.LogsDir
	}
	if c.Governor.Logging.MaxSizeMB == 0 {
		c.Governor.Logging.MaxSizeMB = defaultLogMaxSizeMB
	}
	if c.Governor.Logging.MaxAgeDays == 0 {
		c.Governor.Logging.MaxAgeDays = defaultLogMaxAgeDays
	}
	if c.Governor.Logging.MaxBackups == 0 {
		c.Governor.Logging.MaxBackups = defaultLogMaxBackups
	}
	if !c.Governor.Logging.Compress {
		c.Governor.Logging.Compress = true
	}
	if c.Governor.Logging.Level == "" {
		c.Governor.Logging.Level = defaultLogLevel
	}
	if c.Governor.Advisory.MaxFindings == 0 {
		c.Governor.Advisory.MaxFindings = defaultAdvisoryMaxFindings
	}
	if c.Governor.Advisory.StalenessDays == 0 {
		c.Governor.Advisory.StalenessDays = defaultAdvisoryStalenessDays
	}
	if c.Governor.Advisory.PRAutoClose == nil {
		on := true
		c.Governor.Advisory.PRAutoClose = &on
	}

	if c.Knowledge.Enabled {
		if c.Knowledge.Engine == "" {
			c.Knowledge.Engine = defaultKnowledgeEngine
		}
		if c.Knowledge.Primer.MaxFacts == 0 {
			c.Knowledge.Primer.MaxFacts = defaultKnowledgeMaxFacts
		}
		if c.Knowledge.Primer.MergeStrategy == "" {
			c.Knowledge.Primer.MergeStrategy = "precedence"
		}
		if len(c.Knowledge.Primer.Priority) == 0 {
			c.Knowledge.Primer.Priority = []string{"regression", "gotcha", "test_scaffold", "pattern", "decision"}
		}
		if c.Knowledge.Curator.Schedule == "" {
			c.Knowledge.Curator.Schedule = defaultCuratorSchedule
		}
		if c.Knowledge.Curator.AutoPromoteThreshold == 0 {
			c.Knowledge.Curator.AutoPromoteThreshold = defaultPromoteThreshold
		}
		if c.Knowledge.BeadSynthesizer.Schedule == "" {
			c.Knowledge.BeadSynthesizer.Schedule = "hourly"
		}
		if c.Knowledge.BeadSynthesizer.MinConfidence == 0 {
			c.Knowledge.BeadSynthesizer.MinConfidence = 0.5
		}
		if c.Knowledge.BeadSynthesizer.TargetLayer == "" {
			c.Knowledge.BeadSynthesizer.TargetLayer = "project"
		}
		if c.Knowledge.BeadSynthesizer.MaxFactsPerCycle == 0 {
			c.Knowledge.BeadSynthesizer.MaxFactsPerCycle = 20
		}
		if c.Knowledge.BeadSynthesizer.VaultPath == "" {
			c.Knowledge.BeadSynthesizer.VaultPath = "/data/vaults/bead-synth-wiki"
		}
	}
}

// applyKnownAgentDefaults populates metadata fields for well-known agent names
// when those fields are not explicitly set in YAML. This bridges existing configs.
func applyKnownAgentDefaults(name string, agent *AgentConfig) {
	type knownAgent struct {
		Emoji          string
		Color          string
		Aliases        []string
		LaneKeywords   []string
		DetectKeywords []string
		BeadRole       string
		SortOrder      int
		IncludeRepos   bool
	}

	known := map[string]knownAgent{
		"scanner": {
			Emoji: "🔍", Color: "#3498db", Aliases: []string{"sc"},
			LaneKeywords:   []string{"bug", "triage", "typo", "fix"},
			DetectKeywords: []string{"scanner", "triage", "issue", "bug"},
			BeadRole:       "worker", SortOrder: 20, IncludeRepos: true,
		},
		"ci-maintainer": {
			Emoji: "🔧", Color: "#2ecc71", Aliases: []string{"ci"},
			LaneKeywords:   []string{"workflow-failure", "ci-failure", "nightly", "coverage", "regression", "ga4", "analytics"},
			DetectKeywords: []string{"ci-maintainer", "review", "ci", "coverage", "ga4"},
			BeadRole:       "worker", SortOrder: 30, IncludeRepos: true,
		},
		"architect": {
			Emoji: "🏗", Color: "#9b59b6", Aliases: []string{"ar"},
			LaneKeywords:   []string{"rfc", "architecture", "refactor", "redesign", "migration", "breaking change", "protocol", "api design"},
			DetectKeywords: []string{"architect", "rfc", "refactor"},
			BeadRole:       "worker", SortOrder: 40, IncludeRepos: true,
		},
		"outreach": {
			Emoji: "🌐", Color: "#e67e22", Aliases: []string{"ou"},
			LaneKeywords:   []string{"adopters", "outreach", "community", "engagement"},
			DetectKeywords: []string{"outreach", "adopters", "community"},
			BeadRole:       "worker", SortOrder: 50, IncludeRepos: false,
		},
		"supervisor": {
			Emoji: "👑", Color: "#e74c3c", Aliases: []string{"su"},
			DetectKeywords: []string{"supervisor", "sweep", "monitor"},
			BeadRole:       "supervisor", SortOrder: 10, IncludeRepos: true,
		},
		"sec-check": {
			Emoji: "🛡", Color: "#1abc9c", Aliases: []string{"se"},
			DetectKeywords: []string{"security", "sec-check", "vulnerability"},
			BeadRole:       "worker", SortOrder: 60, IncludeRepos: true,
		},
		"telemetry": {
			Emoji: "📡", Color: "#00a8cc", Aliases: []string{"tm"},
			LaneKeywords:   []string{"observability", "opentelemetry", "prometheus", "grafana", "tracing", "metrics", "structured-logging", "servicemonitor", "podmonitor"},
			DetectKeywords: []string{"telemetry", "observability", "opentelemetry", "prometheus"},
			BeadRole:       "worker", SortOrder: 65, IncludeRepos: true,
		},
		"operations": {
			Emoji: "🚨", Color: "#d35400", Aliases: []string{"op"},
			LaneKeywords:   []string{"healthz", "readyz", "readiness", "slo-", "sli-", "service-level-objective", "service-level-indicator", "error-budget", "runbook", "incident-response", "rollback", "alerting"},
			DetectKeywords: []string{"operations", "operability", "healthz", "runbook"},
			BeadRole:       "worker", SortOrder: 66, IncludeRepos: true,
		},
		"quality": {
			Emoji: "🧪", Color: "#3498db", Aliases: []string{"te", "qa"},
			LaneKeywords:   []string{"test-gap", "test-strategy", "test-coverage", "test-scaffold", "untested", "missing-tests"},
			DetectKeywords: []string{"quality", "test", "coverage"},
			BeadRole:       "worker", SortOrder: 35, IncludeRepos: true,
		},
		"strategist": {
			Emoji: "🧠", Color: "#f39c12", Aliases: []string{"sg"},
			DetectKeywords: []string{"strategist", "strategy"},
			BeadRole:       "worker", SortOrder: 70, IncludeRepos: true,
		},
		"guide": {
			Emoji: "📖", Color: "#8e44ad", Aliases: []string{"gu"},
			LaneKeywords:   []string{"docs", "documentation", "readme", "guide", "tutorial", "onboarding"},
			DetectKeywords: []string{"guide", "docs", "documentation"},
			BeadRole:       "worker", SortOrder: 45, IncludeRepos: true,
		},
	}

	k, ok := known[name]
	if !ok {
		return
	}

	if agent.Emoji == "" {
		agent.Emoji = k.Emoji
	}
	if agent.Color == "" {
		agent.Color = k.Color
	}
	if len(agent.Aliases) == 0 && len(k.Aliases) > 0 {
		agent.Aliases = k.Aliases
	}
	if len(agent.LaneKeywords) == 0 && len(k.LaneKeywords) > 0 {
		agent.LaneKeywords = k.LaneKeywords
	}
	if len(agent.DetectKeywords) == 0 && len(k.DetectKeywords) > 0 {
		agent.DetectKeywords = k.DetectKeywords
	}
	if agent.BeadRole == "" {
		agent.BeadRole = k.BeadRole
	}
	if agent.SortOrder == 0 {
		agent.SortOrder = k.SortOrder
	}
	if agent.IncludeRepos == nil {
		v := k.IncludeRepos
		agent.IncludeRepos = &v
	}
}

// InferenceBackends is the canonical list of model-gateway / inference backend
// IDs. It lives in the config package (a leaf in the import graph) so the
// agent and proxy packages can share it without an import cycle
// (proxy → agent → config).
//
// These are NOT agentic CLIs. Every one of them names a model endpoint that
// hive fronts with its own OpenAI-compatible translator and drives with the
// claude CLI; auth is an API key (or, for watsonx, an IAM bearer minted from
// one) supplied by config, never an interactive login.
//
// "watsonx" is here because IBM watsonx.ai is a model gateway in exactly that
// sense. It was previously supported ONLY as a `gateways:` kind and as an
// onboarding UI option, while the agent launcher had no case for it — so
// `backend: watsonx` was accepted by config and then rejected hours later at
// kick time with "unknown backend: watsonx". Anything that enumerates gateway
// backends must read THIS list rather than repeating the members inline.
var InferenceBackends = []string{"vllm", "llm-d", "litellm", "watsonx"}

// IsInferenceBackend returns true if the backend name is a self-hosted
// inference backend rather than a CLI tool.
func IsInferenceBackend(backend string) bool {
	for _, b := range InferenceBackends {
		if b == backend {
			return true
		}
	}
	return false
}

// CLIBackends is the canonical list of agentic-CLI agent backends — backends
// that launch a real coding CLI binary rather than routing to a model gateway.
//
// This exists so the CONFIG VALIDATOR and the LAUNCHER dispatch on one list.
// They used to keep separate inline sets, which is the drift that let
// `backend: watsonx` be accepted at config-set time and then fail at kick time
// with "unknown backend: watsonx". Adding a backend in one place only is now a
// visible omission rather than a silent accept-then-fail.
//
// `agy` is the Antigravity CLI, Google's replacement for the Gemini CLI. Google
// consolidated its CLI tooling under Antigravity at I/O 2026 and Gemini CLI
// STOPPED SERVING personal and Google AI Pro accounts on 2026-06-18; only
// customers holding paid Gemini Code Assist licences can still invoke it. The
// `gemini` entry is therefore retained for those licence holders, but a hive
// whose Google access is a Gemini/AI Pro subscription can only reach Google
// through `agy`. It was already listed in config/backends.conf's
// KNOWN_BACKENDS, so omitting it here reproduced exactly the accept-in-one-
// place drift this list exists to prevent — except inverted: valid in the
// shell config, rejected by the hub.
//
// TestShellAndGoCLIBackendListsAgree (backend_list_parity_test.go) asserts
// this list against config/backends.conf's KNOWN_BACKENDS, with a closed,
// commented set of exceptions (cliBackendExceptions in that file) for the two
// names — litellm, gemini — that are known to belong on only one side. Adding
// a backend here without also updating the shell side (or, if it genuinely
// belongs on only one side, documenting why in cliBackendExceptions) fails
// that test.
var CLIBackends = []string{"claude", "copilot", "goose", "codex", "pi", "bob", "aider", "gemini", "agy", "opencode", "kilo"}

// IsCLIBackend returns true if the backend launches an agentic CLI binary.
func IsCLIBackend(backend string) bool {
	for _, b := range CLIBackends {
		if b == backend {
			return true
		}
	}
	return false
}

// SupportedBackends returns every backend name valid independent of this
// hive's configuration: the agentic CLIs plus the model-gateway backends. A
// configured gateway NAME is additionally valid as a backend, but that is
// config-dependent and so is checked separately by the validator.
func SupportedBackends() []string {
	out := make([]string, 0, len(CLIBackends)+len(InferenceBackends))
	out = append(out, CLIBackends...)
	out = append(out, InferenceBackends...)
	return out
}

// ValidateBackend reports whether backend is a usable agent backend for this
// config, returning a descriptive error naming the supported values when it is
// not. An empty backend is valid (it means "the hive default").
//
// This is the single gate the config write path uses so an unsupported backend
// is refused AT SET TIME with a clear message, instead of being persisted and
// surfacing later as an agent that silently never launches.
func (g GovernorConfig) ValidateBackend(backend string) error {
	if backend == "" {
		return nil
	}
	if IsCLIBackend(backend) || IsInferenceBackend(backend) {
		return nil
	}
	for _, gw := range g.ResolvedGateways() {
		if gw.Name != "" && strings.EqualFold(gw.Name, backend) {
			return nil
		}
	}
	var gatewayNames []string
	for _, gw := range g.ResolvedGateways() {
		if gw.Name != "" {
			gatewayNames = append(gatewayNames, gw.Name)
		}
	}
	msg := fmt.Sprintf("unsupported backend %q (supported: %s",
		backend, strings.Join(SupportedBackends(), ", "))
	if len(gatewayNames) > 0 {
		msg += fmt.Sprintf("; or a configured gateway name: %s", strings.Join(gatewayNames, ", "))
	} else {
		msg += "; or the name of a gateway configured under the Model Gateways tab"
	}
	return fmt.Errorf("%s)", msg)
}

func (c *Config) validate() error {
	if c.Project.Org == "" {
		return fmt.Errorf("project.org is required")
	}
	// Repos can be empty — L1 inception starts with just an idea, no repo.
	if len(c.Agents) == 0 {
		return fmt.Errorf("at least one agent must be configured")
	}
	// Deliberately a bare zero-test, NOT HasApp(): PlaceholderAppID exists
	// precisely so a hive awaiting its real App can satisfy this check and boot
	// into dashboard-only mode. Everywhere else, use HasApp().
	// A hive described by `forge:` alone satisfies this too: ResolvedAppID()
	// derives a real App ID from a known forge, so the identity is present even
	// though app_id is not written down. Without this, the end state of this
	// design — one field naming the forge, the rest derived — fails validation
	// and the spoke will not boot.
	// An EXPLICIT `forge:` satisfies this too: ResolvedAppID() derives a real
	// App ID from a known forge, so the identity is present even though app_id
	// is not written down. Deliberately keyed on Forge_ (the raw field) and not
	// Forge() — Forge() INFERS public for a blank config, which would make an
	// empty github block validate and silently boot a hive with no credentials
	// at all. Only a forge the operator actually wrote counts.
	if c.GitHub.Token == "" && c.GitHub.AppID == 0 &&
		(strings.TrimSpace(c.GitHub.Forge_) == "" || c.GitHub.ResolvedAppID() == 0) {
		return fmt.Errorf("github.token, github.app_id or github.forge is required")
	}
	if err := c.Governor.LiteLLM.Validate(); err != nil {
		return err
	}
	if normalized, err := ValidateSnapshotFrameAncestors(c.Dashboard.SnapshotFrameAncestors); err != nil {
		return err
	} else {
		c.Dashboard.SnapshotFrameAncestors = normalized
	}
	if normalized, err := ValidateDashboardPublicURL(c.Dashboard.PublicURL); err != nil {
		return err
	} else {
		c.Dashboard.PublicURL = normalized
	}
	if !ValidateThresholdScaling(c.Governor.ThresholdScaling) {
		return fmt.Errorf("governor: invalid threshold_scaling %q (must be linear, sqrt, or none)", c.Governor.ThresholdScaling)
	}
	for modeName, mode := range c.Governor.Modes {
		for agentName, cadence := range mode.Cadences {
			if err := cadence.Validate(); err != nil {
				return fmt.Errorf("governor mode %s cadence for %s: %w", modeName, agentName, err)
			}
		}
	}
	// The hive-wide default goes through the SAME gate as the per-agent field.
	// Without this a bad value here is silently normalized to off by
	// ResolveExplainModeDefault, so an operator who typed "verbose" in the
	// dashboard would see explanation stay off with nothing saying why.
	if !ValidateExplainMode(strings.TrimSpace(c.Governor.ExplainMode)) {
		return fmt.Errorf("governor: invalid explain_mode %q (must be off, brief, or full, or empty to inherit %s)", c.Governor.ExplainMode, ExplainModeEnvVar)
	}
	if !ValidateACMMIssueTracker(strings.TrimSpace(c.Governor.ACMM.IssueTracker)) {
		return fmt.Errorf("governor: invalid acmm.issue_tracker %q (must be %s or %s, or empty for %s)", c.Governor.ACMM.IssueTracker, ACMMIssueTrackerGitHub, ACMMIssueTrackerWorkSource, ACMMIssueTrackerGitHub)
	}
	for name, agent := range c.Agents {
		// One gate, shared with the config write path (dashboard agent-config
		// save) and agreeing with what the launcher can actually dispatch. A
		// configured gateway name is valid too: naming a gateway as the backend
		// routes that agent through it, matched case-insensitively to mirror
		// ResolveGateway.
		if err := c.Governor.ValidateBackend(agent.Backend); err != nil {
			return fmt.Errorf("agent %s: %w", name, err)
		}
		if !ValidateCavemanMode(agent.CavemanMode) {
			return fmt.Errorf("agent %s: invalid caveman_mode %q (must be lite, full, ultra, or wenyan)", name, agent.CavemanMode)
		}
		if !ValidateExplainMode(agent.ExplainMode) {
			return fmt.Errorf("agent %s: invalid explain_mode %q (must be off, brief, or full, or empty to inherit %s)", name, agent.ExplainMode, ExplainModeEnvVar)
		}
		if err := validateChannels(name, agent.Channels); err != nil {
			return err
		}
		if err := validateTools(name, agent.Tools); err != nil {
			return err
		}
		if err := validateConnections(name, agent.Connections); err != nil {
			return err
		}
	}
	return nil
}

func validateChannels(agentName string, channels []ChannelConfig) error {
	validTypes := map[string]bool{"kick": true, "webhook": true, "discord": true, "schedule": true, "bead": true}
	for i, ch := range channels {
		if !validTypes[ch.Type] {
			return fmt.Errorf("agent %s: channel[%d]: invalid type %q", agentName, i, ch.Type)
		}
		switch ch.Type {
		case "webhook":
			if len(ch.Events) == 0 {
				return fmt.Errorf("agent %s: channel[%d]: webhook requires at least one event", agentName, i)
			}
		case "discord":
			if len(ch.Patterns) == 0 {
				return fmt.Errorf("agent %s: channel[%d]: discord requires at least one pattern", agentName, i)
			}
		case "schedule":
			if ch.Schedule == "" {
				return fmt.Errorf("agent %s: channel[%d]: schedule requires a cron expression", agentName, i)
			}
		case "bead":
			if len(ch.Match) == 0 {
				return fmt.Errorf("agent %s: channel[%d]: bead requires at least one match criterion", agentName, i)
			}
		}
	}
	return nil
}

func validateTools(agentName string, tools *ToolsConfig) error {
	if tools == nil {
		return nil
	}
	validPresets := map[string]bool{"": true, "advisory": true, "issues-only": true, "issues-prs": true, "full": true}
	if !validPresets[tools.Preset] {
		return fmt.Errorf("agent %s: tools.preset %q is invalid (must be advisory, issues-only, issues-prs, or full)", agentName, tools.Preset)
	}
	validActions := map[string]bool{"allow": true, "deny": true}
	for i, rule := range tools.Rules {
		if rule.Pattern == "" {
			return fmt.Errorf("agent %s: tools.rules[%d]: pattern is required", agentName, i)
		}
		if !validActions[rule.Action] {
			return fmt.Errorf("agent %s: tools.rules[%d]: action must be allow or deny, got %q", agentName, i, rule.Action)
		}
	}
	return nil
}

func validateConnections(agentName string, conns []ConnectionConfig) error {
	validTypes := map[string]bool{"mcp": true, "api": true, "knowledge": true}
	seen := map[string]bool{}
	for i, conn := range conns {
		if conn.Name == "" {
			return fmt.Errorf("agent %s: connections[%d]: name is required", agentName, i)
		}
		if seen[conn.Name] {
			return fmt.Errorf("agent %s: connections[%d]: duplicate name %q", agentName, i, conn.Name)
		}
		seen[conn.Name] = true
		if !validTypes[conn.Type] {
			return fmt.Errorf("agent %s: connections[%d]: invalid type %q (must be mcp, api, or knowledge)", agentName, i, conn.Type)
		}
		if (conn.Type == "mcp" || conn.Type == "api") && conn.URI == "" {
			return fmt.Errorf("agent %s: connections[%d]: %s requires a uri", agentName, i, conn.Type)
		}
		if conn.Auth != nil {
			validAuthTypes := map[string]bool{"env": true, "file": true}
			if !validAuthTypes[conn.Auth.Type] {
				return fmt.Errorf("agent %s: connections[%d]: auth.type must be env or file, got %q", agentName, i, conn.Auth.Type)
			}
			if conn.Auth.Type == "env" && conn.Auth.EnvVar == "" {
				return fmt.Errorf("agent %s: connections[%d]: auth.env_var is required when auth.type is env", agentName, i)
			}
			if conn.Auth.Type == "file" && conn.Auth.File == "" {
				return fmt.Errorf("agent %s: connections[%d]: auth.file is required when auth.type is file", agentName, i)
			}
		}
	}
	return nil
}

// BaseAgentName returns the declared/base agent name for name. For ordinary
// agents it returns name; for materialized replicas (scanner-2) it returns the
// source agent (scanner).
func (c *Config) BaseAgentName(name string) string {
	if c == nil || c.Agents == nil {
		return name
	}
	if ac, ok := c.Agents[name]; ok && ac.ReplicaOf != "" {
		return ac.ReplicaOf
	}
	return name
}

func replicaAgentName(base string, index int) string {
	if index <= 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, index)
}

func normalizeReplicaCount(n int) int {
	if n == 0 {
		return 1
	}
	return n
}

// ExpandAgentReplicas materializes each declared agent's replicas setting into
// the existing name-keyed machinery: base, base-2, ..., base-N. Derived agents
// are marked with ReplicaOf and are stripped again when the config is saved.
func (c *Config) ExpandAgentReplicas() error {
	if c == nil || c.Agents == nil {
		return nil
	}
	baseAgents := make(map[string]AgentConfig, len(c.Agents))
	for name, agent := range c.Agents {
		if agent.ReplicaOf != "" {
			continue
		}
		baseAgents[name] = agent
	}
	for name, agent := range c.Agents {
		if agent.ReplicaOf != "" {
			delete(c.Agents, name)
		}
	}
	for name, agent := range baseAgents {
		replicas := normalizeReplicaCount(agent.Replicas)
		if replicas < 1 || replicas > MaxAgentReplicas {
			return fmt.Errorf("agent %s: replicas must be between 1 and %d", name, MaxAgentReplicas)
		}
		for i := 2; i <= replicas; i++ {
			derived := replicaAgentName(name, i)
			if existing, ok := baseAgents[derived]; ok && existing.ReplicaOf == "" {
				return fmt.Errorf("agent %s: replicas:%d would create %s, but an agent with that name already exists", name, replicas, derived)
			}
		}
	}
	for name, agent := range baseAgents {
		replicas := normalizeReplicaCount(agent.Replicas)
		agent.Replicas = replicas
		agent.ReplicaOf = ""
		agent.ReplicaIndex = 1
		agent.ReplicaCount = replicas
		agent.name = name
		c.Agents[name] = agent
		for i := 2; i <= replicas; i++ {
			derivedName := replicaAgentName(name, i)
			derived := agent
			derived.name = derivedName
			derived.ID = derivedName
			derived.BeadsDir = fmt.Sprintf("/data/beads/%s", derivedName)
			derived.Role = agent.Role
			derived.ReplicaOf = name
			derived.ReplicaIndex = i
			derived.ReplicaCount = replicas
			c.Agents[derivedName] = derived
		}
	}
	for name, agent := range c.Agents {
		if agent.ReplicaOf == "" {
			continue
		}
		if _, ok := baseAgents[agent.ReplicaOf]; !ok {
			delete(c.Agents, name)
		}
	}
	return nil
}

// MarshalYAML persists only declared agents. Runtime-derived replicas are
// re-created by ExpandAgentReplicas on the next load so they never collide with
// their base agent's replicas setting after a save.
func (c Config) MarshalYAML() (interface{}, error) {
	type plain Config
	out := plain(c)
	if c.Agents != nil {
		out.Agents = make(map[string]AgentConfig, len(c.Agents))
		for name, agent := range c.Agents {
			if agent.ReplicaOf != "" {
				continue
			}
			agent.ReplicaIndex = 0
			agent.ReplicaCount = 0
			out.Agents[name] = agent
		}
	}
	return out, nil
}

func (c *Config) EnabledAgents() map[string]AgentConfig {
	result := make(map[string]AgentConfig)
	for name, agent := range c.Agents {
		if agent.Enabled {
			result[name] = agent
		}
	}
	return result
}

// ResolveAgent finds an agent by name or ID and returns its YAML key (name).
// Returns the key and true if found, empty string and false otherwise.
func (c *Config) ResolveAgent(nameOrID string) (string, bool) {
	if _, ok := c.Agents[nameOrID]; ok {
		return nameOrID, true
	}
	for name, agent := range c.Agents {
		if agent.ID == nameOrID {
			return name, true
		}
	}
	return "", false
}

// AgentByID returns the agent config with the given ID.
func (c *Config) AgentByID(id string) (AgentConfig, bool) {
	for _, agent := range c.Agents {
		if agent.ID == id {
			return agent, true
		}
	}
	return AgentConfig{}, false
}

// validateSaveGuard checks that essential fields are present before allowing
// a config write. This prevents docker compose down -v (or similar) from
// causing Save() to overwrite hive.yaml with an empty/minimal config that
// would crash-loop on next startup.
func (c *Config) validateSaveGuard() error {
	if c.Project.Org == "" {
		log.Printf("WARNING: config.Save() blocked — project.org is empty, would corrupt hive.yaml")
		return fmt.Errorf("project.org is empty")
	}
	// Zero agents is a legitimate state when the operator deliberately deleted
	// them all: #2361's tombstones (RemovedAgents) are the durable record of
	// that intent. Blocking the save here would make the last deletion
	// unpersistable — the in-memory roster empties, the write is refused, and
	// the next reload restores the agents from the seed, silently undoing the
	// operator's action. That is precisely the "they always come back" bug
	// #2361 fixed, reintroduced through the save path.
	//
	// An empty roster with NO tombstones is still refused: that is the
	// truncated/uninitialised case this guard exists to catch. The two states
	// are distinguishable, so distinguish them rather than rejecting both.
	if len(c.Agents) == 0 && len(c.RemovedAgents) == 0 {
		log.Printf("WARNING: config.Save() blocked — no agents configured and no tombstones, would corrupt hive.yaml")
		return fmt.Errorf("no agents configured")
	}
	return nil
}

// Save marshals the current config back to its source YAML file using an
// inode-preserving write (open → truncate → write → sync). This is critical
// for Docker bind-mounted files: an atomic rename (temp + rename) replaces
// the inode, which silently breaks the bind mount — the host file is never
// updated, so changes are lost on container restart.
//
// As a safety measure, Save refuses to write if essential fields are missing
// (project.org, at least one agent). This prevents an empty or minimal config
// from overwriting the bind-mounted hive.yaml — a scenario that causes
// crash-loops on the next startup ("project.org is required").
func (c *Config) Save() error {
	saveMu.Lock()
	defer saveMu.Unlock()
	return c.saveLocked()
}

// SetAgentPausedAndSave atomically updates one agent's Paused field and
// persists the config, all under saveMu. This is the pause-callback path
// (AgentMgr.Pause/Resume). Doing the c.Agents read-modify-write and the Save
// under the SAME lock as every other saver eliminates both the map-mutation
// race (two goroutines writing c.Agents) and the file-level lost-write race.
// Returns whether a change was made (false when already at the target state).
func (c *Config) SetAgentPausedAndSave(name string, paused bool) (bool, error) {
	saveMu.Lock()
	defer saveMu.Unlock()
	ac, ok := c.Agents[name]
	if !ok || ac.Paused == paused {
		return false, nil
	}
	ac.Paused = paused
	c.Agents[name] = ac
	return true, c.saveLocked()
}

// ReconcilePausedAndSave sets each named agent's Paused field to the given
// live value and persists, all under saveMu. This is the async PersistFunc
// path (persistState): it carries the authoritative live paused set from the
// agent manager, so its write is a correcting one rather than a stale snapshot
// that could clobber a concurrent pause. Serializing it with SetAgentPausedAndSave
// under saveMu is what closes the race that dropped pauses when many agents
// were paused in quick succession.
func (c *Config) ReconcilePausedAndSave(livePaused map[string]bool) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	for name, paused := range livePaused {
		if ac, ok := c.Agents[name]; ok && ac.Paused != paused {
			ac.Paused = paused
			c.Agents[name] = ac
		}
	}
	return c.saveLocked()
}

// saveLocked performs the actual marshal-and-write. Callers MUST hold saveMu.
func (c *Config) saveLocked() error {
	if c.SourcePath == "" {
		return fmt.Errorf("config has no source path")
	}
	if err := c.validateSaveGuard(); err != nil {
		return fmt.Errorf("refusing to save invalid config: %w", err)
	}
	data, err := yaml.Marshal(c.redactedForPersist())
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	// Open the existing file (preserving its inode) rather than creating a
	// temp file and renaming. Rename breaks Docker bind mounts because it
	// replaces the inode — the host file is never updated, so acmm_level
	// and other runtime changes are lost on container restart.
	//
	// #3961: a source-path failure must NOT abort the save. On deployments
	// that mount the config read-only (a ConfigMap mounted straight at
	// /etc/hive/hive.yaml — the issue's k3s case), this write can NEVER
	// succeed, and returning here skipped exactly the two layers that DO
	// survive a pod restart: the PVC runtime config (which the entrypoint
	// boots from in both K8s steady state and Docker/LXC) and the dashboard
	// overlay (the K8s first-boot/reprovision merge input). The old early
	// return therefore made every runtime change — pause state, operator
	// model/backend ownership, ACMM level, gateway saves — evaporate on
	// every restart, while spamming "failed to persist" on every save.
	// Record the failure, keep writing the durable layers, and report
	// success iff the state will actually survive a restart.
	var srcErr error
	f, err := os.OpenFile(c.SourcePath, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		// File may not exist yet — fall back to create. Continue below so
		// the PVC backup and dashboard overlay are still written.
		if writeErr := os.WriteFile(c.SourcePath, data, 0o644); writeErr != nil {
			srcErr = fmt.Errorf("writing config (create fallback): %w", writeErr)
		}
	} else {
		if _, err := f.Write(data); err != nil {
			_ = f.Close() // best-effort cleanup; the write error is what's recorded
			srcErr = fmt.Errorf("writing config: %w", err)
		} else if err := f.Sync(); err != nil {
			_ = f.Close() // best-effort cleanup; the sync error is what's recorded
			srcErr = fmt.Errorf("syncing config: %w", err)
		} else if err := f.Close(); err != nil {
			srcErr = fmt.Errorf("closing config: %w", err)
		}
	}

	// Persist the runtime config to the PVC. In K8s this is a recovery copy
	// (the ConfigMap seed plus the overlay is authoritative); in Docker/LXC
	// it IS the boot-time source of truth, since there is no ConfigMap and
	// no overlay there. The entrypoint decides which applies.
	//
	// Always written under the new name. The legacy file is never written,
	// renamed or removed here — see RuntimeConfigFileLegacy.
	runtimePath := RuntimeConfigFile
	var runtimeErr error
	if err := os.WriteFile(runtimePath, data, 0o644); err != nil {
		// Common cause: init container created the file as root, runtime user
		// can't overwrite. Remove and retry so runtime state is not silently lost.
		_ = os.Remove(runtimePath) // best-effort; the retry's own WriteFile error is what's recorded below
		if retryErr := os.WriteFile(runtimePath, data, 0o644); retryErr != nil {
			runtimeErr = retryErr
			log.Printf("[config] warning: failed to write PVC runtime config to %s (even after remove): %v", runtimePath, retryErr)
		} else {
			log.Printf("[config] PVC runtime config written to %s (recovered from permission error)", runtimePath)
		}
	} else {
		log.Printf("[config] PVC runtime config written to %s", runtimePath)
	}

	overlayErr := c.saveDashboardOverlay()

	if srcErr == nil {
		return nil
	}
	// The primary config path failed — a read-only mount, not a transient
	// error, in every observed case. When the boot-durable layers were both
	// written (the overlay write is a no-op outside Kubernetes), the state
	// WILL survive a restart, so this save has done its job: say so once per
	// failure mode instead of letting every caller raise a false
	// "will be lost on restart" alert on every save.
	if runtimeErr == nil && overlayErr == nil {
		log.Printf("[config] primary config path %s is not writable (%v) — state persisted to the PVC layers instead and will survive restarts (see RuntimeConfigFile)", c.SourcePath, srcErr)
		return nil
	}
	return srcErr
}

// RuntimeConfigFile is where Save() persists the full runtime config on the
// PVC. Its role differs by environment, which is exactly why the old
// hive.yaml.bak name was misleading enough to cost debugging time:
//
//   - Kubernetes: a post-merge SNAPSHOT. The entrypoint writes it after
//     merging the dashboard overlay over the ConfigMap seed, and reads it
//     back only in the disaster fallback (ConfigMap missing or empty).
//   - Docker/LXC: a live boot INPUT and the source of truth. There is no
//     ConfigMap and no overlay, so the entrypoint restores this file over
//     the config path on every boot. It is the only reason a dashboard save
//     survives a container recreation — see saveDashboardOverlay, which
//     early-returns outside Kubernetes for that reason.
//
// ".runtime" is accurate for both; ".bak" implied "the restorable backup",
// which is true only of the Kubernetes half.
// A package var (not const) only so tests can point it at a temp dir; it
// never changes at runtime in production (same convention as
// DashboardOverlayFile below).
var RuntimeConfigFile = "/data/hive.yaml.runtime"

// RuntimeConfigFileLegacy is the pre-rename name of RuntimeConfigFile.
//
// It is READ as a fallback and never written, renamed or removed: ~51 live
// hives carry only this file, and on Docker/LXC it is the single copy of
// their live configuration. Mutating it at boot could lose owner
// customisations with no warning, so the migration is copy-forward only —
// readers prefer RuntimeConfigFile and fall back to this one.
//
// Removable one release after every live hive has written the new name.
const RuntimeConfigFileLegacy = "/data/hive.yaml.bak"

// DashboardOverlayFile is where Save() persists a secret-free copy of the
// dashboard-edited config on the PVC in Kubernetes mode. The copy-config
// init container re-seeds /etc/hive/hive.yaml FROM THE CONFIGMAP on every
// pod boot, so without this overlay every dashboard save (LiteLLM
// endpoint, notifications, agent tweaks, ...) silently vanished on the
// next restart or upgrade. The entrypoint merges this file over the
// ConfigMap seed at boot; the ConfigMap stays authoritative for the
// hub/admin-managed keys (acmm_level, hub.is_public).
//
// A package var (not const) only so tests can point it at a temp dir; it
// never changes at runtime in production.
var DashboardOverlayFile = "/data/hive.yaml.dashboard"

// saTokenFile is the Kubernetes serviceaccount token path IsKubernetesPod
// probes. It is a var (not a const) only so tests can point it at a
// non-existent path and stay hermetic on hosts that really are pods;
// production always uses the fixed in-cluster path.
var saTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// SetSATokenFileForTest points IsKubernetesPod's serviceaccount-token probe
// at path and returns a restore func. Out-of-package tests that need the
// non-Kubernetes branch call this with a non-existent path (alongside
// clearing KUBERNETES_SERVICE_HOST) so they stay hermetic on hosts that
// really are pods — in-cluster CI runners and dev hives.
func SetSATokenFileForTest(path string) func() {
	orig := saTokenFile
	saTokenFile = path
	return func() { saTokenFile = orig }
}

// IsKubernetesPod reports whether the process is running inside a
// Kubernetes pod (mirrors the entrypoint's IS_KUBERNETES detection).
func IsKubernetesPod() bool {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	_, err := os.Stat(saTokenFile)
	return err == nil
}

// saveDashboardOverlay writes the secret-free PVC overlay in Kubernetes
// mode. Failures are logged, never fatal — but they ARE returned (#3961):
// when the primary config path is unwritable (read-only ConfigMap mount)
// the overlay and the runtime config are the only layers that survive a pod
// restart, so saveLocked needs to know whether this write landed before it
// can report the save as durable. Outside Kubernetes it returns nil (the
// overlay is not part of the boot path there).
//
// The write MUST be atomic (temp file + rename), unlike saveLocked()'s
// inode-preserving write to the bind-mounted primary config. DashboardOverlayFile
// lives on the PVC (not a bind mount), so rename is safe here, and it is the
// only way to avoid a truncated/partial overlay if the pod is killed mid-write
// (a redeploy sends SIGTERM/SIGKILL at an arbitrary instant). A truncate-in-place
// write (os.WriteFile) can leave the file cut off partway through — GitHubConfig
// marshals AFTER Agents/Project in the Config struct field order (see the
// struct tags above), so a truncated overlay can silently keep valid
// project/agents blocks while losing app_id/installation_id/key_file entirely.
// The entrypoint's merge script only sanity-checks project.org and agents
// before trusting the overlay wholesale, so that truncated-but-plausible file
// would pass the guard and revert a dashboard-installed GitHub App to the
// placeholder ConfigMap seed on the next restart — exactly the durability bug
// this atomic write prevents.
func (c *Config) saveDashboardOverlay() error {
	if !IsKubernetesPod() {
		// Docker/LXC mode: RuntimeConfigFile is already the boot-time
		// source of truth there, so dashboard saves persist without an
		// overlay.
		return nil
	}
	data, err := c.dashboardOverlayBytes()
	if err != nil {
		log.Printf("[config] warning: failed to marshal dashboard overlay: %v", err)
		return err
	}
	tmpPath := DashboardOverlayFile + ".tmp"
	const overlayFileMode = 0o644
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, overlayFileMode)
	if err != nil {
		log.Printf("[config] warning: failed to open dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close() // best-effort cleanup; the write error is what's returned
		log.Printf("[config] warning: failed to write dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close() // best-effort cleanup; the sync error is what's returned
		log.Printf("[config] warning: failed to fsync dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	if err := f.Close(); err != nil {
		log.Printf("[config] warning: failed to close dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	if err := os.Rename(tmpPath, DashboardOverlayFile); err != nil {
		log.Printf("[config] warning: failed to rename dashboard overlay into place %s (dashboard saves will not survive pod restarts): %v", DashboardOverlayFile, err)
		return err
	}
	log.Printf("[config] dashboard overlay written to %s (merged over the ConfigMap seed at next boot)", DashboardOverlayFile)
	return nil
}

// dashboardOverlayBytes marshals the config with env-derived secret VALUES
// collapsed back to their env-var forms, so the PVC overlay stays
// secret-free. Load() re-expands ${VAR} references and applyBootstrapEnv
// re-fills the dashboard auth token from the pod env, so nothing is lost.
func (c *Config) dashboardOverlayBytes() ([]byte, error) {
	// Shallow copy: top-level fields are struct values, so mutating the
	// copy's GitHub/Dashboard sections leaves the live config untouched
	// (the shared Agents map is not modified).
	cp := *c
	if tok := os.Getenv("HIVE_GITHUB_TOKEN"); tok != "" && cp.GitHub.Token == tok {
		cp.GitHub.Token = "${HIVE_GITHUB_TOKEN}"
	}
	for _, env := range []string{"DASHBOARD_AUTH_TOKEN", "HIVE_DASHBOARD_TOKEN"} {
		if v := os.Getenv(env); v != "" && cp.Dashboard.AuthToken == v {
			cp.Dashboard.AuthToken = ""
			break
		}
	}
	cp = *cp.redactedForPersist()
	return yaml.Marshal(&cp)
}

func (c *Config) redactedForPersist() *Config {
	if c == nil {
		return nil
	}
	cp := *c
	cp.OTel.Headers = envRedactedHeaders(cp.OTel.Headers)
	cp.Tracing.Headers = envRedactedHeaders(cp.Tracing.Headers)
	// Work-source credentials are persisted verbatim. The dashboard PUT stores
	// the operator's literal `${LINEAR_API_KEY}` reference (API saves are not
	// env-expanded) and worksource.FromConfig resolves it at the point of use,
	// so the reference round-trips through the overlay unchanged. Do NOT try
	// to "fold" a value back into ${VAR} by scanning the environment: any env
	// value that is a substring of the key (CI's ACCEPT_EULA=Y rewrote the
	// trailing Y of the literal reference) corrupts it.
	// #4041: never write the built-in login-pattern defaults as explicit
	// values. applyDefaults fills LoginPatterns on load, so by save time the
	// in-memory list always LOOKS explicit; marshaling it pins today's
	// defaults into the persisted config, where "defaults only apply to an
	// empty list" freezes them forever — exactly how every pre-#3959 hive
	// ended up stuck with the false-positive-prone generic list. A list equal
	// to the current defaults expresses no operator intent: persist it as
	// absent so future default fixes reach existing hives. An operator-
	// customized list differs from the defaults and is persisted verbatim.
	if stringSlicesEqual(cp.Governor.Sensing.LoginPatterns, defaultLoginPatterns) {
		cp.Governor.Sensing.LoginPatterns = nil
	}
	return &cp
}

func mergeOTelOverride(base, override OTelConfig) OTelConfig {
	merged := base
	if override.Enabled {
		merged.Enabled = true
	}
	if override.Endpoint != "" {
		merged.Endpoint = override.Endpoint
	}
	if len(override.Headers) > 0 {
		merged.Headers = override.Headers
	}
	if override.ServiceName != "" {
		merged.ServiceName = override.ServiceName
	}
	if override.Insecure {
		merged.Insecure = true
	}
	if override.SampleRatio != 0 {
		merged.SampleRatio = override.SampleRatio
	}
	return merged
}

func envRedactedHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return headers
	}
	out := make(map[string]string, len(headers))
	for k, value := range headers {
		out[k] = redactEnvExpandedValue(value)
	}
	return out
}

func redactEnvExpandedValue(value string) string {
	type envValue struct {
		name  string
		value string
	}
	values := make([]envValue, 0)
	for _, pair := range os.Environ() {
		name, val, ok := strings.Cut(pair, "=")
		if !ok || val == "" {
			continue
		}
		values = append(values, envValue{name: name, value: val})
	}
	sort.SliceStable(values, func(i, j int) bool {
		return len(values[i].value) > len(values[j].value)
	})

	redacted := value
	for _, item := range values {
		redacted = strings.ReplaceAll(redacted, item.value, "${"+item.name+"}")
	}
	return redacted
}

// WildcardMatch checks if text matches a pattern supporting:
// - * wildcards (match any substring)
// - /regex/ syntax for full regex
// - plain substring match (case-insensitive)
func WildcardMatch(text, pattern string) bool {
	text = strings.ToLower(text)
	pattern = strings.TrimSpace(pattern)

	if strings.HasPrefix(pattern, "/") && strings.HasSuffix(pattern, "/") {
		re, err := regexp.Compile("(?i)" + pattern[1:len(pattern)-1])
		if err != nil {
			return false
		}
		return re.MatchString(text)
	}

	pattern = strings.ToLower(pattern)
	if strings.Contains(pattern, "*") {
		parts := strings.Split(pattern, "*")
		idx := 0
		for _, part := range parts {
			if part == "" {
				continue
			}
			found := strings.Index(text[idx:], part)
			if found < 0 {
				return false
			}
			idx += found + len(part)
		}
		if !strings.HasPrefix(pattern, "*") && !strings.HasPrefix(text, parts[0]) {
			return false
		}
		if !strings.HasSuffix(pattern, "*") && !strings.HasSuffix(text, parts[len(parts)-1]) {
			return false
		}
		return true
	}

	return strings.Contains(text, pattern)
}

// MatchesAny returns true if text matches any pattern in the list.
func MatchesAny(text string, patterns []string) bool {
	for _, p := range patterns {
		if WildcardMatch(text, p) {
			return true
		}
	}
	return false
}

// Contribute filter modes for title/author/label gating.
const (
	// FilterModeDeny (the default) skips items that MATCH the list; everything
	// else passes.
	FilterModeDeny = "deny"
	// FilterModeAllow passes ONLY items that MATCH the list; everything else is
	// skipped. An EMPTY allow list is treated as "filter off" (see
	// FilterPasses) so a half-configured allow filter never silently blocks
	// every item.
	FilterModeAllow = "allow"
)

// NormalizeFilterMode returns a valid mode, defaulting to deny for empty/unknown
// values (backward compatible: existing config has no mode field, and its lists
// were always deny lists).
func NormalizeFilterMode(mode string) string {
	if mode == FilterModeAllow {
		return FilterModeAllow
	}
	return FilterModeDeny
}

// FilterPasses reports whether a single value (an issue/PR title, author, or one
// of its labels) passes a mode+list filter.
//
//   - deny  mode: pass unless value matches the list.
//   - allow mode: pass only if value matches the list; BUT an empty allow list
//     means "not configured" → pass (never block everything on an empty list).
//
// Patterns use the same wildcard/regex syntax as MatchesAny.
func FilterPasses(value string, list []string, mode string) bool {
	switch NormalizeFilterMode(mode) {
	case FilterModeAllow:
		if len(list) == 0 {
			return true // allow filter not configured → don't gate
		}
		return MatchesAny(value, list)
	default: // deny
		return !MatchesAny(value, list)
	}
}

// LabelsFilterPasses applies a label filter across ALL of an item's labels.
//
//   - deny  mode: pass unless ANY label matches the deny list.
//   - allow mode: pass only if AT LEAST ONE label matches the allow list; an
//     empty allow list means "not configured" → pass.
//
// This is the label-specific counterpart to FilterPasses (labels are a set, not
// a single scalar, so the allow/deny quantifiers differ).
func LabelsFilterPasses(labels []string, list []string, mode string) bool {
	switch NormalizeFilterMode(mode) {
	case FilterModeAllow:
		if len(list) == 0 {
			return true // allow filter not configured → don't gate
		}
		for _, l := range labels {
			if MatchesAny(l, list) {
				return true
			}
		}
		return false
	default: // deny
		for _, l := range labels {
			if MatchesAny(l, list) {
				return false
			}
		}
		return true
	}
}

// EscalationConfig tunes the fix-loop circuit breaker (pkg/escalation): after
// Threshold distinct failed fix attempts on the same agent-authored PR, the
// hub stops dispatching further fixes and escalates to a human with the raw
// CI failure evidence.
type EscalationConfig struct {
	// Disabled turns the breaker off entirely (the zero value keeps it on —
	// escalation is a safety net and must be opt-out, not opt-in).
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	// Threshold is the distinct-red-attempt count that triggers escalation.
	// Zero means DefaultEscalationThreshold.
	Threshold int `yaml:"threshold,omitempty" json:"threshold,omitempty"`
}

// ReviewConfig gates the optional structured review-swarm merge gate. The zero
// value preserves existing behavior: merge eligibility does not require a
// review-verdicts.json aggregate approval unless an operator opts in.
type ReviewConfig struct {
	RequireApproval    bool     `yaml:"require_approval,omitempty" json:"require_approval,omitempty"`
	FanOut             bool     `yaml:"fan_out,omitempty" json:"fan_out,omitempty"`
	MaxParallelReviews int      `yaml:"max_parallel_reviews,omitempty" json:"max_parallel_reviews,omitempty"`
	ReviewerAgents     []string `yaml:"reviewer_agents,omitempty" json:"reviewer_agents,omitempty"`
	FixerAgent         string   `yaml:"fixer_agent,omitempty" json:"fixer_agent,omitempty"`
}

// AutoMergeConfig gates the App-self-merge sweep (SweepSelfAuthoredAutoMerges).
// Prow structurally forbids self-approval on a PR's own author, so a PR the
// Forge App itself opens can never collect the lgtm+approved labels tide
// requires — nobody but the App authored it, and the App cannot review its
// own work. The sweep is Hive's only route to landing such a PR: it merges
// directly over the GitHub REST API (squash), bypassing tide entirely,
// exactly as the existing human-queue sweep already does for tide-pending/
// unstable states (see mergeableFromState). This block controls ONLY that
// self-authored path; the human "Approved ... for Hive auto-merge" queue
// (governor.labels.automerge) is untouched and still requires a distinct
// human queuer.
type AutoMergeConfig struct {
	// SelfAuthored enables SweepSelfAuthoredAutoMerges: the App merges its own
	// open, CI-green PRs without a human queue-approval review, at every
	// intent tier (including Tier3, which normally requires HumanApproval —
	// see intent.Evaluate). *bool so "unset" is distinguishable from an
	// explicit false: default is ON, matching the operator intent that the
	// App should never be blocked behind a review it is structurally unable
	// to obtain for its own PRs. Set `auto_merge.self_authored: false` to
	// disable and fall back to fully manual merges for App PRs.
	SelfAuthored *bool `yaml:"self_authored,omitempty" json:"self_authored,omitempty"`
	// MaxMerges caps merges per sweep pass, shared semantics with
	// AutoMergeSweepOptions.MaxMerges for the human queue sweep. Zero means
	// DefaultAutoMergeSweepMaxMerges.
	MaxMerges int `yaml:"max_merges,omitempty" json:"max_merges,omitempty"`
	// RequiredChecks is the operator-declared list of status-check
	// contexts/check-run names that the self-merge sweep's commitGreen must
	// gate on, e.g. ["build-gate"]. This is the scope-free alternative to
	// asking GitHub's branch-protection API (Repositories.GetRequiredStatusChecks)
	// which one, and only one, of these checks are actually required: that
	// API needs administration:read, a scope the Hive GitHub App does not
	// hold, so the call errors and the sweep used to fail closed to the old
	// isMetaCheck/isIgnorableCICheck allowlist — which still blocked on
	// non-required checks like "Detect untested files" (cancelled) or
	// "Analyze (python)" (CodeQL failure). Declaring the branch's actual
	// required set here removes the dependency on that scope entirely.
	//
	// Unset/empty means "not config-declared" (RequiredCheckSet returns
	// requiredKnown=false) — callers then fall back to the branch-protection
	// API, and if that also cannot determine the set, to the allowlist. There
	// is deliberately no hardcoded default here: the required-checks set is
	// per-repo (e.g. console's main branch requires only "build-gate"), so
	// the operator must declare it per-hive in `auto_merge.required_checks`.
	RequiredChecks []string `yaml:"required_checks,omitempty" json:"required_checks,omitempty"`
}

// RequiredCheckSet returns the config-declared required-status-check set as a
// membership map, and whether the config actually declared one. An
// empty/unset RequiredChecks list returns (nil, false) — "not config-declared"
// — so callers can distinguish that from a genuinely empty required set (e.g.
// an unprotected branch) and fall through to their next source (the
// branch-protection API, then the allowlist fallback). A non-empty list
// always returns requiredKnown=true; entries are matched by exact string
// equality against status-context / check-run names.
func (a AutoMergeConfig) RequiredCheckSet() (map[string]bool, bool) {
	if len(a.RequiredChecks) == 0 {
		return nil, false
	}
	set := make(map[string]bool, len(a.RequiredChecks))
	for _, name := range a.RequiredChecks {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		set[name] = true
	}
	if len(set) == 0 {
		return nil, false
	}
	return set, true
}

// SelfAuthoredEnabled reports whether the App-self-merge sweep is on for this
// hive. Default ON (nil == enabled): see AutoMergeConfig.SelfAuthored.
func (a AutoMergeConfig) SelfAuthoredEnabled() bool {
	return a.SelfAuthored == nil || *a.SelfAuthored
}

// SelfMergeMinACMMLevel is the lowest ACMM level at which the App is allowed
// to self-merge its own PRs. examples/acmm/l5.md is explicit that L5 does NOT
// grant merge authority ("Merge pull requests — no; L5 PRs remain hold-gated
// for human review"), and examples/acmm/l4.md is even more explicit ("DO NOT
// merge pull requests — L4 is not an auto-merge level"). examples/acmm/l6.md
// is the first level whose Responsibilities include "Merge PRs when CI
// passes". So the gate sits at L6, not L5: an L4 (or L5) hive must never run
// the self-authored auto-merge sweep, regardless of the auto_merge.self_authored
// flag below. This was the root cause of ks/hive (acmm_level: 4) wrongly
// self-merging its own PR — the sweep previously had no ACMM check at all.
const SelfMergeMinACMMLevel = 6

// SelfAuthoredAutoMergeAllowed reports whether the App-self-merge sweep may
// run at all for this hive, given its ACMM level. Both conditions must hold:
// the auto_merge.self_authored flag must be ON (SelfAuthoredEnabled) AND the
// hive's ACMM level must be at or above SelfMergeMinACMMLevel. A nil/unset
// ACMM level is treated as NOT high enough (fail closed) — an operator who
// has not explicitly opted a hive into a high ACMM level never gets
// self-merge by accident.
func (a AutoMergeConfig) SelfAuthoredAutoMergeAllowed(acmmLevel *int) bool {
	if !a.SelfAuthoredEnabled() {
		return false
	}
	if acmmLevel == nil {
		return false
	}
	return *acmmLevel >= SelfMergeMinACMMLevel
}

// DefaultEscalationThreshold matches escalation.DefaultThreshold; duplicated
// here (a constant, checked by test) to avoid a config→escalation import.
const DefaultEscalationThreshold = 3

// DefaultMaxParallelReviews matches review.DefaultMaxParallelReviews; duplicated
// here to avoid a config→review import cycle.
const DefaultMaxParallelReviews = 5

// EffectiveMaxParallelReviews resolves the review fan-out width.
func (r ReviewConfig) EffectiveMaxParallelReviews() int {
	if r.MaxParallelReviews > 0 {
		return r.MaxParallelReviews
	}
	return DefaultMaxParallelReviews
}

// EffectiveThreshold resolves the configured threshold with its default.
func (e EscalationConfig) EffectiveThreshold() int {
	if e.Threshold > 0 {
		return e.Threshold
	}
	return DefaultEscalationThreshold
}
