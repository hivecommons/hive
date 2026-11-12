package config

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

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
	// A backend/launch_cmd contradiction written here is a config the loader
	// will reject on the next boot. Refuse it now, while the caller can still
	// report the error, instead of persisting a crash-loop.
	if err := c.ValidateAgentLaunchCmds(); err != nil {
		log.Printf("WARNING: config.Save() blocked — %v", err)
		return err
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
	err := c.saveLocked()
	c.syncAgentOverlayPausedLocked(name, paused)
	return true, err
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
	var changed []string
	for name, paused := range livePaused {
		if ac, ok := c.Agents[name]; ok && ac.Paused != paused {
			ac.Paused = paused
			c.Agents[name] = ac
			changed = append(changed, name)
		}
	}
	err := c.saveLocked()
	for _, name := range changed {
		c.syncAgentOverlayPausedLocked(name, livePaused[name])
	}
	return err
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

	saveSeq.Add(1)
	notifySaveObserver()

	// Write the dashboard overlay before touching the watched primary config.
	// The fsnotify watcher reloads from LoadWithDashboardOverlay(c.SourcePath)
	// after the primary write has been quiet for debounceDelay. On slow PVCs,
	// writing the overlay last allowed that reload to see the previous overlay
	// and resurrect old whole-agent entries (kick_template/mode/model) over the
	// freshly reconciled in-memory config. Installing the atomic overlay first
	// means any reload triggered by the primary write observes the same agent
	// layer this Save is about to persist.
	overlayErr := c.saveDashboardOverlay()

	// Replace the primary config atomically (temp file + fsync + rename onto
	// the resolved target) — see writeFileAtomic for why that is safe on the
	// #17468 symlink and still works on single-file Docker bind mounts.
	//
	// #17862: the previous O_WRONLY|O_TRUNC in-place write could be killed
	// between truncate and write (liveness probe on a crash-looping hive),
	// leaving a 0-byte runtime config that the entrypoint skipped in favour
	// of a months-stale hive.yaml.bak. Pinned by TestSaveSurvivesKillMidWrite.
	//
	// #3961: a source-path failure must NOT abort the save. On deployments
	// that mount the config read-only (a ConfigMap mounted straight at
	// /etc/hive/hive.yaml — the issue's k3s case), this write can NEVER
	// succeed, and returning here would skip the remaining durable layer.
	// The dashboard overlay has already been attempted so a watcher reload
	// cannot beat it; the PVC runtime config is still written below. Together
	// those are the layers that survive a pod restart (runtime is the K8s
	// recovery copy and Docker/LXC boot input; the dashboard overlay is the
	// K8s first-boot/reprovision merge input). Record the source failure, keep
	// writing the runtime layer, and report success iff the state will actually
	// survive a restart.
	var srcErr error
	if err := writeFileAtomic(c.SourcePath, data, sourceConfigMode(c.SourcePath)); err != nil {
		srcErr = fmt.Errorf("writing config: %w", err)
	}

	// Persist the runtime config to the PVC. In K8s this is the boot input
	// after first boot (#2392): the entrypoint boots from it and the seed +
	// overlay merge runs only when it is absent. On hosted spokes
	// SourcePath already links to it (#17468), so this write is the same
	// file a second time; it still matters on the restart right after
	// first boot and on legacy-.bak-only spokes, where the config path is
	// a plain copy. In Docker/LXC it IS the boot-time source of truth,
	// since there is no ConfigMap and no overlay there. The entrypoint
	// decides which applies.
	//
	// Always written under the new name. The legacy file is never written,
	// renamed or removed here — see RuntimeConfigFileLegacy.
	runtimePath := RuntimeConfigFile
	var runtimeErr error
	// 0600, not 0644: the marshaled config carries dashboard.auth_token (and
	// github.token in PAT mode), and /data is world-traversable on hive
	// hosts, so a group/world-readable runtime config hands the dashboard
	// owner credential to every unprivileged agent user (#5331).
	if guardLivePVCPathUnderTest(runtimePath, defaultRuntimeConfigFile, "RuntimeConfigFile") {
		runtimeErr = errLivePVCPathGuarded
	} else if err := writeFileAtomic(runtimePath, data, 0o600); err != nil {
		// Common cause: init container created the file as root, runtime user
		// can't overwrite. Remove and retry so runtime state is not silently lost.
		_ = os.Remove(runtimePath) // best-effort; the retry's own error is what's recorded below
		if retryErr := writeFileAtomic(runtimePath, data, 0o600); retryErr != nil {
			runtimeErr = retryErr
			log.Printf("[config] warning: failed to write PVC runtime config to %s (even after remove): %v", runtimePath, retryErr)
		} else {
			log.Printf("[config] PVC runtime config written to %s (recovered from permission error)", runtimePath)
		}
	} else {
		log.Printf("[config] PVC runtime config written to %s", runtimePath)
	}

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

// sourceConfigMode returns the mode Save should give the primary config
// path: the existing file's permission bits when it exists (an operator may
// have tightened them), else the historical 0644 default.
func sourceConfigMode(path string) os.FileMode {
	if fi, err := os.Stat(path); err == nil {
		return fi.Mode().Perm()
	}
	return 0o644
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
var RuntimeConfigFile = defaultRuntimeConfigFile

const defaultRuntimeConfigFile = "/data/hive.yaml.runtime"

// RuntimeConfigFileLegacy is the pre-rename name of RuntimeConfigFile.
//
// It is READ as a fallback and never written by Save: ~51 live hives carry
// only this file, and on Docker/LXC it is the single copy of their live
// configuration. Readers prefer RuntimeConfigFile and fall back to this one.
// The entrypoint refreshes it from a good RuntimeConfigFile on every clean
// boot (#17862) — a fallback frozen at the Aug 2026 rename booted a
// months-old level/roster when a kill mid-save truncated the runtime file.
//
// Removable one release after every live hive has written the new name.
const RuntimeConfigFileLegacy = "/data/hive.yaml.bak"

// ResolvedConfigPath is path with symlinks resolved, or path itself when it
// cannot be resolved. On hosted spokes /etc/hive/hive.yaml links to
// RuntimeConfigFile (#17468), so this names the file a boot actually read.
func ResolvedConfigPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// PersistedConfigPath names the file whose content survives a restart and is
// the next boot's input — the one an operator must edit. That is
// RuntimeConfigFile whenever the hive has one (every container deployment
// after its first boot), else the legacy name the entrypoint falls back to;
// otherwise the loaded config path, resolved, is the only copy (a bare binary
// on a host).
func PersistedConfigPath(sourcePath string) string {
	for _, p := range []string{RuntimeConfigFile, RuntimeConfigFileLegacy} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			return p
		}
	}
	return ResolvedConfigPath(sourcePath)
}

