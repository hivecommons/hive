package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// uidIsolationMarkersReady reports whether both ownership passes an agent
// depends on have completed: the shared HOME repair and that agent's own tree.
// Marker contents bind readiness to both the ownership-schema revision and the
// assigned UID, so a roster change that shifts UIDs cannot reuse a stale pass.
func uidIsolationMarkersReady(markerDir, revision string, uid int) bool {
	if markerDir == "" || revision == "" || uid <= 0 {
		return true // legacy/shared-UID deployment: no marker contract to await
	}
	checks := map[string]string{
		filepath.Join(markerDir, "home.ready"):                       revision,
		filepath.Join(markerDir, fmt.Sprintf("agent-%d.ready", uid)): revision + ":" + strconv.Itoa(uid),
	}
	for path, expected := range checks {
		data, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(data)) != expected {
			return false
		}
	}
	return true
}

// publishRuntimeUIDIsolationMarker publishes the per-agent completion marker
// for an agent whose UID was allocated at RUNTIME (AddAgent/ReconcileAgents),
// after the entrypoint's boot-time migration walk has already finished. The
// walk only iterates the boot-time roster, so without this an agent added
// post-boot waits in awaitUIDIsolation for an agent-<uid>.ready marker that
// nothing will ever write — a permanent launch hold (observed live: an ACMM
// pack update added `adjudicator` 31 minutes after boot and every governor
// kick failed with "cannot be kicked: it is stopped" from then on).
//
// Semantics mirror the entrypoint's per-agent pass, including its fail-open
// policy: a runtime-added agent normally has NO tree yet under /data/agents
// (it is created at launch, owned by the launching UID), so there is nothing
// to migrate and publishing the marker is the correct, safe resolution. When
// a tree does exist and is foreign-owned, the entrypoint publishes after a
// best-effort chown WARN — withholding the marker forever would turn a
// permissions warning into a permanent agent outage, which is exactly the
// failure this fixes. The hive process cannot chown (it dropped root), so the
// ownership repair half is left to the next boot's walk; the marker unblocks
// launch under the same fail-open contract.
//
// Never overwrites a marker that already carries the expected contents, and
// does nothing on legacy deployments with no marker contract.
func (m *Manager) publishRuntimeUIDIsolationMarker(name string) {
	if m.uidMap == nil {
		return
	}
	markerDir, revision, uid := m.uidMap.IsolationContract(name)
	if markerDir == "" || revision == "" || uid <= 0 {
		return
	}
	marker := filepath.Join(markerDir, fmt.Sprintf("agent-%d.ready", uid))
	expected := revision + ":" + strconv.Itoa(uid)
	if data, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(data)) == expected {
		return
	}
	tmp := marker + ".tmp"
	if err := os.WriteFile(tmp, []byte(expected+"\n"), 0o644); err != nil {
		m.logger.Warn("could not publish runtime UID-isolation marker; agent launch will hold until the next boot's migration walk",
			"agent", name, "uid", uid, "marker", marker, "error", err)
		return
	}
	if err := os.Rename(tmp, marker); err != nil {
		_ = os.Remove(tmp)
		m.logger.Warn("could not publish runtime UID-isolation marker; agent launch will hold until the next boot's migration walk",
			"agent", name, "uid", uid, "marker", marker, "error", err)
		return
	}
	m.logger.Info("published UID-isolation marker for runtime-added agent",
		"agent", name, "uid", uid, "marker", marker)
}

// awaitUIDIsolation keeps expensive PVC ownership walks off the pod startup
// path without weakening per-agent UID isolation. The entrypoint starts the
// root repair worker asynchronously, allowing the dashboard/startup probe to
// become healthy; each agent waits here before sanitizeGitRemotes, tmux setup,
// or any backend process can touch its persistent tree.
func (m *Manager) awaitUIDIsolation(ctx context.Context, agent *AgentProcess) error {
	if m.uidMap == nil || agent.UID <= 0 {
		return nil
	}
	markerDir, revision, uid := m.uidMap.IsolationContract(agent.Name)
	if uidIsolationMarkersReady(markerDir, revision, uid) {
		return nil
	}

	m.logger.Info("agent launch held for UID-isolation migration",
		"agent", agent.Name,
		"uid", uid,
		"marker_dir", markerDir,
		"revision", revision,
	)
	ticker := time.NewTicker(uidIsolationPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for agent %s UID-isolation migration: %w", agent.Name, ctx.Err())
		case <-ticker.C:
			if uidIsolationMarkersReady(markerDir, revision, uid) {
				m.logger.Info("UID-isolation migration complete; releasing agent launch",
					"agent", agent.Name,
					"uid", uid,
					"revision", revision,
				)
				return nil
			}
		}
	}
}

// SharedDevUID reports whether this manager is running every agent on the
// shared dev UID: no UID map was loaded at boot (the "no UID map found, agents
// will share dev UID" path), or the map exists but no agent was allocated a
// per-agent UID. Either way no agent gets its own scoped GitHub token cache.
// The dashboard turns this into an operator-visible health check (#11713)
// when the deployment was expected to isolate agents.
func (m *Manager) SharedDevUID() bool {
	if m.uidMap == nil {
		return true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.agents) == 0 {
		return false
	}
	for _, a := range m.agents {
		if a.UID > 0 {
			return false
		}
	}
	return true
}
