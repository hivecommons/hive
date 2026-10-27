package hub

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
)

const (
	// EnvAuthHealthDownThreshold overrides how long ALL enabled agents on a
	// hive must report a failing BackendAuth status before the hive's
	// aggregate auth_health escalates from degraded to down.
	EnvAuthHealthDownThreshold = "HIVE_HUB_AUTH_HEALTH_DOWN_THRESHOLD"
	// DefaultAuthHealthDownThreshold: #6500's incident lasted hours; the bar
	// here is "faster than a human noticing", not "faster than every possible
	// false positive". 15 minutes is comfortably longer than the spoke's own
	// provider-error backoff ceiling (providerErrorBackoffMaxDefault in
	// pkg/agent), so a hive genuinely still retrying transient upstream
	// errors never trips this — only a fleet stuck on the SAME auth-class
	// failure for a sustained window does.
	DefaultAuthHealthDownThreshold = 15 * time.Minute
)

// auth_health values for RegistryEntry.AuthHealth / the hub dashboard badge.
const (
	AuthHealthOK       = "ok"
	AuthHealthDegraded = "degraded"
	AuthHealthDown     = "down"
)

// authHealthNamedInReason caps how many agent names are listed in the
// auth_health reason before it summarises the remainder, matching
// inactiveAgentsNamedInReason's rationale beside it.
const authHealthNamedInReason = 3

// AuthHealthDownThreshold reads EnvAuthHealthDownThreshold, falling back to
// DefaultAuthHealthDownThreshold for an unset, empty, or unparseable value.
func AuthHealthDownThreshold() time.Duration {
	if v := strings.TrimSpace(os.Getenv(EnvAuthHealthDownThreshold)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultAuthHealthDownThreshold
}

// AuthHealthReport is the per-hive result of evaluateAuthHealth.
type AuthHealthReport struct {
	// Status is one of AuthHealthOK / AuthHealthDegraded / AuthHealthDown.
	Status string
	// Reason is the human sentence for the fleet row / badge tooltip. Empty
	// when Status is ok.
	Reason string
	// Since is when the CURRENT Status began. For "down" this is the latest
	// BackendAuthSince among the failing agents — the moment the LAST enabled
	// agent joined the fleet-wide failure, which is what the threshold above
	// measures against. Zero when Status is ok or the evidence is incomplete.
	Since time.Time
}

// evaluateAuthHealth classifies one hive's fleet-wide backend-auth state from
// its heartbeat-reported per-agent evidence.
//
// A hive with no enabled agents (nothing configured, or an old spoke that
// never reports Enabled) is reported ok — the same "no signal, no alarm"
// convention every other fleet-health facet in this package uses.
func evaluateAuthHealth(agents []AgentSummary, now time.Time) AuthHealthReport {
	var enabled []AgentSummary
	for _, a := range agents {
		if a.Enabled {
			enabled = append(enabled, a)
		}
	}
	if len(enabled) == 0 {
		return AuthHealthReport{Status: AuthHealthOK}
	}

	var failing []AgentSummary
	for _, a := range enabled {
		status := strings.TrimSpace(a.BackendAuthStatus)
		if status == "" || status == crux.BackendAuthOK {
			continue
		}
		failing = append(failing, a)
	}
	if len(failing) == 0 {
		return AuthHealthReport{Status: AuthHealthOK}
	}

	reason := authHealthReason(failing, len(enabled))

	if len(failing) < len(enabled) {
		return AuthHealthReport{Status: AuthHealthDegraded, Reason: reason}
	}

	// ALL enabled agents are failing. Since is the latest per-agent
	// BackendAuthSince — the instant the fleet-wide condition became true —
	// so a hive whose agents failed at staggered times only starts the
	// down-threshold clock once the LAST one joined, not the first.
	since, ok := latestBackendAuthSince(failing)
	if !ok || now.Sub(since) < AuthHealthDownThreshold() {
		return AuthHealthReport{Status: AuthHealthDegraded, Reason: reason, Since: since}
	}
	return AuthHealthReport{Status: AuthHealthDown, Reason: reason, Since: since}
}

// latestBackendAuthSince returns the latest parseable BackendAuthSince among
// agents, and false if none parsed — an incomplete-evidence hive (e.g. a
// legacy spoke reporting status without a timestamp) must never be read as
// "just started failing", which would silently reset the down clock forever.
func latestBackendAuthSince(agents []AgentSummary) (time.Time, bool) {
	var latest time.Time
	found := false
	for _, a := range agents {
		t, ok := parseAgentTime(a.BackendAuthSince)
		if !ok {
			continue
		}
		if !found || t.After(latest) {
			latest = t
			found = true
		}
	}
	return latest, found
}

func authHealthReason(failing []AgentSummary, totalEnabled int) string {
	named := make([]string, 0, len(failing))
	for _, a := range failing {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			continue
		}
		named = append(named, name+" ("+a.BackendAuthStatus+")")
	}
	shown := named
	suffix := ""
	if len(shown) > authHealthNamedInReason {
		shown = shown[:authHealthNamedInReason]
		suffix = " (+" + strconv.Itoa(len(named)-authHealthNamedInReason) + " more)"
	}
	noun := "agent is"
	if len(failing) != 1 {
		noun = "agents are"
	}
	return strconv.Itoa(len(failing)) + " of " + strconv.Itoa(totalEnabled) + " enabled " + noun +
		" failing backend auth: " + strings.Join(shown, ", ") + suffix
}
