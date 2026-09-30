package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestUIDMapAllocateUIDsAppendsNewNamesAfterMaxExistingUID(t *testing.T) {
	u := NewUIDMap()
	u.Agents["architect"] = 2001
	u.Agents["scanner"] = 2009

	u.AllocateUIDs([]string{"adjudicator", "architect", "scanner"})

	if got := u.LookupByName("architect"); got != 2001 {
		t.Fatalf("architect uid = %d, want 2001", got)
	}
	if got := u.LookupByName("scanner"); got != 2009 {
		t.Fatalf("scanner uid = %d, want 2009", got)
	}
	if got := u.LookupByName("adjudicator"); got != 2010 {
		t.Fatalf("adjudicator uid = %d, want max(existing)+1 = 2010", got)
	}
}

func TestNewManagerPrefersPersistedUIDMapOverRuntimeMap(t *testing.T) {
	dir := t.TempDir()
	oldRuntimePath := UIDMapPath
	oldPersistedPath := PersistedUIDMapPath
	UIDMapPath = filepath.Join(dir, "run", "uid-map.json")
	PersistedUIDMapPath = filepath.Join(dir, "data", ".hive", "uid-map.json")
	t.Cleanup(func() {
		UIDMapPath = oldRuntimePath
		PersistedUIDMapPath = oldPersistedPath
	})

	runtimeMap := NewUIDMap()
	runtimeMap.Agents["scanner"] = 2002
	if err := runtimeMap.Save(UIDMapPath); err != nil {
		t.Fatalf("save runtime uid map: %v", err)
	}
	persistedMap := NewUIDMap()
	persistedMap.Agents["scanner"] = 2009
	if err := persistedMap.Save(PersistedUIDMapPath); err != nil {
		t.Fatalf("save persisted uid map: %v", err)
	}

	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "copilot"}}, slog.New(slog.NewTextHandler(os.Stderr, nil)), ProjectContext{})
	m.mu.RLock()
	got := m.agents["scanner"].UID
	m.mu.RUnlock()
	if got != 2009 {
		t.Fatalf("scanner uid = %d, want persisted uid 2009", got)
	}
}
