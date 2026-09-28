package config

import (
	"strings"
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
