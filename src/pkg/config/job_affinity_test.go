package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"gopkg.in/yaml.v3"
)

func testAffinity() *SandboxJobAffinity {
	return &SandboxJobAffinity{NodeAffinity: &SandboxJobNodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &SandboxJobNodeSelector{NodeSelectorTerms: []SandboxJobNodeSelectorTerm{{MatchExpressions: []SandboxJobNodeSelectorRequirement{
			{Key: "spyre.node-classification", Operator: "In", Values: []string{"all"}},
			{Key: "kubernetes.io/hostname", Operator: "NotIn", Values: []string{"bad-node"}},
		}}}},
		PreferredDuringSchedulingIgnoredDuringExecution: []SandboxJobPreferredSchedulingTerm{{Weight: 50, Preference: SandboxJobNodeSelectorTerm{MatchExpressions: []SandboxJobNodeSelectorRequirement{{Key: "pool", Operator: "Exists"}}}}},
	}}
}

func TestJobAffinityValidation(t *testing.T) {
	if err := cruxconfig.ValidateSandboxJobAffinity(nil); err != nil {
		t.Fatal(err)
	}
	if err := cruxconfig.ValidateSandboxJobAffinity(testAffinity()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*SandboxJobAffinity)
		field  string
	}{
		{"missing node affinity", func(a *SandboxJobAffinity) { a.NodeAffinity = nil }, "nodeAffinity"},
		{"empty required", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = nil
		}, "nodeSelectorTerms"},
		{"too many terms", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = make([]SandboxJobNodeSelectorTerm, cruxconfig.MaxJobAffinityTerms+1)
		}, "nodeSelectorTerms"},
		{"empty expressions", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions = nil
		}, "matchExpressions"},
		{"too many expressions", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions = make([]SandboxJobNodeSelectorRequirement, cruxconfig.MaxJobAffinityTerms+1)
		}, "matchExpressions"},
		{"missing key", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Key = ""
		}, ".key"},
		{"bad operator", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Operator = "Equals"
		}, ".operator"},
		{"empty in", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values = nil
		}, ".values"},
		{"exists values", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Operator = "Exists"
		}, ".values"},
		{"exists explicit empty values", func(a *SandboxJobAffinity) {
			expr := &a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
			expr.Operator = "Exists"
			expr.Values = []string{}
		}, ".values"},
		{"does not exist explicit empty values", func(a *SandboxJobAffinity) {
			expr := &a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
			expr.Operator = "DoesNotExist"
			expr.Values = []string{}
		}, ".values"},
		{"too many values", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values = make([]string, cruxconfig.MaxJobAffinityValues+1)
		}, ".values"},
		{"bad integer", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Operator = "Gt"
		}, ".values"},
		{"bad weight", func(a *SandboxJobAffinity) {
			a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].Weight = 101
		}, ".weight"},
		{"too many preferred", func(a *SandboxJobAffinity) {
			a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = make([]SandboxJobPreferredSchedulingTerm, cruxconfig.MaxJobAffinityTerms+1)
		}, "preferredDuringSchedulingIgnoredDuringExecution"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testAffinity()
			tc.change(a)
			if err := cruxconfig.ValidateSandboxJobAffinity(a); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error = %v, want %s", err, tc.field)
			}
		})
	}
	for _, op := range []string{"In", "NotIn", "Exists", "DoesNotExist", "Gt", "Lt"} {
		a := testAffinity()
		expr := &a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
		expr.Operator = op
		switch op {
		case "Exists", "DoesNotExist":
			expr.Values = nil
		case "Gt", "Lt":
			expr.Values = []string{"42"}
		}
		if err := cruxconfig.ValidateSandboxJobAffinity(a); err != nil {
			t.Errorf("%s: %v", op, err)
		}
	}
}

func TestJobAffinityMergeAndSerialization(t *testing.T) {
	globalAffinity := testAffinity()
	global := AgentSandboxConfig{Job: &SandboxJobConfig{Affinity: globalAffinity}}
	var agent *AgentConfig
	if agent.SandboxJob(global).Affinity != globalAffinity {
		t.Fatal("global affinity not inherited")
	}
	agent = &AgentConfig{Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{}}}
	if agent.SandboxJob(global).Affinity != nil {
		t.Fatal("per-agent job must replace global pod fields wholesale")
	}
	agent.Sandbox.Job.Affinity = testAffinity()
	if agent.SandboxJob(global).Affinity != agent.Sandbox.Job.Affinity {
		t.Fatal("agent affinity must win")
	}
	for _, codec := range []struct {
		name      string
		marshal   func(any) ([]byte, error)
		unmarshal func([]byte, any) error
	}{
		{"yaml", yaml.Marshal, yaml.Unmarshal},
		{"json", json.Marshal, json.Unmarshal},
	} {
		t.Run(codec.name, func(t *testing.T) {
			data, err := codec.marshal(agent.Sandbox.Job)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"nodeAffinity", "nodeSelectorTerms", "matchExpressions", "preferredDuringSchedulingIgnoredDuringExecution"} {
				if !strings.Contains(string(data), key) {
					t.Errorf("missing %s in %s", key, data)
				}
			}
			var got SandboxJobConfig
			if err := codec.unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Affinity, agent.Sandbox.Job.Affinity) {
				t.Fatalf("round trip = %#v", got.Affinity)
			}
		})
	}
}

