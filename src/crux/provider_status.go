package crux

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Provider auth states (ADR-0024, RFC #12611 §4). Authentication answers "can
// this provider's credential put an agent to work?", independent of whether
// the provider is currently serving requests.
const (
	ProviderAuthUsable      = "usable"
	ProviderAuthRefreshable = "refreshable"
	ProviderAuthNeedsLogin  = "needs-login"
	// ProviderAuthStoreInaccessible is part of the ADR-0024 vocabulary but no
	// BackendAuth status maps to it yet: today's evidence cannot tell an
	// unreadable store from an absent one.
	ProviderAuthStoreInaccessible = "store-inaccessible"
	ProviderAuthUnknown           = "unknown"
)

// Provider availability states (ADR-0024, RFC #12611 §4). Availability answers
// "is the provider serving this account right now?" — a quota refusal or an
// unreachable gateway is never a reason to log in again.
const (
	ProviderAvailable = "available"
	// ProviderRateLimited is part of the vocabulary but not emitted yet: rate
	// limits are transient and deliberately never change BackendAuth.Status.
	ProviderRateLimited    = "rate-limited"
	ProviderQuotaExhausted = "quota-exhausted"
	ProviderUnavailable    = "unavailable"
	ProviderAvailUnknown   = "unknown"
)

// ProviderStatus is one provider's redacted auth/availability roll-up across
// the hive's enabled agents. It carries no credential material: states are a
// closed vocabulary, Evidence and NextAction are built from status names only,
// and AffectedAgents are agent names.
type ProviderStatus struct {
	Provider          string   `json:"provider"`
	AuthState         string   `json:"auth_state"`
	AvailabilityState string   `json:"availability_state"`
	Since             string   `json:"since,omitempty"`
	Evidence          string   `json:"evidence,omitempty"`
	NextAction        string   `json:"next_action,omitempty"`
	AffectedAgents    []string `json:"affected_agents,omitempty"`
}

// ProviderAgentEvidence is the per-agent input to RollUpProviderStatus. It is
// a plain projection of fields the spoke heartbeat and /api/status already
// carry, so both surfaces roll up identically.
type ProviderAgentEvidence struct {
	Agent          string
	Provider       string
	Enabled        bool
	NeedsLogin     bool
	QuotaExhausted bool
	// AuthStatus is the agent's BackendAuth.Status ("" or BackendAuthOK when
	// no auth-shaped failure has been classified).
	AuthStatus string
	AuthSince  time.Time
	// Refreshable is claude.HasUsableToken over the agent's own credential:
	// nil when unknown (every non-Claude backend — ADR-0024 records no
	// verified idle-refresh path for them).
	Refreshable *bool
}

var providerAuthRank = map[string]int{
	ProviderAuthUsable:            1,
	ProviderAuthRefreshable:       2,
	ProviderAuthUnknown:           3,
	ProviderAuthStoreInaccessible: 4,
	ProviderAuthNeedsLogin:        5,
}

var providerAvailRank = map[string]int{
	ProviderAvailable:      1,
	ProviderRateLimited:    2,
	ProviderQuotaExhausted: 3,
	ProviderUnavailable:    4,
}

// providerAgentStates maps one agent's BackendAuth evidence onto the two
// ADR-0024 dimensions. An empty result means "this agent gives no evidence on
// that dimension" (an auth failure says nothing about provider availability,
// and an unreachable gateway says nothing about the credential).
func providerAgentStates(e ProviderAgentEvidence) (auth, avail, label string) {
	refreshable := e.Refreshable != nil && *e.Refreshable
	switch status := strings.TrimSpace(e.AuthStatus); status {
	case "", BackendAuthOK:
		switch {
		case e.NeedsLogin && refreshable:
			return ProviderAuthRefreshable, "", "login-prompt"
		case e.NeedsLogin:
			return ProviderAuthNeedsLogin, "", "login-prompt"
		case e.QuotaExhausted:
			return ProviderAuthUsable, ProviderQuotaExhausted, BackendAuthQuota
		}
		return ProviderAuthUsable, ProviderAvailable, ""
	case BackendAuthTokenExpired:
		// An expired access token alone is not a logout (#5165): with a live
		// refresh grant, a CLI restart heals it.
		if refreshable {
			return ProviderAuthRefreshable, "", status
		}
		return ProviderAuthNeedsLogin, "", status
	case BackendAuthUnlicensed:
		return ProviderAuthNeedsLogin, "", status
	case BackendAuthForbidden:
		// Cause undetermined (#6500): never convicted as a logout.
		return ProviderAuthUnknown, "", status
	case BackendAuthUnreachable:
		return "", ProviderUnavailable, status
	case BackendAuthQuota:
		return ProviderAuthUsable, ProviderQuotaExhausted, status
	default:
		return ProviderAuthUnknown, "", status
	}
}

// RollUpProviderStatus groups enabled agents by provider and reports, per
// provider, the worst auth state and the worst availability state any agent
// evidences. Disabled agents are skipped so a provider nobody is using never
// becomes an incident. A dimension with no evidence at all is "unknown" —
// never silently healthy. Output is sorted by provider; nil when no enabled
// agent names a provider.
func RollUpProviderStatus(agents []ProviderAgentEvidence) []ProviderStatus {
	type acc struct {
		auth, avail string
		enabled     int
		affected    []string
		labels      map[string]int
		since       time.Time
	}
	byProvider := map[string]*acc{}
	for _, e := range agents {
		provider := strings.ToLower(strings.TrimSpace(e.Provider))
		if !e.Enabled || provider == "" {
			continue
		}
		p := byProvider[provider]
		if p == nil {
			p = &acc{labels: map[string]int{}}
			byProvider[provider] = p
		}
		p.enabled++
		auth, avail, label := providerAgentStates(e)
		if providerAuthRank[auth] > providerAuthRank[p.auth] {
			p.auth = auth
		}
		if providerAvailRank[avail] > providerAvailRank[p.avail] {
			p.avail = avail
		}
		if label == "" {
			continue
		}
		p.labels[label]++
		if name := strings.TrimSpace(e.Agent); name != "" {
			p.affected = append(p.affected, name)
		}
		if !e.AuthSince.IsZero() && (p.since.IsZero() || e.AuthSince.Before(p.since)) {
			p.since = e.AuthSince
		}
	}
	if len(byProvider) == 0 {
		return nil
	}
	out := make([]ProviderStatus, 0, len(byProvider))
	for provider, p := range byProvider {
		if p.auth == "" {
			p.auth = ProviderAuthUnknown
		}
		if p.avail == "" {
			p.avail = ProviderAvailUnknown
		}
		sort.Strings(p.affected)
		ps := ProviderStatus{
			Provider:          provider,
			AuthState:         p.auth,
			AvailabilityState: p.avail,
			Evidence:          providerEvidence(p.labels, len(p.affected), p.enabled),
			NextAction:        providerNextAction(p.auth, p.avail, p.labels),
			AffectedAgents:    p.affected,
		}
		if !p.since.IsZero() {
			ps.Since = p.since.UTC().Format(time.RFC3339)
		}
		out = append(out, ps)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

func providerEvidence(labels map[string]int, affected, enabled int) string {
	if len(labels) == 0 {
		return "no auth or availability failure on " + strconv.Itoa(enabled) + " enabled " + pluralAgents(enabled)
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"×"+strconv.Itoa(labels[k]))
	}
	return strconv.Itoa(affected) + " of " + strconv.Itoa(enabled) + " enabled " + pluralAgents(enabled) + ": " + strings.Join(parts, ", ")
}

func pluralAgents(n int) string {
	if n == 1 {
		return "agent"
	}
	return "agents"
}

// providerNextAction names the one operator action for the worst condition.
// Auth problems outrank availability ones: a quota reset cannot help an agent
// that cannot authenticate. Hive never refreshes a credential itself
// (ADR-0024); "refreshable" points at the CLI's own restart-time refresh.
func providerNextAction(auth, avail string, labels map[string]int) string {
	switch auth {
	case ProviderAuthNeedsLogin:
		if labels[BackendAuthUnlicensed] > 0 {
			return "Restore this account's licence/seat for the provider, then log in once; re-login alone does not fix an entitlement failure."
		}
		return "Log in to the provider once with a licensed identity; see the backend-auth recovery runbook."
	case ProviderAuthStoreInaccessible:
		return "Make the credential store readable by the consuming agent's UID; do not copy credentials between agents."
	case ProviderAuthUnknown:
		if labels[BackendAuthForbidden] > 0 {
			return "Authorization denied with no attributable cause: check seat, org policy and token before re-login."
		}
	case ProviderAuthRefreshable:
		return "Restart the affected agents: the CLI redeems its still-valid refresh grant on start. No login needed."
	}
	switch avail {
	case ProviderUnavailable:
		return "Provider or gateway unreachable: check its status and network path; re-login will not help."
	case ProviderQuotaExhausted:
		return "Quota exhausted: wait for the provider reset or raise the limit; re-login or refresh will not help."
	}
	return ""
}
