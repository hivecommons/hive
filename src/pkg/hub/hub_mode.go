package hub

import (
	"os"
	"strings"
)

// hubModeEnv declares what kind of fleet this hub runs (#20189).
//
// Unset (the default) keeps every hosted-fleet behaviour, including the
// synthesized hive-oke cluster a hub gets when it has no clusters.json: hosted
// hubs rely on that fallback, so a missing registry alone cannot mean "hosts
// nothing".
//
// "self-hosted" tells the hub it provisions no hive: every spoke registers
// itself. The hub then synthesizes a neutral in-cluster entry instead of the
// OKE one, and the dashboard hides the hosted-fleet controls (Add Hosted
// Hive, Request a hive, Cluster Health, Scale Controls, the per-user hosted
// quota, NPS feedback and the per-cluster usage split), leaving "Register
// your own hive" as the only path.
const hubModeEnv = "HIVE_HUB_MODE"

// hubModeSelfHosted is the HIVE_HUB_MODE value for a hub that hosts nothing.
const hubModeSelfHosted = "self-hosted"

// selfHostedClusterName names the in-cluster entry a self-hosted hub
// synthesizes when it has no clusters.json. It claims nothing about the
// cluster's distribution, storage, ingress or architecture.
const selfHostedClusterName = "Hub cluster"

// hubSelfHosted reports HIVE_HUB_MODE=self-hosted. Read on every call (a
// cheap env lookup) so tests can flip it with t.Setenv.
func hubSelfHosted() bool {
	return strings.ToLower(strings.TrimSpace(os.Getenv(hubModeEnv))) == hubModeSelfHosted
}
