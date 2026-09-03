package config

import (
	"fmt"
	"log"
	"os"

	"gopkg.in/yaml.v3"
)

func (c Config) MarshalYAML() (interface{}, error) {
	type plain Config
	out := plain(c)
	if c.Agents != nil {
		out.Agents = make(map[string]AgentConfig, len(c.Agents))
		for name, agent := range c.Agents {
			if agent.ReplicaOf != "" {
				continue
			}
			agent.ReplicaIndex = 0
			agent.ReplicaCount = 0
			out.Agents[name] = agent
		}
	}
	return out, nil
}

// validateSaveGuard checks that essential fields are present before allowing
// a config write. This prevents docker compose down -v (or similar) from
// causing Save() to overwrite hive.yaml with an empty/minimal config that
// would crash-loop on next startup.
func (c *Config) validateSaveGuard() error {
	if c.Project.Org == "" {
		log.Printf("WARNING: config.Save() blocked — project.org is empty, would corrupt hive.yaml")
		return fmt.Errorf("project.org is empty")
	}
	// Zero agents is a legitimate state when the operator deliberately deleted
	// them all: #2361's tombstones (RemovedAgents) are the durable record of
	// that intent. Blocking the save here would make the last deletion
	// unpersistable — the in-memory roster empties, the write is refused, and
	// the next reload restores the agents from the seed, silently undoing the
	// operator's action. That is precisely the "they always come back" bug
	// #2361 fixed, reintroduced through the save path.
	//
	// An empty roster with NO tombstones is still refused: that is the
	// truncated/uninitialised case this guard exists to catch. The two states
	// are distinguishable, so distinguish them rather than rejecting both.
	if len(c.Agents) == 0 && len(c.RemovedAgents) == 0 {
		log.Printf("WARNING: config.Save() blocked — no agents configured and no tombstones, would corrupt hive.yaml")
		return fmt.Errorf("no agents configured")
	}
	return nil
}

// Save marshals the current config back to its source YAML file using an
// inode-preserving write (open → truncate → write → sync). This is critical
// for Docker bind-mounted files: an atomic rename (temp + rename) replaces
// the inode, which silently breaks the bind mount — the host file is never
// updated, so changes are lost on container restart.
//
// As a safety measure, Save refuses to write if essential fields are missing
// (project.org, at least one agent). This prevents an empty or minimal config
// from overwriting the bind-mounted hive.yaml — a scenario that causes
// crash-loops on the next startup ("project.org is required").
func (c *Config) Save() error {
	saveMu.Lock()
	defer saveMu.Unlock()
	return c.saveLocked()
}

// SetAgentPausedAndSave atomically updates one agent's Paused field and
// persists the config, all under saveMu. This is the pause-callback path
// (AgentMgr.Pause/Resume). Doing the c.Agents read-modify-write and the Save
// under the SAME lock as every other saver eliminates both the map-mutation
// race (two goroutines writing c.Agents) and the file-level lost-write race.
// Returns whether a change was made (false when already at the target state).
func (c *Config) SetAgentPausedAndSave(name string, paused bool) (bool, error) {
	saveMu.Lock()
	defer saveMu.Unlock()
	ac, ok := c.Agents[name]
	if !ok || ac.Paused == paused {
		return false, nil
	}
	ac.Paused = paused
	c.Agents[name] = ac
	return true, c.saveLocked()
}

// ReconcilePausedAndSave sets each named agent's Paused field to the given
// live value and persists, all under saveMu. This is the async PersistFunc
// path (persistState): it carries the authoritative live paused set from the
// agent manager, so its write is a correcting one rather than a stale snapshot
// that could clobber a concurrent pause. Serializing it with SetAgentPausedAndSave
// under saveMu is what closes the race that dropped pauses when many agents
// were paused in quick succession.
func (c *Config) ReconcilePausedAndSave(livePaused map[string]bool) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	for name, paused := range livePaused {
		if ac, ok := c.Agents[name]; ok && ac.Paused != paused {
			ac.Paused = paused
			c.Agents[name] = ac
		}
	}
	return c.saveLocked()
}

