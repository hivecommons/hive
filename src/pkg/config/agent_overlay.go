package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadAgentOverrides reads all .yaml files from dir and returns them as a map
// of agent name → AgentConfig. Each file should contain a single agent config
// with the filename (minus extension) used as the agent name.
func LoadAgentOverrides(dir string) (map[string]AgentConfig, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading agent overlay dir %s: %w", dir, err)
	}

	agents := make(map[string]AgentConfig)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".yaml")
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading agent file %s: %w", path, err)
		}

		var agent AgentConfig
		if err := yaml.Unmarshal(data, &agent); err != nil {
			return nil, fmt.Errorf("parsing agent file %s: %w", path, err)
		}
		agent.Managed = true
		agent.SetLoadState(agent.Name(), path)
		agents[name] = agent
	}
	return agents, nil
}

// RejectInvalidAgentOverlays drops every overlay entry that cannot pass
// validation, returning the survivors. It is the blast-radius fix for #6024: a
// single contradictory per-agent file used to fail the whole config load, and
// because the process exits before the dashboard binds, the documented API fix
// (PUT /api/config/agent/{name}/...) was unreachable. One bad overlay bricked
// the entire hive with no supported way back in.
//
// Skipping means the OVERLAY is discarded, not the agent: the main config's
// entry for that name survives untouched and the agent keeps running on it, so
// the set of agents that run does not change. Only when no base entry exists is
// the agent genuinely absent - and in that case it could never have started
// with this config anyway.
//
// Every skip is logged at ERROR naming both the agent and the file, because
// the config the hive is running is now knowingly not the config on disk.
func (c *Config) RejectInvalidAgentOverlays(overlays map[string]AgentConfig) map[string]AgentConfig {
	if len(overlays) == 0 {
		return overlays
	}
	for _, name := range c.Governor.ClearInvalidInferenceLaunchCmds(overlays) {
		slog.Default().Warn("config: cleared invalid inference launch_cmd from per-agent overlay; inference and gateway backends launch through claude",
			"hive_id", c.HiveID,
			"agent", name,
			"file", func() string { o := overlays[name]; return o.SourceFile() }())
	}
	kept := make(map[string]AgentConfig, len(overlays))
	for name, agent := range overlays {
		if err := c.validateAgentOverlay(name, agent); err != nil {
			_, hasBase := c.Agents[name]
			slog.Default().Error("config: rejected invalid per-agent overlay file - the hive is booting WITHOUT it so its dashboard stays reachable; fix the file or the agent via the dashboard API (#6024)",
				"hive_id", c.HiveID,
				"agent", name,
				"file", agent.SourceFile(),
				"error", err.Error(),
				"fell_back_to_base_config", hasBase,
			)
			continue
		}
		kept[name] = agent
	}
	return kept
}

// LaunchCmdDegradation records one agent whose base-config launch_cmd the
// loader dropped because it contradicted the agent's backend.
type LaunchCmdDegradation struct {
	Agent     string
	Backend   string
	LaunchCmd string
	Error     string
}

// DegradeContradictingLaunchCmds drops, from every agent in the merged map,
// a launch_cmd that contradicts the agent's backend, and records each drop in
// c.LaunchCmdDegradations. It is the base-config counterpart of
// RejectInvalidAgentOverlays: that gate skips a contradictory per-agent FILE
// and "falls back to base config", which is useless when the base entry has
// the same defect — and the base config has no further layer to fall back
// to, so validation used to exit the process and the hive crash-looped until
// someone hand-edited the PVC.
//
// backend is the operator-owned field; launch_cmd is the stale derived one
// (the dashboard fills it from the backend when it is empty), so dropping
// launch_cmd is always the safe direction: the agent launches on its declared
// backend's default command. Should clearing not satisfy the gate, the entry
// is left alone and validation fails as before.
//
// Every drop is logged at ERROR in the same shape as the overlay rejection,
// because the config the hive is running is now knowingly not the config on
// disk; the dashboard raises a banner from the recorded list.
func (c *Config) DegradeContradictingLaunchCmds() []LaunchCmdDegradation {
	var degraded []LaunchCmdDegradation
	for name, agent := range c.Agents {
		err := c.Governor.ValidateLaunchCmdBackend(agent.Backend, agent.LaunchCmd)
		if err == nil {
			continue
		}
		if c.Governor.ValidateLaunchCmdBackend(agent.Backend, "") != nil {
			continue
		}
		d := LaunchCmdDegradation{Agent: name, Backend: agent.Backend, LaunchCmd: agent.LaunchCmd, Error: err.Error()}
		slog.Default().Error("config: dropped launch_cmd that contradicts the agent's backend - the hive is booting WITHOUT it so its dashboard stays reachable; the agent launches on its backend's default command until launch_cmd is fixed via the dashboard API",
			"hive_id", c.HiveID,
			"agent", name,
			"file", agent.SourceFile(),
			"backend", agent.Backend,
			"launch_cmd", agent.LaunchCmd,
			"error", err.Error(),
		)
		agent.LaunchCmd = ""
		c.Agents[name] = agent
		degraded = append(degraded, d)
	}
	sort.Slice(degraded, func(i, j int) bool { return degraded[i].Agent < degraded[j].Agent })
	c.LaunchCmdDegradations = append(c.LaunchCmdDegradations, degraded...)
	return degraded
}

