package crux

import "time"

// ActivityObserver exposes the liveness and capability evidence used by the
// spoke's heartbeat. It does not launch or modify agent processes.
type ActivityObserver interface {
	SessionMissing(name string) bool
	AgentCapabilities(name string) (canOpenIssue, canOpenPR, canMerge, ok bool)
	EffectiveBackend(name string) (backend string, ok bool)
	AgentCredentialRefreshable(name string) (refreshable, known bool)
	RestartTelemetry(name string) (total, last24h int, lastRestartAt time.Time, lastReason string, ok bool)
}
