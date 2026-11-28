package hub

import (
	"os"
	"strings"
)

// hubModeEnv names what kind of hub this is (#20196). The only recognised
// value is "self-hosted": a hub that provisions no hives of its own and only
// serves self-registered spokes (for example k3s with Longhorn and Traefik).
// Unset or any other value means "hosted", which is today's behaviour.
//
// The mode is an explicit opt-in rather than inferred from a missing cluster
// registry file: the hosted hub may also run without a registry file and rely
// on the synthesized default cluster, so auto-detection could hide the fleet
// controls on a real fleet.
const hubModeEnv = "HIVE_HUB_MODE"

const (
	// hubModeHosted is the default: the hub provisions hives on its clusters.
	hubModeHosted = "hosted"
	// hubModeSelfHosted is a hub that hosts nothing.
	hubModeSelfHosted = "self-hosted"
)

// hubMode reports the normalized HIVE_HUB_MODE: hubModeSelfHosted when the
// variable says so (case and surrounding space ignored), hubModeHosted
// otherwise. Read on every call (it is a cheap env lookup) so tests can flip it
// with t.Setenv.
func hubMode() string {
	if strings.ToLower(strings.TrimSpace(os.Getenv(hubModeEnv))) == hubModeSelfHosted {
		return hubModeSelfHosted
	}
	return hubModeHosted
}

// hubSelfHosted reports whether HIVE_HUB_MODE=self-hosted.
func hubSelfHosted() bool {
	return hubMode() == hubModeSelfHosted
}
