package dashboard

import (
	"encoding/json"
	"os"
	"strings"
	"time"
)

const (
	dashboardUpgradeStateStarted = "started"
	dashboardUpgradeStateFailed  = "failed"
	dashboardUpgradeStateDone    = "done"
)

var dashboardUpgradeStatePath = "/data/dashboard-upgrade-state.json"

type dashboardUpgradeState struct {
	State     string    `json:"state"`
	Target    string    `json:"target,omitempty"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
	Reason    string    `json:"reason,omitempty"`
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
	case dashboardUpgradeStateStarted:
		out.State = upgradeAttemptInProgress
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
		out.Detail = "Last upgrade SUCCEEDED — the hive is running the requested target."
	default:
		return nil
	}
	return out
}

func formatUpgradeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
