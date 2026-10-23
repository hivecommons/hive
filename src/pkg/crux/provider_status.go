package crux

import "github.com/hivecommons/hive/pkg/crux/types"

// Provider auth states (ADR-0024, RFC #12611 §4). Authentication answers "can
// this provider's credential put an agent to work?", independent of whether
// the provider is currently serving requests.
const (
	ProviderAuthUsable            = types.ProviderAuthUsable
	ProviderAuthRefreshable       = types.ProviderAuthRefreshable
	ProviderAuthNeedsLogin        = types.ProviderAuthNeedsLogin
	ProviderAuthStoreInaccessible = types.ProviderAuthStoreInaccessible
	ProviderAuthUnknown           = types.ProviderAuthUnknown
)

// Provider availability states (ADR-0024, RFC #12611 §4). Availability answers
// "is the provider serving this account right now?".
const (
	ProviderAvailable      = types.ProviderAvailable
	ProviderRateLimited    = types.ProviderRateLimited
	ProviderQuotaExhausted = types.ProviderQuotaExhausted
	ProviderUnavailable    = types.ProviderUnavailable
	ProviderAvailUnknown   = types.ProviderAvailUnknown
)

// BackendAuth status values: the coarse per-agent backend-auth states the
// hub's fleet aggregator and the dashboard distinguish. Hub: auth_health.go,
// provider_status.go. Dashboard: status_builder.go.
const (
	BackendAuthOK           = types.BackendAuthOK
	BackendAuthUnlicensed   = types.BackendAuthUnlicensed
	BackendAuthTokenExpired = types.BackendAuthTokenExpired
	BackendAuthUnreachable  = types.BackendAuthUnreachable
	BackendAuthQuota        = types.BackendAuthQuota
	BackendAuthForbidden    = types.BackendAuthForbidden
)

// BackendAuthState is the per-agent backend-auth canary carried on
// Process.BackendAuth and in the spoke heartbeat. Hub/spoke: heartbeat.go.
type BackendAuthState = types.BackendAuthState

// ProviderStatus is one provider's redacted auth/availability roll-up across
// a hive's enabled agents. Hub: provider_status.go, server.go. Dashboard:
// status_builder.go.
type ProviderStatus = types.ProviderStatus

// ProviderAgentEvidence is the per-agent input to RollUpProviderStatus.
// Spoke: heartbeat.go. Dashboard: status_builder.go.
type ProviderAgentEvidence = types.ProviderAgentEvidence

// RollUpProviderStatus groups enabled agents by provider and reports, per
// provider, the worst auth and availability state any agent evidences; see
// types.RollUpProviderStatus.
func RollUpProviderStatus(agents []ProviderAgentEvidence) []ProviderStatus {
	return types.RollUpProviderStatus(agents)
}
