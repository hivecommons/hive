package config

import (
	"fmt"
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
		agents[name] = agent
	}
	return agents, nil
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
