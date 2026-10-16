package dashboard

import "time"

// FrontendAgentContinuous carries continuous-mode status fields embedded into
// FrontendAgent without shifting the route citations in server.go.
type FrontendAgentContinuous struct {
	ContinuousBackoffUntil string   `json:"continuousBackoffUntil,omitempty"`
	ContinuousBlocked      string   `json:"continuousBlocked,omitempty"`
	ContinuousModes        []string `json:"continuousModes,omitempty"`
	ContinuousKicks        int64    `json:"continuousKicks,omitempty"`
	ContinuousTokens       int64    `json:"continuousTokens,omitempty"`
	// LastKickEnd is when the agent's last kick turn ended (RFC3339).
	LastKickEnd string `json:"lastKickEnd,omitempty"`
	// IdleSince is set while a continuous agent is idle between its last kick
	// end and its scheduled re-kick, so the dashboard can render
	// "idle since 16:31 — re-kick in 42s" instead of a bare empty prompt.
	IdleSince string `json:"idleSince,omitempty"`
}

// continuousIdleSince reports when a continuous agent went idle awaiting its
// re-kick: the last kick end, only while the agent is idle and a re-kick is
// still pending. Zero otherwise.
func continuousIdleSince(continuousNow bool, busy string, lastKickEnd, nextKick time.Time) time.Time {
	if !continuousNow || lastKickEnd.IsZero() || nextKick.IsZero() || busy == "working" {
		return time.Time{}
	}
	return lastKickEnd
}
