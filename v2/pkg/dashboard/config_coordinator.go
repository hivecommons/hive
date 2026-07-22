package dashboard

import (
	"fmt"
	"sort"
	"sync"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/governor"
)

// ConfigSaveFunc persists one complete candidate config. The main service
// replaces the default with the config watcher's digest-aware save wrapper.
type ConfigSaveFunc func(*config.Config) error

// ConfigCoordinator is the single writer for runtime config. It stages each
// mutation in a detached candidate, persists the complete candidate, prepares
// enabled Manager entries, then publishes Config and Governor snapshots.
type ConfigCoordinator struct {
	mu       sync.Mutex
	config   *config.Config
	governor *governor.Governor
	manager  *agent.Manager
	save     ConfigSaveFunc
}

func NewConfigCoordinator(cfg *config.Config, gov *governor.Governor, manager *agent.Manager) *ConfigCoordinator {
	return &ConfigCoordinator{config: cfg, governor: gov, manager: manager}
}

// SetSaveFunc installs the persistence function used by later mutations.
func (c *ConfigCoordinator) SetSaveFunc(save ConfigSaveFunc) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.save = save
}

// Snapshot returns a fully detached view of the currently published config.
func (c *ConfigCoordinator) Snapshot() *config.Config {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.config == nil {
		return nil
	}
	return c.config.Clone()
}

// Mutate stages, persists, and publishes one complete config transaction.
func (c *ConfigCoordinator) Mutate(mutate func(*config.Config) error) error {
	if c == nil || c.config == nil {
		return fmt.Errorf("runtime config coordinator is not configured")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	candidate := c.config.Clone()
	if mutate != nil {
		if err := mutate(candidate); err != nil {
			return err
		}
	}
	if err := c.persistLocked(candidate); err != nil {
		return err
	}
	c.publishLocked(candidate)
	return nil
}

// Replace publishes a config loaded by the external file watcher without
// writing it back. prepare runs under the same writer lock and may preserve
// runtime-only fields from current into candidate.
func (c *ConfigCoordinator) Replace(candidate *config.Config, prepare func(current, candidate *config.Config) error) error {
	if c == nil || c.config == nil || candidate == nil {
		return fmt.Errorf("runtime config replacement is not configured")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	next := candidate.Clone()
	if prepare != nil {
		if err := prepare(c.config.Clone(), next); err != nil {
			return err
		}
	}
	c.publishLocked(next)
	return nil
}

// PublishCurrent reconciles config mutated during single-threaded startup.
func (c *ConfigCoordinator) PublishCurrent() {
	if c == nil || c.config == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.publishLocked(c.config.Clone())
}

func (c *ConfigCoordinator) persistLocked(candidate *config.Config) error {
	if c.save != nil {
		return c.save(candidate)
	}
	if candidate.SourcePath == "" {
		return nil
	}
	return candidate.Save()
}

func (c *ConfigCoordinator) publishLocked(candidate *config.Config) {
	published := candidate.Clone()
	previousManagerAgents := c.prepareManagerLocked(published)

	*c.config = *published
	if c.governor != nil {
		c.governor.UpdateConfigAndAgents(published.Governor, published.EnabledAgents())
	}

	// Removal follows Governor publication so a concurrently evaluating
	// Governor cannot select an entry after its Manager process disappeared.
	if c.manager != nil {
		for _, name := range previousManagerAgents {
			if _, configured := published.Agents[name]; !configured {
				c.manager.RemoveAgent(name)
			}
		}
	}
}

// prepareManagerLocked makes every enabled candidate addressable before the
// Governor can admit or cadence-kick it. Existing disabled entries retain
// their process record but receive the current config.
func (c *ConfigCoordinator) prepareManagerLocked(candidate *config.Config) []string {
	if c.manager == nil {
		return nil
	}
	statuses := c.manager.AllStatuses()
	previous := make([]string, 0, len(statuses))
	for name := range statuses {
		previous = append(previous, name)
	}
	sort.Strings(previous)

	names := make([]string, 0, len(candidate.Agents))
	for name := range candidate.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		agentConfig := config.CloneAgentConfig(candidate.Agents[name])
		if _, exists := statuses[name]; exists {
			_ = c.manager.UpdateConfig(name, agentConfig)
			continue
		}
		if agentConfig.Enabled {
			c.manager.AddAgent(name, agentConfig)
		}
	}
	return previous
}
