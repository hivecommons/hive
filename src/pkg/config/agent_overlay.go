package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
		agent.sourceFile = path
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
	kept := make(map[string]AgentConfig, len(overlays))
	for name, agent := range overlays {
		if err := c.validateAgentOverlay(name, agent); err != nil {
			_, hasBase := c.Agents[name]
			slog.Default().Error("config: rejected invalid per-agent overlay file - the hive is booting WITHOUT it so its dashboard stays reachable; fix the file or the agent via the dashboard API (#6024)",
				"hive_id", c.HiveID,
				"agent", name,
				"file", agent.sourceFile,
				"error", err.Error(),
				"fell_back_to_base_config", hasBase,
			)
			continue
		}
		kept[name] = agent
	}
	return kept
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
	if !ValidateExplainMode(agent.ExplainMode) {
		return fmt.Errorf("invalid explain_mode %q (must be off, brief, or full, or empty to inherit %s)", agent.ExplainMode, ExplainModeEnvVar)
	}
	if err := validateChannels(name, agent.Channels); err != nil {
		return err
	}
	if err := validateTools(name, agent.Tools); err != nil {
		return err
	}
	return validateConnections(name, agent.Connections)
}

// MergeAgentOverrides merges overlay agents into the config's agent map.
// Overlay agents override base config agents with the same name.
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
		agent.Managed = true
		c.Agents[name] = agent
	}
	c.PruneRemovedAgents()
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
func SaveAgentFile(dir, name string, agent AgentConfig) error {
	if strings.Contains(name, "..") || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("invalid agent name %q", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating agent overlay dir: %w", err)
	}

	// Don't persist internal-only fields
	agent.Managed = false
	agent.name = ""
	agent.sourceFile = ""
	agent.clearOnKickSet = false

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
	agent.name = name
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
	if !agent.Enabled && !agent.enabledSet {
		agent.Enabled = true
	}
	if !agent.clearOnKickSet {
		agent.ClearOnKick = true
	}
	if agent.Role == "" {
		agent.Role = name
	}
	applyKnownAgentDefaults(name, &agent)
	c.Agents[name] = agent
}
