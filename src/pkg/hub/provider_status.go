package hub

import (
	"strings"

	"github.com/hivecommons/hive/pkg/agent"
)

// maxProviderStatuses bounds the spoke-reported providers block. A hive runs a
// handful of backends; anything larger is a broken or hostile spoke.
const maxProviderStatuses = 16

var (
	knownProviderAuthStates = map[string]bool{
		agent.ProviderAuthUsable:            true,
		agent.ProviderAuthRefreshable:       true,
		agent.ProviderAuthNeedsLogin:        true,
		agent.ProviderAuthStoreInaccessible: true,
		agent.ProviderAuthUnknown:           true,
	}
	knownProviderAvailabilityStates = map[string]bool{
		agent.ProviderAvailable:      true,
		agent.ProviderRateLimited:    true,
		agent.ProviderQuotaExhausted: true,
		agent.ProviderUnavailable:    true,
		agent.ProviderAvailUnknown:   true,
	}
)

// sanitizeProviderStatuses cleans the spoke-reported providers block
// (ADR-0024, #12611). States outside the closed vocabulary become "unknown"
// rather than being dropped or trusted, so a newer spoke's state never reads
// as healthy on an older hub. nil stays nil (old spoke: no signal).
func sanitizeProviderStatuses(in []agent.ProviderStatus) []agent.ProviderStatus {
	if in == nil {
		return nil
	}
	out := make([]agent.ProviderStatus, 0, len(in))
	for _, p := range in {
		if len(out) >= maxProviderStatuses {
			break
		}
		provider := sanitizeHeartbeatField(p.Provider)
		if provider == "" {
			continue
		}
		auth := strings.TrimSpace(p.AuthState)
		if !knownProviderAuthStates[auth] {
			auth = agent.ProviderAuthUnknown
		}
		avail := strings.TrimSpace(p.AvailabilityState)
		if !knownProviderAvailabilityStates[avail] {
			avail = agent.ProviderAvailUnknown
		}
		out = append(out, agent.ProviderStatus{
			Provider:          provider,
			AuthState:         auth,
			AvailabilityState: avail,
			Since:             sanitizeHeartbeatTime(p.Since),
			Evidence:          sanitizeProseField(p.Evidence),
			NextAction:        sanitizeProseField(p.NextAction),
			AffectedAgents:    sanitizeAgentNameList(p.AffectedAgents),
		})
	}
	return out
}
