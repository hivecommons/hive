// Package hub contains the SaaS control-plane server and its hub-side state.
//
// Spoke-mode callers must use pkg/hub/spoke (and wire DTOs) instead. The cmd/hive
// binary is intentionally dual-mode, so non-spoke startup code may still link this
// package for SaaS mode while hubwire.go uses pkg/hub/spoke directly.
//
// Tech debt: several hub-server paths still share package-level mutable state for
// persistence and poller coordination (registry save-loop globals, latest-SHA
// poller state, reach reporter seams, webhook retry timing, and SaaS filesystem
// roots). Spoke-side heartbeat, SSO, terminal, FD, cluster-health, and self-upgrade
// implementations live in pkg/hub/spoke.
package hub
