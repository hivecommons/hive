package dashboard

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/hub"
)

// Auto-update status surface (#6962, #6963).
//
// #6962 asked for a FINDABLE place in the dashboard that shows the configured
// auto-update policy AND its current status. #6963 is the companion bug: a hive
// sat four days on an old commit while "configured for daily autoupdate", and
// nothing on the dashboard said whether auto-update was healthy, stuck, or
// merely unknown — and, critically, WHY. This builds a single explicit status
// object the dashboard renders, with one hard invariant carried by the Healthy
// field: an unknown or failed state is NEVER reported as healthy. Reporting
// unknown as healthy is exactly the bug #6963 describes.

// autoUpdateState enumerates the mutually exclusive states the dashboard can
// render. Named constants (no magic strings) keep the Go classifier and the
// frontend switch in step.
const (
	// autoUpdateStateDisabled — this hive is not hub-managed for upgrades.
	autoUpdateStateDisabled = "disabled"
	// autoUpdateStateUpToDate — running the latest built commit on the target
	// line. The ONLY healthy "green" state.
	autoUpdateStateUpToDate = "up_to_date"
	// autoUpdateStateBehind — known to be N commits behind the target and the
	// update has not landed yet.
	autoUpdateStateBehind = "behind"
	// autoUpdateStateRetrying — an instructed upgrade is in flight but has not
	// landed (the on-PVC marker is still present, below the attempt budget).
	autoUpdateStateRetrying = "retrying"
	// autoUpdateStateFailed — an instructed upgrade exhausted its retry budget.
	// The reason travels in LastError.
	autoUpdateStateFailed = "failed"
	// autoUpdateStateUnknown — we could not determine whether the hive is up to
	// date (e.g. the version comparison was unavailable). Must NOT read as
	// healthy — this is the #6963 invariant.
	autoUpdateStateUnknown = "unknown"
)

// autoUpdatePeriodUnknown is the period reported when the spoke does not know
// its own schedule. The schedule (instant/daily/weekly) is stored hub-side; a
// spoke only learns it if auto_upgrade_mode is present in its config. Rather
// than guess, an absent mode degrades to an explicit "unknown" — the same
// tolerate-older-spokes contract the rest of this surface follows.
const autoUpdatePeriodUnknown = "unknown"

// AutoUpdateStatus is the JSON contract the dashboard renders for the
// findable auto-update section. Every field is additive; older spokes that do
// not populate a field degrade to an explicit unknown rather than an error.
type AutoUpdateStatus struct {
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
	// Healthy is the single guard #6963 turns on: false for failed, behind,
	// retrying AND unknown. The frontend must never paint a non-healthy state
	// green.
	Healthy       bool   `json:"healthy"`
	Period        string `json:"period"`
	TargetBranch  string `json:"targetBranch,omitempty"`
	TargetCommit  string `json:"targetCommit,omitempty"`
	CurrentCommit string `json:"currentCommit,omitempty"`
	CommitsBehind *int   `json:"commitsBehind,omitempty"`
	LastAttemptAt string `json:"lastAttemptAt,omitempty"`
	LastError     string `json:"lastError,omitempty"`
	Attempts      int    `json:"attempts,omitempty"`
	MaxAttempts   int    `json:"maxAttempts,omitempty"`
	// Detail is a human-friendly one-liner that always explains the state and,
	// for a failure or unknown, WHY.
	Detail string `json:"detail"`
}

// autoUpdateInputs is what the dashboard already knows locally at the moment it
// answers /api/version. Passing it explicitly keeps buildAutoUpdateStatus a
// pure function that the tests exercise through the real handler.
type autoUpdateInputs struct {
	Enabled       bool
	Period        string // raw config mode (may be "")
	TargetBranch  string
	TargetCommit  string
	CurrentCommit string
	CommitsBehind *int           // nil = unknown
	Marker        map[string]any // readUpgradeMarker() output; nil when no upgrade is in flight/failing
}

