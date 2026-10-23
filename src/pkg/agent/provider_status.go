package agent

import "github.com/hivecommons/hive/pkg/crux/types"

// Provider status vocabulary and roll-up (ADR-0024, RFC #12611 §4). The
// definitions live in pkg/crux/types as part of the Crux runtime surface
// (hivecommons/hive#13536); these aliases keep pkg/agent's names valid.
const (
	ProviderAuthUsable            = types.ProviderAuthUsable
	ProviderAuthRefreshable       = types.ProviderAuthRefreshable
	ProviderAuthNeedsLogin        = types.ProviderAuthNeedsLogin
	ProviderAuthStoreInaccessible = types.ProviderAuthStoreInaccessible
	ProviderAuthUnknown           = types.ProviderAuthUnknown

	ProviderAvailable      = types.ProviderAvailable
	ProviderRateLimited    = types.ProviderRateLimited
	ProviderQuotaExhausted = types.ProviderQuotaExhausted
	ProviderUnavailable    = types.ProviderUnavailable
	ProviderAvailUnknown   = types.ProviderAvailUnknown
)

// ProviderStatus is one provider's redacted auth/availability roll-up; see
// types.ProviderStatus.
type ProviderStatus = types.ProviderStatus

// ProviderAgentEvidence is the per-agent input to RollUpProviderStatus; see
// types.ProviderAgentEvidence.
type ProviderAgentEvidence = types.ProviderAgentEvidence

// RollUpProviderStatus is types.RollUpProviderStatus.
func RollUpProviderStatus(agents []ProviderAgentEvidence) []ProviderStatus {
	return types.RollUpProviderStatus(agents)
}