// DashboardOverlayFile is where Save() persists a secret-free copy of the
// dashboard-edited config on the PVC in Kubernetes mode. Before #2402 the
// copy-config init container re-seeded /etc/hive/hive.yaml from the
// ConfigMap on every pod boot, so without this overlay every dashboard save
// (LiteLLM endpoint, notifications, agent tweaks, ...) silently vanished on
// the next restart or upgrade. Since #2402 the runtime config is the boot
// input once it exists (and since #17468 the config path links to it), so
// the overlay is consumed only on a boot with no runtime config — first
// boot or a reprovision onto a fresh PVC — where the entrypoint merges it
// over the ConfigMap seed; the ConfigMap stays authoritative there for the
// hub/admin-managed keys (acmm_level, hub.is_public).
//
// A package var (not const) only so tests can point it at a temp dir; it
// never changes at runtime in production.
var DashboardOverlayFile = defaultDashboardOverlayFile

const defaultDashboardOverlayFile = "/data/hive.yaml.dashboard"

// guardLivePVCPathUnderTest prevents an in-pod `go test` run from writing
// fixture config into the live PVC files that hive boots from.
var errLivePVCPathGuarded = errors.New("live PVC config path not written from a test binary")

func guardLivePVCPathUnderTest(current, production, label string) bool {
	if !testing.Testing() || current != production {
		return false
	}
	log.Printf("[config] test binary: refusing to write the live %s at %s — point config.%s at a temp dir to exercise it", label, current, label)
	return true
}

// productionAgentOverlayDir is DefaultAgentOverlayDir's production value. A
// test binary never rewrites a per-agent overlay there.
const productionAgentOverlayDir = "/data/agent-configs"

