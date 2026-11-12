package dashboard

import (
	"reflect"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

func TestAgentStatusSandboxWarningsByRuntime(t *testing.T) {
	config.StoreSandboxGateReport(config.SandboxGateReport{
		OpenShell: []string{"openshell gateway unreachable"},
		Substrate: []string{"substrate operator not found"},
	})
	t.Cleanup(func() { config.StoreSandboxGateReport(config.SandboxGateReport{}) })

	enabled := true
	agent := func(runtime string, sandboxed bool) config.AgentConfig {
		a := config.AgentConfig{Backend: "claude", Enabled: true}
		if sandboxed {
			a.Sandbox = &config.AgentSandboxOverride{Enabled: &enabled, Runtime: runtime}
		}
		return a
	}
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			AgentSandbox: config.AgentSandboxConfig{Enabled: true, Image: "img"},
			Agents: map[string]config.AgentConfig{
				"sub":  agent("substrate", true),
				"osh":  agent("openshell", true),
				"job":  agent("job", true),
				"none": agent("", false),
			},
		},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{}},
	}
	statuses := map[string]*crux.Process{}
	for n, a := range cfg.Agents {
		statuses[n] = &crux.Process{Config: a}
	}
	payload := BuildAgentOnlyStatus(governor.State{Mode: "IDLE"}, statuses, cfg)

	want := map[string][]string{
		"sub":  {"substrate operator not found"},
		"osh":  {"openshell gateway unreachable"},
		"job":  nil,
		"none": nil,
	}
	got := map[string][]string{}
	for _, a := range payload.Agents {
		got[a.Name] = a.SandboxWarnings
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sandbox warnings by agent = %#v, want %#v", got, want)
	}
}
