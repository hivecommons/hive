package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Nous gate protocol tunables (RFC #11585 part 3, #11696).
const (
	// DefaultNousGatePollInterval is how often the hive re-reads
	// <nous_dir>/gates/pending.json for a gate the agent is waiting on.
	// GET /api/nous/gate-pending also re-reads it on demand, so this only
	// bounds how late the owner notification can be.
	DefaultNousGatePollInterval = 15 * time.Second

	// NousGatePollIntervalEnv overrides nous.gate_poll_seconds at runtime.
	// Accepts a Go duration ("30s") or a bare number of seconds.
	NousGatePollIntervalEnv = "HIVE_NOUS_GATE_POLL_INTERVAL"
)

// NousConfig is the `nous:` section of hive.yaml.
type NousConfig struct {
	// GatePollSeconds is the pending-gate poll cadence in seconds. Zero or
	// absent applies DefaultNousGatePollInterval.
	GatePollSeconds int `yaml:"gate_poll_seconds,omitempty" json:"gate_poll_seconds,omitempty"`
}

// GatePollInterval resolves the pending-gate poll cadence with precedence
// env > yaml > default. Non-positive or unparseable values fall through to
// the next source so a typo can never stop gate polling.
func (n NousConfig) GatePollInterval() time.Duration {
	if raw := strings.TrimSpace(os.Getenv(NousGatePollIntervalEnv)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
		if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	if n.GatePollSeconds > 0 {
		return time.Duration(n.GatePollSeconds) * time.Second
	}
	return DefaultNousGatePollInterval
}
