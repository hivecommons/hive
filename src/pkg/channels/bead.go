package channels

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const beadPollIntervalS = 30

// BeadWatcher polls bead stores and triggers agents based on match criteria.
type BeadWatcher struct {
	mgr     *Manager
	mu      sync.Mutex
	agents  map[string]config.AgentConfig
	seen    map[string]bool
	cancel  context.CancelFunc
}

// NewBeadWatcher creates a new bead watcher.
func NewBeadWatcher(mgr *Manager) *BeadWatcher {
	return &BeadWatcher{
		mgr:  mgr,
		seen: make(map[string]bool),
	}
}

// Start begins polling bead stores.
func (b *BeadWatcher) Start(ctx context.Context) {
	b.mu.Lock()
	b.agents = b.mgr.getAgents()
	childCtx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	b.mu.Unlock()

	go b.poll(childCtx)
}

// Stop halts the bead watcher.
func (b *BeadWatcher) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
}

// Rebuild updates the agent config for the watcher.
func (b *BeadWatcher) Rebuild(agents map[string]config.AgentConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.agents = agents
}

func (b *BeadWatcher) poll(ctx context.Context) {
	ticker := time.NewTicker(beadPollIntervalS * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.check()
		}
	}
}

func (b *BeadWatcher) check() {
	b.mu.Lock()
	agents := b.agents
	b.mu.Unlock()

	for agentName, agentCfg := range agents {
		for _, ch := range agentCfg.ChannelsOfType("bead") {
			if !ch.IsEnabled() {
				continue
			}
			b.checkBeadsForAgent(agentName, agentCfg, ch)
		}
	}
}

func (b *BeadWatcher) checkBeadsForAgent(agentName string, agentCfg config.AgentConfig, ch config.ChannelConfig) {
	beadsDir := agentCfg.BeadsDir
	if beadsDir == "" {
		return
	}

	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		beadID := entry.Name()
		seenKey := agentName + ":" + beadID
		if b.seen[seenKey] {
			continue
		}

		beadPath := filepath.Join(beadsDir, beadID)
		if b.matchesBead(beadPath, ch.Match) {
			b.seen[seenKey] = true
			ctx := TriggerContext{
				ChannelType: "bead",
				Event:       beadID,
				Payload: map[string]string{
					"bead_id":   beadID,
					"bead_path": beadPath,
				},
			}
			go b.mgr.triggerKick(agentName, ctx)
		}
	}
}

func (b *BeadWatcher) matchesBead(path string, match map[string]string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	var metadata map[string]interface{}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return false
	}

	for key, expected := range match {
		val, ok := metadata[key]
		if !ok {
			return false
		}
		valStr, ok := val.(string)
		if !ok || valStr != expected {
			return false
		}
	}
	return true
}
