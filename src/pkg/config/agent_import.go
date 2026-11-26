package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
)

var (
	ErrImportAgentExists   = errors.New("agent already exists")
	ErrImportAgentInvalid  = errors.New("invalid imported agent")
	ErrImportAgentNoSource = errors.New("agent cadences require a writable config source")
	ErrImportAgentOverlay  = errors.New("dashboard overlay cannot store the imported agent and its cadences")
)

// agentImportMu keeps a reload from reading a partial import or rollback.
// Acquire it before saveMu when both are needed.
var agentImportMu sync.RWMutex

// completedAgentImports identifies reloads read before a committed import.
// Guarded by agentImportMu.
var completedAgentImports uint64

// ValidateImportedAgent checks an imported definition for preview.
func (c *Config) ValidateImportedAgent(name string, agent AgentConfig) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	return c.validateAgentOverlay(name, agent)
}

// withAgentCadences prepares an independent copy of the modes and ownership
// entries changed by an operator.
func (g GovernorConfig) withAgentCadences(name string, cadences map[string]Cadence) GovernorConfig {
	modes := make(map[string]ModeConfig, len(g.Modes))
	maps.Copy(modes, g.Modes)
	g.Modes = modes
	g.CadenceOwners = maps.Clone(g.CadenceOwners)
	for modeName, cadence := range cadences {
		mode := g.Modes[modeName]
		next := make(map[string]Cadence, len(mode.Cadences)+1)
		maps.Copy(next, mode.Cadences)
		next[name] = cadence
		mode.Cadences = next
		g.Modes[modeName] = mode
		if g.CadenceOwners != nil {
			g.CadenceOwners[modeName] = maps.Clone(g.CadenceOwners[modeName])
		}
		g.ClaimCadenceOwnership(modeName, name)
	}
	return g
}

// SaveAgentCadences saves the operator's selected schedules and returns the
// previous modes for auditing. The caller validates the values.
func (c *Config) SaveAgentCadences(name string, cadences map[string]Cadence) (map[string]ModeConfig, error) {
	saveMu.Lock()
	defer saveMu.Unlock()

	before := c.Governor.Modes
	next := *c
	next.Governor = c.Governor.withAgentCadences(name, cadences)
	if err := next.saveLocked(); err != nil {
		return nil, err
	}
	c.Governor.Modes = next.Governor.Modes
	c.Governor.CadenceOwners = next.Governor.CadenceOwners
	c.disk = next.disk
	return before, nil
}

// SaveImportedAgent installs the prompt and saves the agent with its cadences
// before publishing the roster to background saves and the dashboard.
// The caller validates the agent name and schedules.
func (c *Config) SaveImportedAgent(name string, agent AgentConfig, cadences map[string]Cadence, promptDir, prompt string) error {
	agentImportMu.Lock()
	defer agentImportMu.Unlock()
	saveMu.Lock()
	defer saveMu.Unlock()
	if _, exists := c.Agents[name]; exists {
		return fmt.Errorf("%w: %s", ErrImportAgentExists, name)
	}
	if c.Data.AgentsDir == "" {
		return errors.New("agents_dir not configured")
	}
	if len(cadences) > 0 && c.SourcePath == "" {
		return ErrImportAgentNoSource
	}
	if agent.ReplicaOf != "" {
		return fmt.Errorf("%w: import the declared agent instead of a derived replica", ErrImportAgentInvalid)
	}

	next := *c
	next.Agents = make(map[string]AgentConfig, len(c.Agents)+1)
	maps.Copy(next.Agents, c.Agents)
	if prompt != "" {
		agent.KickTemplate = name + ".md"
	}
	next.Agents[name] = agent
	next.ClearAgentRemoved(name)
	next.ApplyAgentDefaults(name)
	if err := next.validateAgentOverlay(name, next.Agents[name]); err != nil {
		return fmt.Errorf("%w: %w", ErrImportAgentInvalid, err)
	}
	if err := next.ExpandAgentReplicas(); err != nil {
		return fmt.Errorf("%w: %w", ErrImportAgentInvalid, err)
	}

	files := &agentImportFiles{
		sourcePath: ResolvedConfigPath(c.SourcePath),
		files:      make(map[string]*importFile),
	}
	if prompt != "" {
		if err := os.MkdirAll(promptDir, 0o755); err != nil {
			return fmt.Errorf("creating prompt directory: %w", err)
		}
		path := filepath.Join(promptDir, agent.KickTemplate)
		if err := files.write(path, []byte(prompt), sourceConfigMode(path)); err != nil {
			return files.rollback(fmt.Errorf("saving imported prompt: %w", err))
		}
	}
	// Publishing the overlay first would let a restart load the agent without
	// its cadences. Keep it staged while each config layer saves the agent
	// and its schedules together.
	overlayPath := filepath.Join(next.Data.AgentsDir, name+".yaml")
	staged := len(cadences) > 0
	if staged {
		if _, err := files.backup(overlayPath); err != nil {
			return files.rollback(fmt.Errorf("checking agent overlay: %w", err))
		}
	}
	if err := saveAgentFile(next.Data.AgentsDir, name, agent, files, staged); err != nil {
		return files.rollback(fmt.Errorf("saving imported agent: %w", err))
	}
	if staged {
		next.Governor = c.Governor.withAgentCadences(name, cadences)
		if c.disk != nil {
			disk := *c.disk
			disk.sums = maps.Clone(c.disk.sums)
			next.disk = &disk
		}
		if err := next.saveLockedWithImportFiles(files); err != nil {
			return files.rollback(fmt.Errorf("cadences not saved: %w", err))
		}
		if err := files.publishAgentOverlay(overlayPath); err != nil {
			return files.rollback(fmt.Errorf("publishing agent overlay: %w", err))
		}
	}

	c.Agents, c.RemovedAgents = next.Agents, next.RemovedAgents
	c.Governor.Modes, c.Governor.CadenceOwners = next.Governor.Modes, next.Governor.CadenceOwners
	c.disk = next.disk
	completedAgentImports++
	c.agentImportGeneration = completedAgentImports
	return nil
}
