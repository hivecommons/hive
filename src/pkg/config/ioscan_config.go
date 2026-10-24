package config

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/ioscan"
)

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
	// findings. "open" redacts the offending text and continues the kick;
	// "closed" blocks the kick and records an ioscan_fail_closed audit entry.
	// Empty (nil) is not a fixed default: it resolves per ACMM level via the
	// packs — closed at L5/L6 (where agents can merge), open at L1-L4 — while an
	// explicit "open"/"closed" overrides that in either direction. Read via
	// FailClosedAtLevel(), never compared raw. The tradeoff of closed-by-default
	// at L5+ is that every Critical false-positive stalls the queue item until
	// an operator intervenes.
	FailMode string `yaml:"fail_mode,omitempty" json:"fail_mode,omitempty"`
	// Canaries plants per-kick exfiltration markers in agent prompts and scans
	// agent-visible egress for leaks. Pointer so an omitted `canaries:` key
	// (nil) is distinguishable from an explicit false: nil DEFAULTS ON now that
	// CanaryRegistry.Scan is encoding-aware (#6701, #6720) and no longer misses
	// encoded egress (#6686), while an operator can still opt out with an
	// explicit `canaries: false`. Read via CanariesEnabled(), never dereferenced
	// raw.
	Canaries *bool `yaml:"canaries,omitempty" json:"canaries,omitempty"`
	// BlockAt is the per-surface severity at which an injection or dangerous
	// finding in text by an UNTRUSTED author is withheld from the kick
	// (hivecommons/hive#6685). Keys are ioscan surfaces (issue_body, comment,
	// pr_body, review); values are low/medium/high/critical. A surface left
	// out sits at the default, medium, so any rule that fires withholds the
	// text. Text by a trusted author (the hive's own identities, repo owners
	// and members) always uses the high line regardless of this map. Read via
	// Policy(), which also validates the keys and values.
	BlockAt map[string]string `yaml:"block_at,omitempty" json:"block_at,omitempty"`
	// Classifier controls the LLM-judge semantic prompt-injection classifier
	// that scores text the deterministic rules let through. Its enabled and
	// fail_mode defaults follow the ACMM level (see IoscanClassifierConfig).
	Classifier IoscanClassifierConfig `yaml:"classifier,omitempty" json:"classifier,omitempty"`
}

// IoscanClassifierConfig controls the model-based semantic injection
// classifier that runs after deterministic ioscan redaction.
//
// Enabled is a pointer so an omitted key follows the ACMM pack: on at L4-L6,
// off at L1-L3 and when no level is set. FailMode ("open" or "closed") says
// what happens when the classifier cannot answer (endpoint error, timeout,
// unparseable reply): open passes the rule-cleaned text on, closed withholds
// it. Omitted, it follows ioscan.fail_mode and so the pack: closed at L5-L6.
// Read both via EnabledAtLevel / FailClosedAtLevel, never raw.
//
// Backend names a subscription CLI (e.g. "claude") that runs the classifier
// tool-less instead of the OpenAI-compatible HTTP endpoint; empty keeps the
// HTTP path. Boot refuses a backend ioscan.SupportsToolless rejects. With a
// CLI backend, CLIBudgetPerEval replaces the per-pass budget and
// CLIDailyQuota caps calls per UTC day; zero uses the defaults. Read them
// via CLIBudgetPerEvalOrDefault / CLIDailyQuotaOrDefault.
type IoscanClassifierConfig struct {
	Enabled          *bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	FailMode         string  `yaml:"fail_mode,omitempty" json:"fail_mode,omitempty"`
	Model            string  `yaml:"model,omitempty" json:"model,omitempty"`
	Backend          string  `yaml:"backend,omitempty" json:"backend,omitempty"`
	CLIBudgetPerEval int     `yaml:"cli_budget_per_eval,omitempty" json:"cli_budget_per_eval,omitempty"`
	CLIDailyQuota    int     `yaml:"cli_daily_quota,omitempty" json:"cli_daily_quota,omitempty"`
	WarnThreshold    float64 `yaml:"warn_threshold,omitempty" json:"warn_threshold,omitempty"`
	BlockThreshold   float64 `yaml:"block_threshold,omitempty" json:"block_threshold,omitempty"`
}

