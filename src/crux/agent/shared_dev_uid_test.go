package agent

import (
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// SharedDevUID feeds the uid_isolation health check (#11713): it must report
// the shared-dev-UID fallback both when no UID map was loaded and when a map
// exists but every agent sits on UID 0, and must not fire for an isolated
// roster or an empty one.
func TestSharedDevUID(t *testing.T) {
	newMgr := func() *Manager {
		return NewManager(map[string]config.AgentConfig{}, slog.Default(), ProjectContext{ACMMLevel: 6})
	}

	t.Run("no uid map", func(t *testing.T) {
		m := newMgr()
		m.uidMap = nil
		m.agents["scanner"] = &AgentProcess{Name: "scanner"}
		if !m.SharedDevUID() {
			t.Fatal("nil UID map must report shared dev UID")
		}
	})

	t.Run("every agent on uid 0", func(t *testing.T) {
		m := newMgr()
		m.uidMap = NewUIDMap()
		m.agents["scanner"] = &AgentProcess{Name: "scanner"}
		m.agents["fixer"] = &AgentProcess{Name: "fixer"}
		if !m.SharedDevUID() {
			t.Fatal("UID map with every agent on UID 0 must report shared dev UID")
		}
	})

	t.Run("isolated agent", func(t *testing.T) {
		m := newMgr()
		m.uidMap = NewUIDMap()
		m.agents["scanner"] = &AgentProcess{Name: "scanner", UID: 2001}
		m.agents["fixer"] = &AgentProcess{Name: "fixer"}
		if m.SharedDevUID() {
			t.Fatal("an agent with a per-agent UID must not report shared dev UID")
		}
	})

	t.Run("map loaded, no agents", func(t *testing.T) {
		m := newMgr()
		m.uidMap = NewUIDMap()
		if m.SharedDevUID() {
			t.Fatal("an empty roster with a UID map must not report shared dev UID")
		}
	})
}