// validateAgentOverlay runs the per-agent gates that an overlay file can
// realistically violate. Deliberately the same calls validateAgents makes, so
// a file rejected here is exactly a file that would have failed the whole load.
func (c *Config) validateAgentOverlay(name string, agent AgentConfig) error {
	if err := c.Governor.ValidateBackend(agent.Backend); err != nil {
		return err
	}
	if err := c.Governor.ValidateLaunchCmdBackend(agent.Backend, agent.LaunchCmd); err != nil {
		return err
	}
	if !ValidateCavemanMode(agent.CavemanMode) {
		return fmt.Errorf("invalid caveman_mode %q (must be lite, full, ultra, or wenyan)", agent.CavemanMode)
	}
	if !ValidateJevMode(agent.JevMode) {
		return fmt.Errorf("invalid jev_mode %q (must be off or assist)", agent.JevMode)
	}
	if !ValidateExplainMode(agent.ExplainMode) {
		return fmt.Errorf("invalid explain_mode %q (must be off, brief, or full, or empty to inherit %s)", agent.ExplainMode, ExplainModeEnvVar)
	}
	if err := ValidateKickTemplateName(agent.KickTemplate); err != nil {
		return err
	}
	if err := ValidateExperimentEngine(effectiveRole(name, agent), agent.Engine); err != nil {
		return err
	}
	if err := ValidateBobDisplayName(agent.BobDisplayName); err != nil {
		return err
	}
	if err := ValidateBobSessionLabel("bob.session_label", strings.TrimSpace(agent.Bob.SessionLabel)); err != nil {
		return err
	}
	if err := validateChannels(name, agent.Channels); err != nil {
		return err
	}
	if err := validateTools(name, agent.Tools); err != nil {
		return err
	}
	if err := validateAgentVariables(name, agent.Variables); err != nil {
		return err
	}
	return validateConnections(name, agent.Connections)
}

// MergeAgentOverrides merges overlay agents into the config's agent map.
// Overlay agents override base config agents with the same name, except that
// a hand-set base backend outranks a pack-owned overlay backend (see
// keepHandSetBackend, #13221).
//
// Tombstoned agents (Config.RemovedAgents) are skipped AND evicted from the
// base map. Both halves matter: skipping stops a stale
// /data/agent-configs/<name>.yaml from resurrecting a deleted agent, and the
// eviction stops the ConfigMap seed — which the dashboard cannot rewrite —
// from doing the same. Before this, either source alone brought the agent
// back on the very next config reload.
func (c *Config) MergeAgentOverrides(overlays map[string]AgentConfig) {
	if c.Agents == nil {
		c.Agents = make(map[string]AgentConfig)
	}
	for name, agent := range overlays {
		if c.IsAgentRemoved(name) {
			// Observability (#2439): a tombstoned agent still had a per-agent
			// overlay file on disk (it is a key in overlays) — the stale-file
			// resurrection vector — and the tombstone guard just won over it.
			// Log at INFO once per skip: this line firing proves the guard held.
			slog.Default().Info("merge: skipped tombstoned agent",
				"hive_id", c.HiveID,
				"agent", name,
				"had_overlay_file", true,
			)
			continue
		}
		if base, ok := c.Agents[name]; ok {
			agent = c.keepHandSetBackend(name, base, agent)
		}
		agent.Managed = true
		c.Agents[name] = agent
	}
	c.PruneRemovedAgents()
}

// keepHandSetBackend stops a pack-written overlay from shadowing a backend an
// operator wrote by hand into the base config (#13221). ApplyPack persists
// every agent it reconciles to the overlay layers with backend_owner: pack;
// because an overlay replaces the whole base entry, a later hand edit of
// `backend:` in hive.yaml lost to that stale pack value on the next load, and
// ApplyPack then saw nothing to change. A base backend with no owner stamp
// that differs from a pack-owned overlay backend is an operator edit, so it
// wins; its owner stays empty so ApplyPack treats it as hand-set (and stamps
// it operator) instead of reconciling it. Operator-owned or unowned overlay
// backends, and an empty base backend, keep the overlay value as before.
//
// Only an UNSTAMPED base backend counts as a hand edit. A base entry whose
// backend: line alone was edited while it still carries a backend_owner: line
// — pack from a runtime file saved before the first apply, or operator after
// Save() wrote the stamp back — loses to the overlay: a pack-owned base/overlay
// pair that disagree can also be a runtime file saved before a pack upgrade,
// and keeping the stale base there would pin an old pack backend for good. The
// changelog fragment and docs/agent-configuration.md name the workaround
// (delete the backend_owner: line too, whatever its value).
func (c *Config) keepHandSetBackend(name string, base, overlay AgentConfig) AgentConfig {
	if base.Backend == "" || base.BackendOwner != "" || overlay.BackendOwner != FieldOwnerPack ||
		strings.EqualFold(base.Backend, overlay.Backend) {
		return overlay
	}
	if c.Governor.ValidateBackend(base.Backend) != nil || c.Governor.ValidateLaunchCmdBackend(base.Backend, overlay.LaunchCmd) != nil {
		return overlay
	}
	slog.Default().Info("config: keeping hand-set agent backend over pack-owned overlay value",
		"hive_id", c.HiveID,
		"agent", name,
		"backend", base.Backend,
		"overlay_backend", overlay.Backend,
		"file", overlay.SourceFile(),
	)
	overlay.Backend = base.Backend
	overlay.BackendOwner = ""
	return overlay
}

