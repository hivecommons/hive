package dashboard

import (
	"encoding/json"
	"os"
	"strings"
	"time"
)

const (
	dashboardUpgradeStateStarted    = "started"
	dashboardUpgradeStateQueued     = "queued"
	dashboardUpgradeStateFailed     = "failed"
	dashboardUpgradeStateDone       = "done"
	dashboardUpgradeStateSuperseded = "superseded"
)

var dashboardUpgradeStatePath = "/data/dashboard-upgrade-state.json"

type dashboardUpgradeState struct {
	State       string    `json:"state"`
	Target      string    `json:"target,omitempty"`
	StartedFrom string    `json:"startedFrom,omitempty"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Reason      string    `json:"reason,omitempty"`
	Action      string    `json:"action,omitempty"`
}

func readDashboardUpgradeState() *dashboardUpgradeState {
	data, err := os.ReadFile(dashboardUpgradeStatePath)
	if err != nil {
		return nil
	}
	var st dashboardUpgradeState
	if err := json.Unmarshal(data, &st); err != nil || strings.TrimSpace(st.State) == "" {
		return nil
	}
	return &st
}

func writeDashboardUpgradeState(st dashboardUpgradeState) error {
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now().UTC()
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(dashboardUpgradeStatePath, data, 0o644)
}

func (s *Server) rememberDashboardUpgradeState(st dashboardUpgradeState) {
	if err := writeDashboardUpgradeState(st); err != nil && s != nil && s.logger != nil {
		s.logger.Error("dashboard self-upgrade state persistence failed",
			"state", st.State, "target", st.Target, "reason", st.Reason, "error", err)
	}
}

func upgradeAttemptFromDashboardState(st *dashboardUpgradeState) *UpgradeAttemptStatus {
	if st == nil {
		return nil
	}
	out := &UpgradeAttemptStatus{
		Target: st.Target,
		At:     formatUpgradeTime(st.StartedAt),
		Reason: st.Reason,
	}
	switch st.State {
	case dashboardUpgradeStateQueued:
		out.State = upgradeAttemptInProgress
		out.Detail = "Upgrade queued — image building."
		if st.Target != "" {
			out.Detail = "Upgrade to " + shortSHADashboard(st.Target) + " queued — image building."
		}
	case dashboardUpgradeStateStarted:
		out.State = upgradeAttemptInProgress
		if st.Action == "podman-quadlet-request" {
			out.Detail = "Upgrade request written; waiting for hive-upgrade.service on the host to apply it."
			break
		}
		out.Detail = "Upgrade request accepted by the hub; waiting for this spoke to collect the instruction on heartbeat and roll its Deployment."
		if st.Target != "" {
			out.Detail = "Upgrade to " + st.Target + " was accepted by the hub; waiting for this spoke to collect the instruction on heartbeat and roll its Deployment."
		}
	case dashboardUpgradeStateFailed:
		out.State = upgradeAttemptFailed
		out.Attempts = 1
		out.MaxAttempts = 1
		if out.Reason == "" {
			out.Reason = "upgrade request failed before the spoke could roll"
		}
		out.Detail = "Last upgrade FAILED: " + out.Reason
	case dashboardUpgradeStateDone:
		out.State = upgradeAttemptSucceeded
		out.CompletedAt = formatUpgradeTime(st.UpdatedAt)
		if out.Target != "" {
			out.Detail = "Last upgrade completed — the hive is running " + shortSHADashboard(out.Target) + "."
		} else {
			out.Detail = "Last upgrade completed."
		}
	case dashboardUpgradeStateSuperseded:
		out.State = upgradeAttemptSuperseded
		out.CompletedAt = formatUpgradeTime(st.UpdatedAt)
		if out.Reason == "" {
			out.Reason = "upgrade request was superseded before completion could be observed"
		}
		out.Detail = "Last upgrade superseded: " + out.Reason
	default:
		return nil
	}
	return out
}

func upgradeImageBuildingReason(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	return strings.Contains(reason, "is not published yet") ||
		strings.Contains(reason, "not published yet")
}

func formatUpgradeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// selectUpgradeAttempt decides whether the dashboard-button record (manual)
// or the landed-outcome classification (base, from buildUpgradeAttemptStatus)
// is the "last upgrade" the Channel card shows. Newest evidence wins.
//
// The dashboard record is written by the dashboard's own upgrade button and
// then settled once by reconcileDashboardUpgradeState; no other rollout path
// (a hub-driven channel pull, a floating-tag retag, kubectl set image, a
// redeploy) ever touches it again. Letting a terminal record outrank the
// outcome unconditionally made the card report "Last upgrade 0978b9c · 4d
// ago" on a hive that had been rolled daily since, while the upgrade-notice
// banner beside it correctly announced the newer commit.
func selectUpgradeAttempt(base UpgradeAttemptStatus, manual *dashboardUpgradeState, outcome *upgradeOutcome, runningSHA string) UpgradeAttemptStatus {
	at := upgradeAttemptFromDashboardState(manual)
	if at == nil {
		return base
	}
	switch manual.State {
	case dashboardUpgradeStateStarted, dashboardUpgradeStateQueued:
		// An unresolved request is live state and outranks any past landing.
		return *at
	}
	manualAt := manual.UpdatedAt
	if manualAt.IsZero() {
		manualAt = manual.StartedAt
	}
	if outcome != nil {
		outcomeAt := outcome.CompletedAt
		if outcomeAt.IsZero() {
			outcomeAt = outcome.RequestedAt
		}
		if !outcomeAt.IsZero() && (manualAt.IsZero() || outcomeAt.After(manualAt)) {
			return base
		}
	}
	runningSHA = strings.TrimSpace(runningSHA)
	if manual.State == dashboardUpgradeStateDone && runningSHA != "" && manual.Target != "" && !sameCommitDashboard(runningSHA, manual.Target) {
		if outcome != nil {
			// The outcome path already reports the superseded landing with the
			// running commit leading; prefer its wording over a second copy.
			return base
		}
		at.Superseded = true
		at.RunningCommit = shortSHADashboard(runningSHA)
		at.Detail = "The hive is running " + at.RunningCommit + ". The last dashboard-requested upgrade (to " +
			shortSHADashboard(manual.Target) + ") has since been superseded — the hive was rolled by the hub or a floating image tag, not by this dashboard."
	}
	return *at
}
