// Package spoke contains the hub-facing client helpers used by spoke-mode hives.
//
// The cmd/hive binary remains dual-mode, so cmd/hive may still link the control-plane
// pkg/hub server for SaaS mode. Spoke-only packages such as pkg/dashboard must depend
// on this package (and wire DTOs), not on pkg/hub.
package spoke
