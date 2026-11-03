// Package advisorhook holds the advisor lane's launch-side contract: the
// `hive advisor-hook` subcommand name, its output dialects and the advise
// endpoint variable. crux/agent projects these into agent hook files at
// launch; the hub side (pkg/advisor) serves the endpoint and implements the
// subcommand, and re-exports these names so its callers are unchanged.
package advisorhook

// HookSubcommand is the `hive advisor-hook` subcommand every supported
// backend invokes at its turn boundary. It reads the transcript reference the
// backend provides on stdin, asks the hub's loopback advise endpoint for the
// verdict, and prints the answer in the calling backend's dialect. Keeping
// the backend-specific part to formatting is what makes each adapter thin.
const HookSubcommand = "advisor-hook"

// HookFormatCopilot renders the Copilot CLI `agentStop` dialect (camelCase
// payload, `decision: block` with a reason that becomes the forced next
// prompt). HookFormatCodex is the Codex CLI `Stop` dialect, which matches
// Claude Code's payload and answer.
const (
	HookFormatCopilot = "copilot"
	HookFormatCodex   = "codex"
)

// EndpointEnvVar carries the advise endpoint base URL to advised agents, the
// way HIVE_JEV_ENDPOINT carries the Jev endpoint.
const EndpointEnvVar = "HIVE_ADVISOR_ENDPOINT"

// DefaultEndpoint is what EndpointEnvVar carries to agents.
const DefaultEndpoint = "http://127.0.0.1:18447"
