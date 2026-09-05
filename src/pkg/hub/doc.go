// Package hub contains the SaaS control-plane server and its hub-side state.
//
// Spoke-mode callers must use pkg/hub/spoke (and wire DTOs) instead. The cmd/hive
// binary is intentionally dual-mode, so non-spoke startup code may still link this
// package for SaaS mode while hubwire.go uses pkg/hub/spoke directly.
package hub