func TestJobAffinityLoadValidationAndWarning(t *testing.T) {
	bad := testAffinity()
	bad.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Operator = "bad"
	cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{AgentSandbox: AgentSandboxConfig{Job: &SandboxJobConfig{Affinity: bad}}}}
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "agent_sandbox.job.affinity.nodeAffinity") {
		t.Fatalf("global error = %v", err)
	}
	cfg.AgentSandbox.Job = nil
	cfg.Agents = map[string]AgentConfig{"quality": {Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{Affinity: bad}}}}
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "agents.quality.sandbox.job.affinity.nodeAffinity") {
		t.Fatalf("agent error = %v", err)
	}
	yes := true
	cfg.AgentSandbox = AgentSandboxConfig{Enabled: true, Runtime: SandboxRuntimeJob, Image: "image", Job: &SandboxJobConfig{WorkspaceClaim: "data", NodeSelector: map[string]string{"kubernetes.io/hostname": "host"}}}
	cfg.Agents = map[string]AgentConfig{"quality": {Sandbox: &AgentSandboxOverride{Enabled: &yes}}}
	if warnings := AgentSandboxGateWarnings(cfg); len(warnings) != 1 || !strings.Contains(warnings[0], "Pending") || !strings.Contains(warnings[0], "kubernetes.io/hostname") {
		t.Fatalf("warnings = %v", warnings)
	}
	cfg.Agents["quality"] = AgentConfig{Sandbox: &AgentSandboxOverride{Enabled: &yes, Job: &SandboxJobConfig{WorkspaceClaim: "data"}}}
	if warnings := AgentSandboxGateWarnings(cfg); len(warnings) != 0 {
		t.Fatalf("overridden selector warned: %v", warnings)
	}
}

func TestJobAffinitySaveReload(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hive.yaml")
	oldOverlay, oldRuntime := DashboardOverlayFile, RuntimeConfigFile
	DashboardOverlayFile, RuntimeConfigFile = filepath.Join(dir, "dashboard"), filepath.Join(dir, "runtime")
	t.Cleanup(func() { DashboardOverlayFile, RuntimeConfigFile = oldOverlay, oldRuntime })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	seed := "project:\n  org: testorg\ngithub:\n  token: ghp_test123456789\nagents:\n  quality:\n    backend: copilot\n    enabled: true\n    kick_template: quality-advisory.md\n    mode: ADVISORY\n"
	if err := os.WriteFile(source, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(source)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AgentSandbox.Job = &SandboxJobConfig{Affinity: testAffinity()}
	a := cfg.Agents["quality"]
	a.Sandbox = &AgentSandboxOverride{Job: &SandboxJobConfig{Affinity: testAffinity()}}
	cfg.Agents["quality"] = a
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadWithDashboardOverlay(source)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentSandbox.Job == nil || !reflect.DeepEqual(got.AgentSandbox.Job.Affinity, testAffinity()) {
		t.Fatalf("global affinity lost: %+v", got.AgentSandbox.Job)
	}
	if got.Agents["quality"].Sandbox == nil || got.Agents["quality"].Sandbox.Job == nil || !reflect.DeepEqual(got.Agents["quality"].Sandbox.Job.Affinity, testAffinity()) {
		t.Fatal("agent affinity lost")
	}
}

func TestJobAffinityRejectedOnLoad(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hive.yaml")
	seed := "project:\n  org: testorg\ngithub:\n  token: ghp_test123456789\nagents:\n  quality:\n    backend: copilot\n    enabled: true\n    kick_template: quality-advisory.md\n    mode: ADVISORY\n"
	invalid := "agent_sandbox:\n  job:\n    affinity:\n      nodeAffinity:\n        requiredDuringSchedulingIgnoredDuringExecution:\n          nodeSelectorTerms:\n          - matchExpressions:\n            - key: pool\n              operator: Exists\n              values: [bad]\n"
	if err := os.WriteFile(source, []byte(seed+invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithOverrides(source, "-"); err == nil || !strings.Contains(err.Error(), "agent_sandbox.job.affinity") || !strings.Contains(err.Error(), ".values") {
		t.Fatalf("load error = %v", err)
	}
}
