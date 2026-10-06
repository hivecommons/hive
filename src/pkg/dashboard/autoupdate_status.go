package dashboard

import (
	"fmt"
	"strings"
	"time"

	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

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
	// autoUpdateStatePaused — upgrades are managed but the hub's fleet-wide
	// kill switch is engaged (#7262). Deliberate, like disabled, so healthy;
	// never "up to date".
	autoUpdateStatePaused = "paused"
)

// Who applies upgrades to this hive (#7262). Reported so the Hub tab can lock
// the spoke-local toggle and say so, instead of rendering that toggle as if it
// were the policy.
const (
	autoUpdateManagedByHub   = "hub"
	autoUpdateManagedBySpoke = "spoke"
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
	Healthy bool   `json:"healthy"`
	Period  string `json:"period"`
	// ManagedBy is "hub" (the hub rolls the Deployment), "spoke" (the spoke
	// self-upgrades on hub instruction) or "" (nobody). PolicySource says where
	// that answer came from: "hub" when the heartbeat delivered it, "local" when
	// only the spoke's own config was available (older hub, or unmanaged hive).
	ManagedBy     string `json:"managedBy,omitempty"`
	PolicySource  string `json:"policySource"`
	Paused        bool   `json:"paused,omitempty"`
	TargetBranch  string `json:"targetBranch,omitempty"`
	TargetChannel string `json:"targetChannel,omitempty"`
	TargetCommit  string `json:"targetCommit,omitempty"`
	CurrentCommit string `json:"currentCommit,omitempty"`
	CommitsBehind *int   `json:"commitsBehind,omitempty"`
	LastAttemptAt string `json:"lastAttemptAt,omitempty"`
	// LastUpdatedAt is the RFC3339 time the most recent instructed upgrade
	// actually LANDED on this hive (#10038) — the answer to "when was my hive
	// last updated". It is drawn from the persisted last-landed outcome, which
	// survives the restart an upgrade causes; the in-flight Marker cannot carry
	// it because it is cleared the moment the new image boots. Additive and
	// omitempty: a hive that has never recorded a landed upgrade (or a spoke too
	// old to persist one) reports it absent rather than guessing.
	LastUpdatedAt string `json:"lastUpdatedAt,omitempty"`
	// NextUpdateAt is the RFC3339 time the hub expects the next promotion into
	// this hive's release channel (#10257), relayed from the heartbeat upgrade
	// policy. NextUpdateStatus distinguishes queued/none/unknown so a current
	// stable hive can explicitly say no update is queued instead of going blank.
	// It may be in the past while a promotion gate holds.
	NextUpdateAt     string `json:"nextUpdateAt,omitempty"`
	NextUpdateStatus string `json:"nextUpdateStatus,omitempty"`
	LastError        string `json:"lastError,omitempty"`
	Attempts         int    `json:"attempts,omitempty"`
	MaxAttempts      int    `json:"maxAttempts,omitempty"`
	// Detail is a human-friendly one-liner that always explains the state and,
	// for a failure or unknown, WHY.
	Detail string `json:"detail"`
}

// autoUpdateInputs is what the dashboard already knows locally at the moment it
// answers /api/version. Passing it explicitly keeps buildAutoUpdateStatus a
// pure function that the tests exercise through the real handler.
type autoUpdateInputs struct {
	Enabled bool
	Period  string // raw config mode (may be "")
	// Policy is the hub's heartbeat upgrade policy when one has been delivered
	// (#7262). When non-nil it overrides Enabled/Period: the hub, not the
	// spoke's config file, is the authority on who upgrades this hive and when.
	Policy        *spoke.HeartbeatUpgradePolicy
	TargetBranch  string
	TargetChannel string
	TargetCommit  string
	CurrentCommit string
	CommitsBehind *int           // nil = unknown
	Marker        map[string]any // readUpgradeMarker() output; nil when no upgrade is in flight/failing
	// LastUpdate is the persisted last-LANDED upgrade record (readUpgradeOutcome
	// output); nil when no upgrade has ever landed on this hive. It is how the
	// status answers "when was this hive last updated" (#10038) — the in-flight
	// Marker is cleared the moment an upgrade lands, so only this outcome
	// survives to carry the landed time.
	LastUpdate *upgradeOutcome
}