// syncAgentOverlayPausedLocked keeps the paused bit in name's per-agent
// overlay file (Data.AgentsDir/<name>.yaml) equal to the pause just
// persisted to the main config. The overlay replaces the agent's whole
// main-config entry at load (MergeAgentOverrides), paused included, but it is
// written only by config edits (SaveAgentFile) and pack applies, never by a
// pause or resume. So it kept whatever the agent's state was at its last
// config edit: an agent whose model was changed while it was paused came
// back paused after a later resume and a hive restart, and because
// hive-state.json no longer recorded that pause, it came back with no
// trigger, reason, actor or time (an operator pause, as far as anything could
// tell). An agent with no overlay file is left alone. Failures are logged:
// the main config, the pause's authority, is already saved. Caller holds
// saveMu.
//
// Only the paused key is patched, in place (yaml.Node): every other key and
// comment in the file is kept byte-for-byte in meaning. Round-tripping the
// file through AgentConfig would not be: enabled and clear_on_kick default
// to true only while the key is absent (UnmarshalYAML), and marshaling
// writes them back as explicit false, so a hand-authored overlay without
// them would come back disabled on the next load.
func (c *Config) syncAgentOverlayPausedLocked(name string, paused bool) {
	dir := c.Data.AgentsDir
	if dir == "" || guardLivePVCPathUnderTest(dir, productionAgentOverlayDir, "AgentsDir") {
		return
	}
	if strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return
	}
	path := filepath.Join(dir, name+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[config] warning: could not read agent overlay %s to record its pause state: %v", path, err)
		}
		return
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		log.Printf("[config] warning: could not parse agent overlay %s to record its pause state: %v", path, err)
		return
	}
	changed, err := setOverlayPausedNode(&doc, paused)
	if err != nil {
		log.Printf("[config] warning: could not record pause state in agent overlay %s: %v", path, err)
		return
	}
	if !changed {
		return
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		log.Printf("[config] warning: could not record pause state in agent overlay %s: %v", path, err)
		return
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0o644); err != nil {
		log.Printf("[config] warning: could not record pause state in agent overlay %s: %v", path, err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		log.Printf("[config] warning: could not record pause state in agent overlay %s: %v", path, err)
	}
}

// setOverlayPausedNode sets the top-level paused key of a parsed overlay
// document to paused, adding the key when it is absent and paused is true
// (an absent key already means false). It reports whether the document
// changed.
func setOverlayPausedNode(doc *yaml.Node, paused bool) (bool, error) {
	root := doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			root.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
		}
		root = root.Content[0]
	}
	if root.Kind == 0 {
		// An empty file parses to an empty node; make it a document holding
		// one mapping so the key has somewhere to go.
		*doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return false, fmt.Errorf("top level is %v, not a mapping", root.Tag)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "paused" {
			continue
		}
		val := root.Content[i+1]
		var cur bool
		if err := val.Decode(&cur); err != nil {
			return false, fmt.Errorf("paused: %w", err)
		}
		if cur == paused {
			return false, nil
		}
		val.Kind = yaml.ScalarNode
		val.Tag = "!!bool"
		val.Style = 0
		val.Value = strconv.FormatBool(paused)
		val.Content = nil
		return true, nil
	}
	if !paused {
		return false, nil
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "paused"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	return true, nil
}

// saTokenFile is the Kubernetes serviceaccount token path IsKubernetesPod
// probes. It is a var (not a const) only so tests can point it at a
// non-existent path and stay hermetic on hosts that really are pods;
// production always uses the fixed in-cluster path.
var saTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// SetSATokenFileForTest points IsKubernetesPod's serviceaccount-token probe
// at path and returns a restore func. Out-of-package tests that need the
// non-Kubernetes branch call this with a non-existent path (alongside
// clearing KUBERNETES_SERVICE_HOST) so they stay hermetic on hosts that
// really are pods — in-cluster CI runners and dev hives.
func SetSATokenFileForTest(path string) func() {
	orig := saTokenFile
	saTokenFile = path
	return func() { saTokenFile = orig }
}

// IsKubernetesPod reports whether the process is running inside a
// Kubernetes pod (mirrors the entrypoint's IS_KUBERNETES detection).
func IsKubernetesPod() bool {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	_, err := os.Stat(saTokenFile)
	return err == nil
}

