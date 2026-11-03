package dashboard

import (
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Operator advisory for a hive that boots on the shared dev UID (#11713).
//
// When the entrypoint fails to build the UID map ("Failed to build UID map" /
// "persisted UID map … unreadable") the manager logs "no UID map found, agents
// will share dev UID" and carries on: every agent runs as dev with no
// per-agent scoped GitHub token. Nothing operator-visible degraded, so 21 hives
// on one cluster sat in that state for up to two weeks (#11707, fixed by
// #11706) and the only symptom was an agent-authored "HIVE_AGENT_TOKEN_CACHE
// unset" advisory. This file turns the condition into a health check so the
// dashboard tooltip, /api/health/deep and the hub alert surface
// (failingHealthChecks raises health-check-failing for every "fail" check in
// HealthSummary) all flag it.

// healthCheckUIDIsolation is the check name both health surfaces use.
const healthCheckUIDIsolation = "uid_isolation"

// uidIsolationTroubleshootingURL is the runbook for the token-cache symptom a
// shared-dev-UID hive produces.
const uidIsolationTroubleshootingURL = "https://github.com/hivecommons/hive/blob/v5/src/docs/troubleshooting.md#hive_agent_token_cache-unset-no-read-credential-available"

// uidIsolationSharedFn reports whether the agents are running on the shared
// dev UID. A package-level seam (mirroring healthAgentStatuses) so tests can
// stage the condition without a live UID map. nil ⇒ AgentMgr.SharedDevUID.
var uidIsolationSharedFn func() bool

// uidIsolationExpectedFn overrides uidIsolationExpected. A package-level seam
// (mirroring dataDiskUsageFn): the real probe reads ambient HIVE_* env and the
// Kubernetes serviceaccount mount, which a CI runner inside a hive pod really
// has. TestMain pins it to false. nil ⇒ uidIsolationExpected.
var uidIsolationExpectedFn func() bool

// uidIsolationExpected reports whether this hive was deployed to run per-agent
// UID isolation: the image entrypoint ran (it exports
// HIVE_UID_ISOLATION_REVISION), its root phase found agents to isolate (it
// exports a non-empty AGENT_NAMES, which survives the privilege drop), and the
// hive runs under Kubernetes or Podman. A dev laptop or bare `go run` sets
// none of these and never sees the check.
func uidIsolationExpected() bool {
	if strings.TrimSpace(os.Getenv("HIVE_UID_ISOLATION_REVISION")) == "" {
		return false
	}
	if strings.TrimSpace(os.Getenv("AGENT_NAMES")) == "" {
		return false
	}
	return config.IsKubernetesPod() || os.Getenv("container") == "podman"
}

// uidIsolationHealth grades the check. Fail (not warn) so the summary turns
// "degraded" and the hub raises its standing alert: agents without scoped
// tokens cannot do authenticated GitHub reads, and the detail names the
// remedy.
func uidIsolationHealth(shared bool) (status, detail string) {
	if !shared {
		return "pass", "agents run on per-agent UIDs"
	}
	return "fail", "agents share the dev UID (UID map missing or unreadable) — no per-agent scoped GitHub tokens. " +
		"Restart on an image that includes #11706, or delete the empty /data/.hive/uid-map.json and restart. See " +
		uidIsolationTroubleshootingURL
}

// uidIsolationCheck returns the uid_isolation check, ok=false when it does
// not apply (isolation not expected, or no agent manager to ask).
func (s *Server) uidIsolationCheck() (status, detail string, ok bool) {
	expected := uidIsolationExpected
	if uidIsolationExpectedFn != nil {
		expected = uidIsolationExpectedFn
	}
	if !expected() {
		return "", "", false
	}
	var shared bool
	switch {
	case uidIsolationSharedFn != nil:
		shared = uidIsolationSharedFn()
	case s.deps != nil && s.deps.AgentMgr != nil:
		shared = s.deps.AgentMgr.SharedDevUID()
	default:
		return "", "", false
	}
	status, detail = uidIsolationHealth(shared)
	return status, detail, true
}