// PruneRemovedAgents drops every tombstoned agent from the in-memory agent
// map. Safe to call repeatedly; a no-op when nothing is tombstoned.
func (c *Config) PruneRemovedAgents() {
	for _, name := range c.RemovedAgents {
		delete(c.Agents, name)
	}
}

// IsAgentRemoved reports whether an operator deliberately deleted this agent.
func (c *Config) IsAgentRemoved(name string) bool {
	for _, removed := range c.RemovedAgents {
		if removed == name {
			return true
		}
	}
	return false
}

// MarkAgentRemoved records a deliberate deletion so no later merge, reload, or
// ACMM pack apply brings the agent back. Returns true when the tombstone was
// newly added (a repeat delete of the same agent is a no-op).
func (c *Config) MarkAgentRemoved(name string) bool {
	if name == "" || c.IsAgentRemoved(name) {
		return false
	}
	c.RemovedAgents = append(c.RemovedAgents, name)
	return true
}

// ClearAgentRemoved lifts a tombstone. This is the operator explicitly
// re-adding an agent they had previously deleted — the one unambiguous signal
// that the deletion no longer reflects their intent. Returns true when a
// tombstone was actually lifted.
func (c *Config) ClearAgentRemoved(name string) bool {
	kept := make([]string, 0, len(c.RemovedAgents))
	for _, removed := range c.RemovedAgents {
		if removed != name {
			kept = append(kept, removed)
		}
	}
	if len(kept) == len(c.RemovedAgents) {
		return false
	}
	c.RemovedAgents = kept
	return true
}

// SaveAgentFile writes a single agent config to dir/<name>.yaml.
//
// It refuses a backend/launch_cmd contradiction: the loader rejects such a
// file on the next boot (RejectInvalidAgentOverlays), so writing it would
// only defer the failure to a restart. The gateway-aware half of the gate
// needs the governor config; callers that have it should also run
// Config.ValidateAgentLaunchCmds before calling this.
func SaveAgentFile(dir, name string, agent AgentConfig) error {
	if strings.Contains(name, "..") || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("invalid agent name %q", name)
	}
	if err := (GovernorConfig{}).ValidateLaunchCmdBackend(agent.Backend, agent.LaunchCmd); err != nil {
		return fmt.Errorf("refusing to save agent overlay %s: %w", name, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating agent overlay dir: %w", err)
	}

	// Don't persist internal-only fields
	agent.Managed = false
	agent.ClearLoadState()

	data, err := yaml.Marshal(&agent)
	if err != nil {
		return fmt.Errorf("marshaling agent %s: %w", name, err)
	}

	path := filepath.Join(dir, name+".yaml")
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("writing agent file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming agent file %s: %w", path, err)
	}
	return nil
}

// RemoveAgentFile deletes dir/<name>.yaml.
func RemoveAgentFile(dir, name string) error {
	if strings.Contains(name, "..") || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("invalid agent name %q", name)
	}
	path := filepath.Join(dir, name+".yaml")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing agent file %s: %w", path, err)
	}
	return nil
}

// ApplyAgentDefaults runs the same defaulting logic as applyDefaults for a
// single agent entry. Call this after adding an agent at runtime.
func (c *Config) ApplyAgentDefaults(name string) {
	agent, ok := c.Agents[name]
	if !ok {
		return
	}
	agent.SetLoadState(name, agent.SourceFile())
	if agent.ID == "" {
		agent.ID = name
	}
	if agent.Replicas == 0 {
		agent.Replicas = 1
	}
	if agent.BeadsDir == "" {
		agent.BeadsDir = fmt.Sprintf("/data/beads/%s", name)
	}
	// Default to enabled unless the user explicitly set enabled: false.
	if !agent.Enabled && !agent.EnabledExplicitlySet() {
		agent.Enabled = true
	}
	if !agent.ClearOnKickExplicitlySet() {
		agent.ClearOnKick = true
	}
	if agent.Role == "" {
		agent.Role = name
	}
	if agent.IsExperimenter() && agent.Engine == "" {
		agent.Engine = DefaultExperimentEngine
	}
	applyKnownAgentDefaults(name, &agent)
	c.Agents[name] = agent
}

// DefaultAgentOverlayDir is the directory Load falls back to for per-agent overlays.
// It is a var so tests and hermetic binaries can redirect the host-local path.
var DefaultAgentOverlayDir = "/data/agent-configs"