// normalizeAutoUpdatePeriod maps a stored/legacy auto_upgrade_mode onto a
// concrete, displayable period. An empty or unrecognised value is reported as
// "unknown" rather than silently assumed to be instant: the spoke genuinely
// does not know its schedule, and #6963 is precisely about not dressing an
// unknown up as a definite answer.
func normalizeAutoUpdatePeriod(mode string) string {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case autoUpdatePeriodInstant:
		return autoUpdatePeriodInstant
	case autoUpdatePeriodDaily:
		return autoUpdatePeriodDaily
	case autoUpdatePeriodWeekly:
		return autoUpdatePeriodWeekly
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
		PolicySource:  "local",
		TargetBranch:  in.TargetBranch,
		TargetChannel: in.TargetChannel,
		TargetCommit:  in.TargetCommit,
		CurrentCommit: in.CurrentCommit,
		CommitsBehind: in.CommitsBehind,
	}
	if o := in.LastUpdate; o != nil && !o.CompletedAt.IsZero() {
		st.LastUpdatedAt = o.CompletedAt.UTC().Format(time.RFC3339)
	}
	if in.Enabled {
		st.ManagedBy = autoUpdateManagedBySpoke
	}
	unresolvedChannel := ""
	if p := in.Policy; p != nil {
		st.PolicySource = upgradeTargetSourceHub
		st.Enabled = p.HubManaged || p.SpokeManaged
		st.Period = normalizeAutoUpdatePeriod(p.Schedule)
		st.Paused = p.Paused
		st.NextUpdateAt = strings.TrimSpace(p.NextUpdateAt)
		st.NextUpdateStatus = strings.TrimSpace(p.NextUpdateStatus)
		switch {
		case p.HubManaged:
			st.ManagedBy = autoUpdateManagedByHub
		case p.SpokeManaged:
			st.ManagedBy = autoUpdateManagedBySpoke
		default:
			st.ManagedBy = ""
		}
		if !p.TargetResolved {
			unresolvedChannel = p.Channel
			if unresolvedChannel == "" {
				unresolvedChannel = "its release channel"
			}
		}
	}

	if !st.Enabled {
		st.NextUpdateAt = ""
		st.NextUpdateStatus = ""
		st.State = autoUpdateStateDisabled
		// Disabled is a deliberate configuration, not a fault, so it is not
		// "unhealthy" — but it must never read as "up to date" either.
		st.Healthy = true
		if st.PolicySource == upgradeTargetSourceHub {
			st.Detail = "The hub does not apply upgrades to this hive automatically and the spoke's own auto-upgrade is off; new versions are not applied automatically."
		} else {
			st.Detail = "Automatic updates are turned off for this hive; new versions are not applied automatically."
		}
		return st
	}
	if st.Paused {
		st.NextUpdateAt = ""
		st.NextUpdateStatus = ""
		st.State = autoUpdateStatePaused
		st.Healthy = true
		st.Detail = "Automatic updates are paused fleet-wide by a hub admin; no new version is applied until the hub resumes spoke upgrades."
		if in.CommitsBehind != nil && *in.CommitsBehind > 0 {
			st.Detail += fmt.Sprintf(" This hive is %d commit(s) behind%s.", *in.CommitsBehind, onBranchSuffix(in.TargetBranch))
		}
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
		if unresolvedChannel != "" {
			st.Detail = fmt.Sprintf("Could not determine whether this hive is up to date: the hub could not resolve %s to a commit. Not treating unknown as healthy.", unresolvedChannel)
		} else {
			st.Detail = "Could not determine whether this hive is up to date: the version comparison is unavailable. Not treating unknown as healthy."
		}
		return st
	}
	if *in.CommitsBehind <= 0 {
		st.State = autoUpdateStateUpToDate
		st.Healthy = true
		st.Detail = "This hive is running the latest built commit" + onBranchSuffix(in.TargetBranch) + onChannelSuffix(in.TargetChannel) + "." + lastUpdatedSuffix(st.LastUpdatedAt)
		return st
	}
	st.State = autoUpdateStateBehind
	st.Healthy = false
	st.Detail = fmt.Sprintf("This hive is %d commit(s) behind%s%s and the update has not been applied yet.",
		*in.CommitsBehind, onBranchSuffix(in.TargetBranch), onChannelSuffix(in.TargetChannel)) + lastUpdatedSuffix(st.LastUpdatedAt)
	return st
}

// lastUpdatedSuffix renders the "when was this hive last updated" clause that
// #10038 asks for, appended to the detail of the states where it is meaningful.
// Empty when no landed upgrade has been recorded, so the detail degrades to the
// prior wording rather than claiming an update that never happened.
func lastUpdatedSuffix(at string) string {
	if strings.TrimSpace(at) == "" {
		return ""
	}
	return " Last updated " + at + "."
}

func onChannelSuffix(channel string) string {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return ""
	}
	return " (:" + channel + " channel)"
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

const (
	autoUpdatePeriodInstant = "instant"
	autoUpdatePeriodDaily   = "daily"
	autoUpdatePeriodWeekly  = "weekly"
)