// saveLocked performs the actual marshal-and-write. Callers MUST hold saveMu.
func (c *Config) saveLocked() error {
	if c.SourcePath == "" {
		return fmt.Errorf("config has no source path")
	}
	if err := c.validateSaveGuard(); err != nil {
		return fmt.Errorf("refusing to save invalid config: %w", err)
	}
	data, err := yaml.Marshal(c.redactedForPersist())
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	// Open the existing file (preserving its inode) rather than creating a
	// temp file and renaming. Rename breaks Docker bind mounts because it
	// replaces the inode — the host file is never updated, so acmm_level
	// and other runtime changes are lost on container restart.
	//
	// #3961: a source-path failure must NOT abort the save. On deployments
	// that mount the config read-only (a ConfigMap mounted straight at
	// /etc/hive/hive.yaml — the issue's k3s case), this write can NEVER
	// succeed, and returning here skipped exactly the two layers that DO
	// survive a pod restart: the PVC runtime config (which the entrypoint
	// boots from in both K8s steady state and Docker/LXC) and the dashboard
	// overlay (the K8s first-boot/reprovision merge input). The old early
	// return therefore made every runtime change — pause state, operator
	// model/backend ownership, ACMM level, gateway saves — evaporate on
	// every restart, while spamming "failed to persist" on every save.
	// Record the failure, keep writing the durable layers, and report
	// success iff the state will actually survive a restart.
	var srcErr error
	f, err := os.OpenFile(c.SourcePath, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		// File may not exist yet — fall back to create. Continue below so
		// the PVC backup and dashboard overlay are still written.
		if writeErr := os.WriteFile(c.SourcePath, data, 0o644); writeErr != nil {
			srcErr = fmt.Errorf("writing config (create fallback): %w", writeErr)
		}
	} else {
		if _, err := f.Write(data); err != nil {
			_ = f.Close() // best-effort cleanup; the write error is what's recorded
			srcErr = fmt.Errorf("writing config: %w", err)
		} else if err := f.Sync(); err != nil {
			_ = f.Close() // best-effort cleanup; the sync error is what's recorded
			srcErr = fmt.Errorf("syncing config: %w", err)
		} else if err := f.Close(); err != nil {
			srcErr = fmt.Errorf("closing config: %w", err)
		}
	}

	// Persist the runtime config to the PVC. In K8s this is a recovery copy
	// (the ConfigMap seed plus the overlay is authoritative); in Docker/LXC
	// it IS the boot-time source of truth, since there is no ConfigMap and
	// no overlay there. The entrypoint decides which applies.
	//
	// Always written under the new name. The legacy file is never written,
	// renamed or removed here — see RuntimeConfigFileLegacy.
	runtimePath := RuntimeConfigFile
	var runtimeErr error
	// 0600, not 0644: the marshaled config carries dashboard.auth_token (and
	// github.token in PAT mode), and /data is world-traversable on hive
	// hosts, so a group/world-readable runtime config hands the dashboard
	// owner credential to every unprivileged agent user (#5331).
	if err := os.WriteFile(runtimePath, data, 0o600); err != nil {
		// Common cause: init container created the file as root, runtime user
		// can't overwrite. Remove and retry so runtime state is not silently lost.
		_ = os.Remove(runtimePath) // best-effort; the retry's own WriteFile error is what's recorded below
		if retryErr := os.WriteFile(runtimePath, data, 0o600); retryErr != nil {
			runtimeErr = retryErr
			log.Printf("[config] warning: failed to write PVC runtime config to %s (even after remove): %v", runtimePath, retryErr)
		} else {
			log.Printf("[config] PVC runtime config written to %s (recovered from permission error)", runtimePath)
		}
	} else {
		log.Printf("[config] PVC runtime config written to %s", runtimePath)
		// os.WriteFile's mode only applies when it CREATES the file; a
		// pre-existing world-readable inode (every hive deployed before
		// this fix) keeps its old 0644 bits, so tighten explicitly.
		if chmodErr := os.Chmod(runtimePath, 0o600); chmodErr != nil {
			log.Printf("[config] warning: failed to tighten permissions on %s: %v", runtimePath, chmodErr)
		}
	}

	overlayErr := c.saveDashboardOverlay()

	if srcErr == nil {
		return nil
	}
	// The primary config path failed — a read-only mount, not a transient
	// error, in every observed case. When the boot-durable layers were both
	// written (the overlay write is a no-op outside Kubernetes), the state
	// WILL survive a restart, so this save has done its job: say so once per
	// failure mode instead of letting every caller raise a false
	// "will be lost on restart" alert on every save.
	if runtimeErr == nil && overlayErr == nil {
		log.Printf("[config] primary config path %s is not writable (%v) — state persisted to the PVC layers instead and will survive restarts (see RuntimeConfigFile)", c.SourcePath, srcErr)
		return nil
	}
	return srcErr
}

// RuntimeConfigFile is where Save() persists the full runtime config on the
// PVC. Its role differs by environment, which is exactly why the old
// hive.yaml.bak name was misleading enough to cost debugging time:
//
//   - Kubernetes: a post-merge SNAPSHOT. The entrypoint writes it after
//     merging the dashboard overlay over the ConfigMap seed, and reads it
//     back only in the disaster fallback (ConfigMap missing or empty).
//   - Docker/LXC: a live boot INPUT and the source of truth. There is no
//     ConfigMap and no overlay, so the entrypoint restores this file over
//     the config path on every boot. It is the only reason a dashboard save
//     survives a container recreation — see saveDashboardOverlay, which
//     early-returns outside Kubernetes for that reason.
//
// ".runtime" is accurate for both; ".bak" implied "the restorable backup",
// which is true only of the Kubernetes half.
// A package var (not const) only so tests can point it at a temp dir; it
// never changes at runtime in production (same convention as
// DashboardOverlayFile below).
var RuntimeConfigFile = "/data/hive.yaml.runtime"

// RuntimeConfigFileLegacy is the pre-rename name of RuntimeConfigFile.
//
// It is READ as a fallback and never written, renamed or removed: ~51 live
// hives carry only this file, and on Docker/LXC it is the single copy of
// their live configuration. Mutating it at boot could lose owner
// customisations with no warning, so the migration is copy-forward only —
// readers prefer RuntimeConfigFile and fall back to this one.
//
// Removable one release after every live hive has written the new name.
const RuntimeConfigFileLegacy = "/data/hive.yaml.bak"

// DashboardOverlayFile is where Save() persists a secret-free copy of the
// dashboard-edited config on the PVC in Kubernetes mode. The copy-config
// init container re-seeds /etc/hive/hive.yaml FROM THE CONFIGMAP on every
// pod boot, so without this overlay every dashboard save (LiteLLM
// endpoint, notifications, agent tweaks, ...) silently vanished on the
// next restart or upgrade. The entrypoint merges this file over the
// ConfigMap seed at boot; the ConfigMap stays authoritative for the
// hub/admin-managed keys (acmm_level, hub.is_public).
//
// A package var (not const) only so tests can point it at a temp dir; it
// never changes at runtime in production.
var DashboardOverlayFile = "/data/hive.yaml.dashboard"

// saTokenFile is the Kubernetes serviceaccount token path IsKubernetesPod
// probes. It is a var (not a const) only so tests can point it at a
// non-existent path and stay hermetic on hosts that really are pods;
