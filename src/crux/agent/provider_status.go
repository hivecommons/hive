package agent

import "github.com/hivecommons/hive/crux"

// Provider status vocabulary and roll-up (ADR-0024, RFC #12611 §4). The
// definitions live in pkg/crux as part of the Crux runtime surface
// (hivecommons/hive#13536); these aliases keep pkg/agent's names valid.
const (
	ProviderAuthUsable            = crux.ProviderAuthUsable
	ProviderAuthRefreshable       = crux.ProviderAuthRefreshable
	ProviderAuthNeedsLogin        = crux.ProviderAuthNeedsLogin
	ProviderAuthStoreInaccessible = crux.ProviderAuthStoreInaccessible
	ProviderAuthUnknown           = crux.ProviderAuthUnknown

	ProviderAvailable      = crux.ProviderAvailable
	ProviderRateLimited    = crux.ProviderRateLimited
	ProviderQuotaExhausted = crux.ProviderQuotaExhausted
	ProviderUnavailable    = crux.ProviderUnavailable
	ProviderAvailUnknown   = crux.ProviderAvailUnknown
)

// ProviderStatus is one provider's redacted auth/availability roll-up; see
// crux.ProviderStatus.
type ProviderStatus = crux.ProviderStatus

// ProviderAgentEvidence is the per-agent input to RollUpProviderStatus; see
// crux.ProviderAgentEvidence.
type ProviderAgentEvidence = crux.ProviderAgentEvidence

// RollUpProviderStatus is crux.RollUpProviderStatus.
func RollUpProviderStatus(agents []ProviderAgentEvidence) []ProviderStatus {
	return crux.RollUpProviderStatus(agents)
}
