package agent

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

func ompAgent(name string, uid int, backend, override string) *AgentProcess {
	return &AgentProcess{Name: name, UID: uid, BackendOverride: override, Config: config.AgentConfig{Backend: backend}}
}

func TestOmpAgentHomesNilManager(t *testing.T) {
	var m *Manager
	if got := m.OmpAgentHomes(); got != nil {
		t.Errorf("nil manager: got %v, want nil", got)
	}
}

func TestOmpAgentHomesNoAgents(t *testing.T) {
	withTestAgentHomes(t)
	m := testManager(4)
	got := m.OmpAgentHomes()
	if got == nil || len(got) != 0 {
		t.Errorf("got %#v, want empty non-nil slice", got)
	}
}

func TestOmpAgentHomesFiltersAndSorts(t *testing.T) {
	root := withTestAgentHomes(t)
	t.Setenv("HIVE_SHARED_AGENT_HOME", "")
	m := testManager(4)
	m.agents["zeta"] = ompAgent("zeta", 2003, "omp", "")
	m.agents["alpha"] = ompAgent("alpha", 2001, "omp", "")
	m.agents["viaoverride"] = ompAgent("viaoverride", 2002, "claude", "omp")
	m.agents["overriddenaway"] = ompAgent("overriddenaway", 2004, "omp", "claude")
	m.agents["claudeonly"] = claudeAgent("claudeonly", 2005)
	m.agents["nilproc"] = nil

	want := []string{
		filepath.Join(root, "agents", "alpha"),
		filepath.Join(root, "agents", "viaoverride"),
		filepath.Join(root, "agents", "zeta"),
	}
	if got := m.OmpAgentHomes(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestOmpAgentHomesDeduplicatesSharedHome(t *testing.T) {
	withTestAgentHomes(t)
	t.Setenv("HOME", "/shared/omp-home")
	m := testManager(4)
	m.agents["a"] = ompAgent("a", 0, "omp", "")
	m.agents["b"] = ompAgent("b", 0, "omp", "")

	got := m.OmpAgentHomes()
	if len(got) != 1 || got[0] != "/shared/omp-home" {
		t.Errorf("got %v, want a single deduplicated shared home", got)
	}
}
