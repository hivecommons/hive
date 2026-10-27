// Package jev is the Crux ring's share of Hive's Jev typed-decision tool
// (hivecommons/hive#8939): the environment the agent runtime exports to an
// enabled agent and the jev-decide skill it installs into the agent's CLI
// home. It is stdlib-only. The decision endpoint, the provider client and
// the `hive jev` CLI stay in pkg/jev, which aliases these symbols.
package jev

// Environment the agent manager exports alongside HIVE_CAVEMAN_MODE.
const (
	// ModeEnvVar carries the agent's jev_mode ("assist" when the tool is on).
	ModeEnvVar = "HIVE_JEV_MODE"
	// EndpointEnvVar carries the decision endpoint base URL.
	EndpointEnvVar = "HIVE_JEV_ENDPOINT"
	// DefaultEndpoint is what EndpointEnvVar carries to agents.
	DefaultEndpoint = "http://127.0.0.1:18446"
)