// IsEnabled reports whether input/output scanning is active. Absent (nil)
// defaults to true (fail-safe): scanning is on unless an operator explicitly
// sets `enabled: false`.
func (c IoscanConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

const ioscanFailModeClosed = "closed"

const ioscanFailModeOpen = "open"

// FailClosed reports whether an explicit fail_mode setting selects closed. It
// ignores the ACMM-level default entirely (an empty FailMode is never closed
// here), so callers that need the effective, level-aware behavior must use
// FailClosedAtLevel instead. It is retained for the config-serialization and
// explicit-override paths that only care about what the operator literally set.
func (c IoscanConfig) FailClosed() bool {
	return strings.EqualFold(c.FailMode, ioscanFailModeClosed)
}

// FailClosedAtLevel reports whether Critical injection findings should block the
// whole kick (fail-closed) instead of only redacting the offending untrusted
// text (fail-open), for a hive at the given ACMM level. An explicit
// fail_mode ("open" or "closed") always wins. When unset, the default is
// resolved from the ACMM pack for the level: closed at L5/L6 (the levels where
// agents can merge), open at L1-L4. The cost of the L5+ closed default is that
// a Critical false-positive stalls the kick until an operator clears it.
func (c IoscanConfig) FailClosedAtLevel(level int) bool {
	if strings.EqualFold(c.FailMode, ioscanFailModeClosed) {
		return true
	}
	if strings.EqualFold(c.FailMode, ioscanFailModeOpen) {
		return false
	}
	if c.FailMode != "" {
		return false
	}
	return strings.EqualFold(IoscanFailModeForLevel(level), ioscanFailModeClosed)
}

// CanariesEnabled reports whether per-kick exfiltration canaries are active.
// Absent (nil) defaults to true now that the egress scan is encoding-aware
// (#6701, #6720); an operator opts out with an explicit `canaries: false`.
func (c IoscanConfig) CanariesEnabled() bool {
	return c.Canaries == nil || *c.Canaries
}

// EnabledAtLevel reports whether the classifier runs for a hive at the given
// ACMM level. An explicit enabled wins; otherwise the pack decides.
func (c IoscanClassifierConfig) EnabledAtLevel(level int) bool {
	if c.Enabled != nil {
		return *c.Enabled
	}
	return IoscanClassifierForLevel(level)
}

// CLIBackend returns the configured classifier CLI backend, trimmed; empty
// means the HTTP endpoint path.
func (c IoscanClassifierConfig) CLIBackend() string {
	return strings.TrimSpace(c.Backend)
}

// CLIBudgetPerEvalOrDefault returns the per-pass classifier budget used with
// a CLI backend.
func (c IoscanClassifierConfig) CLIBudgetPerEvalOrDefault() int {
	if c.CLIBudgetPerEval > 0 {
		return c.CLIBudgetPerEval
	}
	return ioscan.DefaultCLIClassifierBudgetPerEval
}

// CLIDailyQuotaOrDefault returns the per-UTC-day call quota used with a CLI
// backend.
func (c IoscanClassifierConfig) CLIDailyQuotaOrDefault() int {
	if c.CLIDailyQuota > 0 {
		return c.CLIDailyQuota
	}
	return ioscan.DefaultCLIClassifierDailyQuota
}

// ValidateCLI rejects CLI budget and quota values that cannot be honoured.
// The backend name itself is checked at boot, which refuses it with a log
// line rather than failing config load.
func (c IoscanClassifierConfig) ValidateCLI() error {
	if c.CLIBudgetPerEval < 0 || c.CLIBudgetPerEval > ioscan.DefaultClassifierBudgetPerEval {
		return fmt.Errorf("ioscan.classifier.cli_budget_per_eval: %d is not between 0 and %d", c.CLIBudgetPerEval, ioscan.DefaultClassifierBudgetPerEval)
	}
	if c.CLIDailyQuota < 0 {
		return fmt.Errorf("ioscan.classifier.cli_daily_quota: %d is negative", c.CLIDailyQuota)
	}
	return nil
}

// ClassifierEnabledAtLevel reports whether ioscan as a whole and the
// classifier are both on for the given ACMM level.
func (c IoscanConfig) ClassifierEnabledAtLevel(level int) bool {
	return c.IsEnabled() && c.Classifier.EnabledAtLevel(level)
}

// ClassifierFailClosedAtLevel reports whether a classifier that cannot
// answer withholds the text (closed) or passes it on (open). An explicit
// classifier.fail_mode wins; otherwise it follows FailClosedAtLevel, so an
// explicit ioscan.fail_mode and then the ACMM pack decide.
func (c IoscanConfig) ClassifierFailClosedAtLevel(level int) bool {
	if strings.EqualFold(c.Classifier.FailMode, ioscanFailModeClosed) {
		return true
	}
	if strings.EqualFold(c.Classifier.FailMode, ioscanFailModeOpen) {
		return false
	}
	return c.FailClosedAtLevel(level)
}

// Policy resolves block_at into the ioscan severity matrix. An empty map is
// the shipped default (medium on every surface for untrusted authors).
func (c IoscanConfig) Policy() (ioscan.Policy, error) {
	return ioscan.ParsePolicy(c.BlockAt)
}
