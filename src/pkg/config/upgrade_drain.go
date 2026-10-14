package config

import "time"

// defaultUpgradeDrainTimeoutS is how long a self-upgrade waits for in-flight
// agent kicks when hub.upgrade_drain_timeout_s is unset (#11876).
const defaultUpgradeDrainTimeoutS = 45 * 60

// EffectiveUpgradeDrainTimeout returns the self-upgrade drain bound: the
// configured value, the default when unset, or 0 (drain disabled) when the
// operator set a negative value.
func (h HubConfig) EffectiveUpgradeDrainTimeout() time.Duration {
	switch {
	case h.UpgradeDrainTimeoutS < 0:
		return 0
	case h.UpgradeDrainTimeoutS > 0:
		return time.Duration(h.UpgradeDrainTimeoutS) * time.Second
	default:
		return defaultUpgradeDrainTimeoutS * time.Second
	}
}
