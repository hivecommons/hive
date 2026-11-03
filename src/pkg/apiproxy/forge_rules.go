package apiproxy

import (
	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/proxy"
)

// AllowedByMode reports whether an HTTP method and path on any supported forge
// (GitHub, GitLab, Gitea) is permitted for an agent running in the specified mode.
// Hard-denied routes (e.g. direct MR/PR creation and merge) and unmapped routes
// fail closed (return false). Capability-aware evaluation (AllowedByModeCaps)
// stays inside crux/proxy, behind the Crux boundary.
func AllowedByMode(mode crux.AgentMode, method, path string) bool {
	return proxy.AllowedByMode(mode, method, path)
}

// DeniedMessage returns the agent-facing directive explaining why a request was
// blocked by a hard-deny rule (e.g. instructing to use the request-file path),
// and true if a hard-deny rule matched.
func DeniedMessage(method, path string) (string, bool) {
	return proxy.DeniedMessage(method, path)
}