// saveDashboardOverlay writes the secret-free PVC overlay in Kubernetes
// mode. Failures are logged, never fatal — but they ARE returned (#3961):
// when the primary config path is unwritable (read-only ConfigMap mount)
// the overlay and the runtime config are the only layers that survive a pod
// restart, so saveLocked needs to know whether this write landed before it
// can report the save as durable. Outside Kubernetes it returns nil (the
// overlay is not part of the boot path there).
//
// The write MUST be atomic (temp file + rename), unlike saveLocked()'s
// inode-preserving write to the bind-mounted primary config. DashboardOverlayFile
// lives on the PVC (not a bind mount), so rename is safe here, and it is the
// only way to avoid a truncated/partial overlay if the pod is killed mid-write
// (a redeploy sends SIGTERM/SIGKILL at an arbitrary instant). A truncate-in-place
// write (os.WriteFile) can leave the file cut off partway through — GitHubConfig
// marshals AFTER Agents/Project in the Config struct field order (see the
// struct tags above), so a truncated overlay can silently keep valid
// project/agents blocks while losing app_id/installation_id/key_file entirely.
// The entrypoint's merge script only sanity-checks project.org and agents
// before trusting the overlay wholesale, so that truncated-but-plausible file
// would pass the guard and revert a dashboard-installed GitHub App to the
// placeholder ConfigMap seed on the next restart — exactly the durability bug
// this atomic write prevents.
func (c *Config) saveDashboardOverlay() error {
	if !IsKubernetesPod() {
		// Docker/LXC mode: RuntimeConfigFile is already the boot-time
		// source of truth there, so dashboard saves persist without an
		// overlay.
		return nil
	}
	if guardLivePVCPathUnderTest(DashboardOverlayFile, defaultDashboardOverlayFile, "DashboardOverlayFile") {
		return errLivePVCPathGuarded
	}
	data, err := c.dashboardOverlayBytes()
	if err != nil {
		log.Printf("[config] warning: failed to marshal dashboard overlay: %v", err)
		return err
	}
	tmpPath := DashboardOverlayFile + ".tmp"
	// 0600, not 0644: dashboardOverlayBytes only folds the dashboard auth
	// token back to its env form when it matches a bootstrap env var — a
	// dashboard-minted token is persisted verbatim, so the overlay is not
	// reliably secret-free (#5331).
	const overlayFileMode = 0o600
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, overlayFileMode)
	if err != nil {
		log.Printf("[config] warning: failed to open dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	// OpenFile's mode only applies on create; a leftover 0644 tmp file from a
	// crash before this fix would otherwise carry its old bits through the
	// rename. Best-effort: the rename below installs whatever mode f has.
	_ = f.Chmod(overlayFileMode)
	if _, err := f.Write(data); err != nil {
		_ = f.Close() // best-effort cleanup; the write error is what's returned
		log.Printf("[config] warning: failed to write dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close() // best-effort cleanup; the sync error is what's returned
		log.Printf("[config] warning: failed to fsync dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	if err := f.Close(); err != nil {
		log.Printf("[config] warning: failed to close dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	if err := os.Rename(tmpPath, DashboardOverlayFile); err != nil {
		log.Printf("[config] warning: failed to rename dashboard overlay into place %s (dashboard saves will not survive pod restarts): %v", DashboardOverlayFile, err)
		return err
	}
	log.Printf("[config] dashboard overlay written to %s (merged over the ConfigMap seed at next boot)", DashboardOverlayFile)
	return nil
}

// dashboardOverlayBytes marshals the config with env-derived secret VALUES
// collapsed back to their env-var forms, so the PVC overlay stays
// secret-free. Load() re-expands ${VAR} references and applyBootstrapEnv
// re-fills the dashboard auth token from the pod env, so nothing is lost.
func (c *Config) dashboardOverlayBytes() ([]byte, error) {
	// Shallow copy: top-level fields are struct values, so mutating the
	// copy's GitHub/Dashboard sections leaves the live config untouched
	// (the shared Agents map is not modified).
	cp := *c
	if tok := os.Getenv("HIVE_GITHUB_TOKEN"); tok != "" && cp.GitHub.Token == tok {
		cp.GitHub.Token = "${HIVE_GITHUB_TOKEN}"
	}
	for _, env := range []string{"DASHBOARD_AUTH_TOKEN", "HIVE_DASHBOARD_TOKEN"} {
		if v := os.Getenv(env); v != "" && cp.Dashboard.AuthToken == v {
			cp.Dashboard.AuthToken = ""
			break
		}
	}
	if cp.Dashboard.AuthToken != "" {
		if v := readDashboardAuthTokenFile(); v != "" && cp.Dashboard.AuthToken == v {
			cp.Dashboard.AuthToken = ""
		}
	}
	cp = *cp.redactedForPersist()
	return yaml.Marshal(&cp)
}

func (c *Config) redactedForPersist() *Config {
	if c == nil {
		return nil
	}
	cp := *c
	cp.OTel.Headers = envRedactedHeaders(cp.OTel.Headers)
	cp.Tracing.Headers = envRedactedHeaders(cp.Tracing.Headers)
	// Work-source credentials are persisted verbatim. The dashboard PUT stores
	// the operator's literal `${LINEAR_API_KEY}` reference (API saves are not
	// env-expanded) and worksource.FromConfig resolves it at the point of use,
	// so the reference round-trips through the overlay unchanged. Do NOT try
	// to "fold" a value back into ${VAR} by scanning the environment: any env
	// value that is a substring of the key (CI's ACCEPT_EULA=Y rewrote the
	// trailing Y of the literal reference) corrupts it.
	// #4041: never write the built-in login-pattern defaults as explicit
	// values. applyDefaults fills LoginPatterns on load, so by save time the
	// in-memory list always LOOKS explicit; marshaling it pins today's
	// defaults into the persisted config, where "defaults only apply to an
	// empty list" freezes them forever — exactly how every pre-#3959 hive
	// ended up stuck with the false-positive-prone generic list. A list equal
	// to the current defaults expresses no operator intent: persist it as
	// absent so future default fixes reach existing hives. An operator-
	// customized list differs from the defaults and is persisted verbatim.
	if stringSlicesEqual(cp.Governor.Sensing.LoginPatterns, defaultLoginPatterns) {
		cp.Governor.Sensing.LoginPatterns = nil
	}
	return &cp
}

func mergeOTelOverride(base, override OTelConfig) OTelConfig {
	merged := base
	if override.Enabled {
		merged.Enabled = true
	}
	if override.Endpoint != "" {
		merged.Endpoint = override.Endpoint
	}
	if len(override.Headers) > 0 {
		merged.Headers = override.Headers
	}
	if override.ServiceName != "" {
		merged.ServiceName = override.ServiceName
	}
	if override.Insecure {
		merged.Insecure = true
	}
	if override.SampleRatio != 0 {
		merged.SampleRatio = override.SampleRatio
	}
	return merged
}

func envRedactedHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return headers
	}
	out := make(map[string]string, len(headers))
	for k, value := range headers {
		out[k] = redactEnvExpandedValue(value)
	}
	return out
}

func redactEnvExpandedValue(value string) string {
	type envValue struct {
		name  string
		value string
	}
	values := make([]envValue, 0)
	for _, pair := range os.Environ() {
		name, val, ok := strings.Cut(pair, "=")
		if !ok || val == "" {
			continue
		}
		values = append(values, envValue{name: name, value: val})
	}
	sort.SliceStable(values, func(i, j int) bool {
		return len(values[i].value) > len(values[j].value)
	})

	redacted := value
	for _, item := range values {
		redacted = strings.ReplaceAll(redacted, item.value, "${"+item.name+"}")
	}
	return redacted
}

// ValidateAgentLaunchCmds runs the backend/launch_cmd gate over every agent.
// It is the write-side twin of the loader's check: the dashboard's
// persistence paths (Save, SaveAgentFile) call it so a contradiction is
// refused at save time rather than discovered as a fatal error at boot.
func (c *Config) ValidateAgentLaunchCmds() error {
	names := make([]string, 0, len(c.Agents))
	for name := range c.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		agent := c.Agents[name]
		if err := c.Governor.ValidateLaunchCmdBackend(agent.Backend, agent.LaunchCmd); err != nil {
			return fmt.Errorf("agent %s: %w", agentSourceLabel(name, agent.SourceFile()), err)
		}
	}
	return nil
}
