package dashboard

import "time"

// CruxRuntimeHealth is the crux_runtime member of /api/health
// (hivecommons/hive#14138 stage 4): whether the agent runtime hive talks to
// is reachable, and — when hive supervises a `crux serve` child itself —
// what that child is doing. In-process runtimes are always reachable.
type CruxRuntimeHealth struct {
	// Mode is CRUX_MODE: inprocess or sidecar.
	Mode string `json:"mode"`
	// Reachable is false while the sidecar socket answers nothing; kicks
	// are refused with crux.ErrRuntimeUnavailable and the
	// crux_runtime_unavailable fleet-report condition holds.
	Reachable bool `json:"reachable"`
	// LastError is the most recent transport error ("" when reachable).
	LastError string `json:"last_error,omitempty"`
	// Since is when the current outage began (zero when reachable).
	Since time.Time `json:"since,omitempty"`
	// Supervise is CRUX_SUPERVISE: child or external (empty in-process).
	Supervise string `json:"supervise,omitempty"`
	// Child describes the supervised `crux serve` process; nil under
	// external supervision or in-process.
	Child *CruxRuntimeChild `json:"child,omitempty"`
}

// CruxRuntimeChild mirrors crux/runtime.SupervisorState for the health body.
type CruxRuntimeChild struct {
	Running  bool   `json:"running"`
	Ready    bool   `json:"ready"`
	PID      int    `json:"pid,omitempty"`
	Starts   int    `json:"starts"`
	LastExit string `json:"last_exit,omitempty"`
	Binary   string `json:"binary,omitempty"`
}