// normalizeAutoUpdatePeriod maps a stored/legacy auto_upgrade_mode onto a
// concrete, displayable period. An empty or unrecognised value is reported as
// "unknown" rather than silently assumed to be instant: the spoke genuinely
// does not know its schedule, and #6963 is precisely about not dressing an
// unknown up as a definite answer.
func normalizeAutoUpdatePeriod(mode string) string {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case hub.AutoUpgradeModeInstant:
		return hub.AutoUpgradeModeInstant
	case hub.AutoUpgradeModeDaily:
		return hub.AutoUpgradeModeDaily
	case hub.AutoUpgradeModeWeekly:
		return hub.AutoUpgradeModeWeekly
	default:
		return autoUpdatePeriodUnknown
	}
}

// buildAutoUpdateStatus classifies the hive's auto-update state from what the
// spoke knows locally. The ordering matters: a failing or in-flight upgrade
// (evidenced by the on-PVC marker) outranks a stale commit count, and an
// unknown comparison outranks an assumed "up to date".
func buildAutoUpdateStatus(in autoUpdateInputs) AutoUpdateStatus {
	st := AutoUpdateStatus{
		Enabled:       in.Enabled,
		Period:        normalizeAutoUpdatePeriod(in.Period),
		TargetBranch:  in.TargetBranch,
		TargetCommit:  in.TargetCommit,
		CurrentCommit: in.CurrentCommit,
		CommitsBehind: in.CommitsBehind,
	}

	if !in.Enabled {
		st.State = autoUpdateStateDisabled
		// Disabled is a deliberate configuration, not a fault, so it is not
		// "unhealthy" — but it must never read as "up to date" either.
		st.Healthy = true
		st.Detail = "Automatic updates are turned off for this hive; new versions are not applied automatically."
		return st
	}

	// A present marker always describes an upgrade that has NOT landed (it is
	// removed on the boot that lands the new image). It therefore outranks any
	// commit-count reading.
	if in.Marker != nil {
		st.Attempts, _ = in.Marker["attempts"].(int)
		st.MaxAttempts, _ = in.Marker["maxAttempts"].(int)
		if s, ok := in.Marker["lastError"].(string); ok {
			st.LastError = s
		}
		if s, ok := in.Marker["requestedAt"].(string); ok {
			st.LastAttemptAt = s
		}
		target, _ := in.Marker["target"].(string)
		failed, _ := in.Marker["failed"].(bool)
		if failed {
			st.State = autoUpdateStateFailed
			st.Healthy = false
			why := "the target image never became the running image"
			if st.LastError != "" {
				why = st.LastError
			}
			st.Detail = fmt.Sprintf("Auto-update to %s FAILED after %d/%d attempts: %s",
				orUnknownSHA(target), st.Attempts, st.MaxAttempts, why)
			return st
		}
		st.State = autoUpdateStateRetrying
		st.Healthy = false
		st.Detail = fmt.Sprintf("Auto-update to %s is in progress (attempt %d of %d) and has not landed yet.",
			orUnknownSHA(target), st.Attempts, st.MaxAttempts)
		if st.LastError != "" {
			st.Detail += " Last error: " + st.LastError
		}
		return st
	}

	// No marker: fall back to the commit-behind reading. A nil count means the
	// comparison was unavailable — report UNKNOWN, never a green "up to date".
	if in.CommitsBehind == nil {
		st.State = autoUpdateStateUnknown
		st.Healthy = false
		st.Detail = "Could not determine whether this hive is up to date: the version comparison is unavailable. Not treating unknown as healthy."
		return st
	}
	if *in.CommitsBehind <= 0 {
		st.State = autoUpdateStateUpToDate
		st.Healthy = true
		st.Detail = "This hive is running the latest built commit" + onBranchSuffix(in.TargetBranch) + "."
		return st
	}
	st.State = autoUpdateStateBehind
	st.Healthy = false
	st.Detail = fmt.Sprintf("This hive is %d commit(s) behind%s and the update has not been applied yet.",
		*in.CommitsBehind, onBranchSuffix(in.TargetBranch))
	return st
}

func onBranchSuffix(branch string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	return " on " + branch
}

func orUnknownSHA(sha string) string {
	if strings.TrimSpace(sha) == "" {
		return "the target commit"
	}
	return sha
}
