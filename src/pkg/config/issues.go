package config

import (
	"strings"
	"time"
)

const (
	DefaultCloseOnMergeBackfillInterval = time.Hour
	// DefaultCloseOnMergeInterval is how often the close-on-merge ledger
	// sweep walks the whole claim ledger. Between sweeps only claims whose PR
	// merged since the last sweep are checked.
	DefaultCloseOnMergeInterval = 10 * time.Minute
	// DefaultCloseOnMergeRecheck is how long a ledger claim with a settled
	// close-on-merge verdict is skipped before it is re-verified once.
	DefaultCloseOnMergeRecheck = 24 * time.Hour

	// CloseOnMergeIntervalEnvVar and CloseOnMergeRecheckEnvVar override the
	// matching issues.* settings with a Go duration ("10m", "24h").
	CloseOnMergeIntervalEnvVar = "HIVE_CLOSE_ON_MERGE_INTERVAL"
	CloseOnMergeRecheckEnvVar  = "HIVE_CLOSE_ON_MERGE_RECHECK"

	// DefaultSHAHoldTrustedLabel is the soft label the SHA-hold sweep applies
	// to a trusted reporter's bug that lacks a commit SHA, instead of the hard
	// `hold` (hivecommons/hive#12002).
	DefaultSHAHoldTrustedLabel = "needs-sha"
	// DefaultHardSuppressReconcileInterval is how often the eval loop runs
	// the reconcile that lifts hive-applied `hold`/`blocked` labels whose
	// reason lapsed (hivecommons/hive#12002).
	DefaultHardSuppressReconcileInterval = 30 * time.Minute
)

type IssuesConfig struct {
	CloseOnMerge                 *bool         `yaml:"close_on_merge,omitempty" json:"close_on_merge,omitempty"`
	CloseOnMergeBackfillInterval time.Duration `yaml:"close_on_merge_backfill_interval,omitempty" json:"close_on_merge_backfill_interval,omitempty"`
	// CloseOnMergeInterval is the full-ledger sweep cadence; see
	// DefaultCloseOnMergeInterval.
	CloseOnMergeInterval time.Duration `yaml:"close_on_merge_interval,omitempty" json:"close_on_merge_interval,omitempty"`
	// CloseOnMergeRecheck is the settled-claim recheck window; see
	// DefaultCloseOnMergeRecheck.
	CloseOnMergeRecheck time.Duration `yaml:"close_on_merge_recheck,omitempty" json:"close_on_merge_recheck,omitempty"`
	// ReporterConfirmation restores the legacy human-filed-bug close gate for
	// every matching issue. Default false: issues close when their fix merges;
	// use the per-issue hive: needs-confirmation marker when only one issue
	// needs reporter verification.
	ReporterConfirmation *bool `yaml:"reporter_confirmation,omitempty" json:"reporter_confirmation,omitempty"`
	// SHAHoldTrustedLabel is the soft label the SHA-hold sweep applies to a
	// bug filed by a reporter in issue_filter.reporter_trust's trusted
	// associations (OWNER/MEMBER/COLLABORATOR by default) when no commit SHA
	// is present. Unset uses DefaultSHAHoldTrustedLabel; "hold" restores the
	// legacy hard hold for every reporter.
	SHAHoldTrustedLabel *string `yaml:"sha_hold_trusted_label,omitempty" json:"sha_hold_trusted_label,omitempty"`
	// LiftHiveHardSuppress enables the reconcile that lifts a `hold`/`blocked`
	// label hive itself applied once a PR referencing the issue has merged
	// since. Default true; a human-applied label is never lifted.
	LiftHiveHardSuppress *bool `yaml:"lift_hive_hard_suppress,omitempty" json:"lift_hive_hard_suppress,omitempty"`
	// HardSuppressReconcileInterval is the reconcile cadence; see
	// DefaultHardSuppressReconcileInterval.
	HardSuppressReconcileInterval time.Duration `yaml:"hard_suppress_reconcile_interval,omitempty" json:"hard_suppress_reconcile_interval,omitempty"`
	// RFCExistingCheck comments, once, on new RFC/feature issues with the
	// existing settings that may already cover the request
	// (hivecommons/hive#13575). Default off.
	RFCExistingCheck RFCExistingCheckConfig `yaml:"rfc_existing_check,omitempty" json:"rfc_existing_check,omitempty"`
}

// EffectiveSHAHoldTrustedLabel returns the trusted-reporter SHA label.
func (i IssuesConfig) EffectiveSHAHoldTrustedLabel() string {
	if i.SHAHoldTrustedLabel == nil {
		return DefaultSHAHoldTrustedLabel
	}
	if v := strings.TrimSpace(*i.SHAHoldTrustedLabel); v != "" {
		return v
	}
	return DefaultSHAHoldTrustedLabel
}

// LiftHiveHardSuppressEnabled reports whether the hard-suppress reconcile runs.
func (i IssuesConfig) LiftHiveHardSuppressEnabled() bool {
	if i.LiftHiveHardSuppress == nil {
		return true
	}
	return *i.LiftHiveHardSuppress
}

// EffectiveHardSuppressReconcileInterval returns the reconcile cadence.
func (i IssuesConfig) EffectiveHardSuppressReconcileInterval() time.Duration {
	if i.HardSuppressReconcileInterval <= 0 {
		return DefaultHardSuppressReconcileInterval
	}
	return i.HardSuppressReconcileInterval
}

func (i IssuesConfig) CloseOnMergeEnabled() bool {
	if i.CloseOnMerge == nil {
		return true
	}
	return *i.CloseOnMerge
}

func (i IssuesConfig) EffectiveCloseOnMergeBackfillInterval() time.Duration {
	if i.CloseOnMergeBackfillInterval <= 0 {
		return DefaultCloseOnMergeBackfillInterval
	}
	return i.CloseOnMergeBackfillInterval
}

func (i IssuesConfig) EffectiveCloseOnMergeInterval() time.Duration {
	if i.CloseOnMergeInterval <= 0 {
		return DefaultCloseOnMergeInterval
	}
	return i.CloseOnMergeInterval
}

func (i IssuesConfig) EffectiveCloseOnMergeRecheck() time.Duration {
	if i.CloseOnMergeRecheck <= 0 {
		return DefaultCloseOnMergeRecheck
	}
	return i.CloseOnMergeRecheck
}

func (i IssuesConfig) ReporterConfirmationEnabled() bool {
	return i.ReporterConfirmation != nil && *i.ReporterConfirmation
}

// applyEnv applies the close-on-merge environment overrides. getenv is
// os.Getenv in production; a malformed or non-positive duration is ignored
// so a typo falls back to the file/default value rather than disabling the
// sweep.
func (i *IssuesConfig) applyEnv(getenv func(string) string) {
	if d, ok := parsePositiveDurationEnv(getenv(CloseOnMergeIntervalEnvVar)); ok {
		i.CloseOnMergeInterval = d
	}
	if d, ok := parsePositiveDurationEnv(getenv(CloseOnMergeRecheckEnvVar)); ok {
		i.CloseOnMergeRecheck = d
	}
}

func parsePositiveDurationEnv(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}
