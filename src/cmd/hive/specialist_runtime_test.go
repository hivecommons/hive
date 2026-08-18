package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/agent"
	"github.com/kubestellar/hive/pkg/integrated"
)

func TestSpecialistAgentConfigsUseOnlyExistingAdvisoryRoles(t *testing.T) {
	configs := specialistAgentConfigs("gpt-specialist")
	want := []string{"quality", "ci-maintainer", "sec-check", "architect", "scanner"}
	if len(configs) != len(want) {
		t.Fatalf("specialist count = %d, want %d", len(configs), len(want))
	}
	for _, name := range want {
		entry, ok := configs[name]
		if !ok {
			t.Errorf("missing existing specialist %s", name)
			continue
		}
		if entry.ID != name || entry.Role != name || entry.Backend != "codex" || entry.Model != "gpt-specialist" || entry.Mode != "ADVISORY" || !entry.Enabled || !entry.OnDemand || !entry.ClearOnKick || !entry.CLIPinned {
			t.Errorf("unsafe specialist config for %s: %+v", name, entry)
		}
		if entry.Tools != nil || len(entry.Connections) != 0 || entry.CavemanMode != "" || entry.LaunchCmd != "" {
			t.Errorf("specialist %s received an expansion surface: %+v", name, entry)
		}
	}
	for _, forbidden := range []string{"supervisor", "guide", "publisher", "visual-hive-agent"} {
		if _, exists := configs[forbidden]; exists {
			t.Errorf("created competing generic role %s", forbidden)
		}
	}
}

func TestIntegratedSpecialistRuntimeOwnsStableRootAndExclusiveLease(t *testing.T) {
	stateDir := t.TempDir()
	durable := testIntegratedSpecialistConfig(t, stateDir)
	first, err := newIntegratedSpecialistRuntime(stateDir, durable)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.Manager == nil {
		t.Fatal("repair-pr config did not create a specialist runtime")
	}
	wantRoot := filepath.Join(stateDir, "integrated", "specialists", "agents")
	if first.WorkDir != wantRoot || first.Manager.SpecialistProviderSHA256() == "" {
		t.Fatalf("runtime identity = root %q provider %q", first.WorkDir, first.Manager.SpecialistProviderSHA256())
	}
	if _, err := newIntegratedSpecialistRuntime(stateDir, durable); !errors.Is(err, errSpecialistRuntimeLeaseHeld) {
		t.Fatalf("second runtime claim = %v, want lease held", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("idempotent close failed: %v", err)
	}
	second, err := newIntegratedSpecialistRuntime(stateDir, durable)
	if err != nil {
		t.Fatalf("lease was not reacquirable after exact cleanup: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(wantRoot, ".provider-seals"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("specialist provider seals were orphaned: %v", entries)
	}
}

func TestIntegratedSpecialistRuntimeFailsClosedOnStateAndConfigDrift(t *testing.T) {
	stateDir := t.TempDir()
	durable := testIntegratedSpecialistConfig(t, stateDir)
	if _, err := newIntegratedSpecialistRuntime(t.TempDir(), durable); err == nil || !strings.Contains(err.Error(), "state path mismatch") {
		t.Fatalf("state mismatch was accepted: %v", err)
	}
	durable.Provider = "other"
	if _, err := newIntegratedSpecialistRuntime(stateDir, durable); err == nil || !strings.Contains(err.Error(), "require the Codex") {
		t.Fatalf("non-Codex provider was accepted: %v", err)
	}
	durable = testIntegratedSpecialistConfig(t, stateDir)
	durable.ProviderArgs = []string{"--profile", "unsafe"}
	if _, err := newIntegratedSpecialistRuntime(stateDir, durable); err == nil || !strings.Contains(err.Error(), "only the Codex --model") {
		t.Fatalf("unsupported provider prefix was accepted: %v", err)
	}
	durable = testIntegratedSpecialistConfig(t, stateDir)
	runtimeState, err := newIntegratedSpecialistRuntime(stateDir, durable)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeState.Close()
	durable.ACMMLevel++
	if err := runtimeState.matches(stateDir, durable); err == nil || !strings.Contains(err.Error(), "configuration changed") {
		t.Fatalf("out-of-band config drift was accepted: %v", err)
	}
}

func TestIntegratedSpecialistRuntimeIsNotCreatedOutsideLocalRepairMode(t *testing.T) {
	stateDir := t.TempDir()
	for _, mutate := range []func(*integrated.Config){
		func(config *integrated.Config) { config.Automation = integrated.AutomationIssues },
		func(config *integrated.Config) { config.ExecutionMode = integrated.ExecutionHosted },
	} {
		durable := testIntegratedSpecialistConfig(t, stateDir)
		mutate(&durable)
		runtimeState, err := newIntegratedSpecialistRuntime(stateDir, durable)
		if err != nil || runtimeState != nil {
			t.Fatalf("non-local-repair config created runtime: runtime=%v err=%v", runtimeState, err)
		}
	}
}

func TestSpecialistModelFromProviderArgsIsNarrowAndDeterministic(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--model", "gpt-5"}, "gpt-5"},
		{[]string{"--model=gpt-5.1"}, "gpt-5.1"},
	} {
		got, err := specialistModelFromProviderArgs(test.args)
		if err != nil || got != test.want {
			t.Fatalf("args %v = %q, %v; want %q", test.args, got, err, test.want)
		}
	}
	for _, args := range [][]string{nil, {"--model"}, {"--model", "a", "--model=b"}, {"--disable", "sandbox"}, {"exec"}} {
		if _, err := specialistModelFromProviderArgs(args); err == nil {
			t.Errorf("unsafe provider args were accepted: %v", args)
		}
	}
}

func TestIntegratedSetupCodexProviderUsesExplicitBoundedHome(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	t.Setenv("CODEX_HOME", codexHome)
	provider := integratedSetupCodexProvider("/opt/hive/codex/bin/codex", []string{"--model=gpt-5"})
	if provider.Command != "/opt/hive/codex/bin/codex" || provider.CodexHome != codexHome ||
		!reflect.DeepEqual(provider.Prefix, []string{"--model=gpt-5"}) {
		t.Fatalf("provider = %#v", provider)
	}
}

func testIntegratedSpecialistConfig(t *testing.T, stateDir string) integrated.Config {
	t.Helper()
	codexHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	providerName := "codex"
	if runtime.GOOS == "windows" {
		providerName = "codex.exe"
	}
	provider := filepath.Join(t.TempDir(), providerName)
	if err := os.WriteFile(provider, []byte("native-placeholder"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return integrated.Config{
		Repository: "example/proof", StateDir: stateDir, ExecutionMode: integrated.ExecutionLocal,
		Automation: integrated.AutomationRepairPR, Provider: "codex", ProviderCommand: provider, ACMMLevel: 6,
		ProviderArgs: []string{"--model=gpt-5.6-sol"},
	}
}

var _ = agent.SpecialistQuality
