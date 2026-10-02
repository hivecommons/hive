package config

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
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

	// Open the existing file (preserving its inode) rather than creating a
	// temp file and renaming. Rename breaks Docker bind mounts because it
	// replaces the inode — the host file is never updated, so acmm_level
	// and other runtime changes are lost on container restart.
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
	if guardLivePVCPathUnderTest(runtimePath, defaultRuntimeConfigFile, "RuntimeConfigFile") {
		runtimeErr = errLivePVCPathGuarded
	} else if err := os.WriteFile(runtimePath, data, 0o600); err != nil {
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
var RuntimeConfigFile = defaultRuntimeConfigFile

const defaultRuntimeConfigFile = "/data/hive.yaml.runtime"

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
