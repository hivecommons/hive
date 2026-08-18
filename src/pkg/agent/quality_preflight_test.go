package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

func newQualityProbeTestManager(t *testing.T, model string) *Manager {
	t.Helper()
	workRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workRoot, "quality"), 0o755); err != nil {
		t.Fatal(err)
	}
	uidMap := NewUIDMap()
	uidMap.Agents["quality"] = 2006
	return newManagerWithUIDMap(map[string]config.AgentConfig{
		"quality": {Backend: codexBackend, Model: model, BeadsDir: filepath.Join(workRoot, "beads")},
	}, discardLogger(), ProjectContext{ACMMLevel: 4}, workRoot, "", "", uidMap)
}

func withQualityProbeFakes(t *testing.T, execute func(context.Context, string, []string, string, []string) ([]byte, []byte, error)) {
	t.Helper()
	originalResolve, originalRandom, originalHash, originalExecute := qualityProbeResolveBackend, qualityProbeRandomRead, qualityProbeHashCommand, qualityProbeExecute
	command := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(command, []byte("test-codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	qualityProbeResolveBackend = func(string) (string, error) { return command, nil }
	qualityProbeRandomRead = func(value []byte) (int, error) {
		for index := range value {
			value[index] = byte(index + 1)
		}
		return len(value), nil
	}
	qualityProbeHashCommand = hashOrdinaryFile
	qualityProbeExecute = execute
	t.Cleanup(func() {
		qualityProbeResolveBackend, qualityProbeRandomRead, qualityProbeHashCommand, qualityProbeExecute = originalResolve, originalRandom, originalHash, originalExecute
	})
}

func qualityProbeFixtureOutput(directory string) ([]byte, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hive-quality-preflight-") {
			data, readErr := os.ReadFile(filepath.Join(directory, entry.Name()))
			return []byte(fmt.Sprintf(`{"tool":"shell","path":%q,"output":%q}`, entry.Name(), strings.TrimSpace(string(data)))), readErr
		}
	}
	return nil, errors.New("nonce file not found")
}

func TestQualityRuntimeProbeUsesExactUnattendedCredentialFreeRuntime(t *testing.T) {
	for _, name := range append(ordinaryAgentControlPlaneCredentialNames(), "HIVE_AGENT_TOKEN_CACHE") {
		t.Setenv(name, "must-not-leak")
	}
	withQualityProbeFakes(t, func(_ context.Context, launcher string, args []string, directory string, environment []string) ([]byte, []byte, error) {
		if launcher != "su-exec" || len(args) < 2 || args[0] != "2006" || !containsString(args, "never") || !containsString(args, "read-only") {
			return nil, nil, fmt.Errorf("unexpected launch: %s %v", launcher, args)
		}
		if environmentValue(environment, "HOME") != "/data/home" || environmentValue(environment, "CODEX_HOME") != "/data/home/.codex-quality" {
			return nil, nil, fmt.Errorf("wrong HOME binding")
		}
		for _, name := range append(ordinaryAgentControlPlaneCredentialNames(), "HIVE_AGENT_TOKEN_CACHE") {
			if environmentValue(environment, name) != "" {
				return nil, nil, fmt.Errorf("credential %s leaked", name)
			}
		}
		output, err := qualityProbeFixtureOutput(directory)
		return output, nil, err
	})
	result, err := newQualityProbeTestManager(t, "gpt-test").ProbeQualityRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Agent != "quality" || result.UID != 2006 || result.Model != "gpt-test" || result.ApprovalPolicy != "never" || result.OutputSHA256 == "" {
		t.Fatalf("unexpected Quality probe result: %+v", result)
	}
}

func TestQualityRuntimeProbeRejectsUnsupportedRuntimeToolFailureAndApproval(t *testing.T) {
	if _, err := newQualityProbeTestManager(t, "").ProbeQualityRuntime(context.Background()); err == nil || !strings.Contains(err.Error(), "exact model") {
		t.Fatalf("empty model was accepted: %v", err)
	}
	t.Run("tool failure", func(t *testing.T) {
		withQualityProbeFakes(t, func(context.Context, string, []string, string, []string) ([]byte, []byte, error) {
			return nil, []byte("model not supported"), errors.New("exit 1")
		})
		if _, err := newQualityProbeTestManager(t, "missing-model").ProbeQualityRuntime(context.Background()); err == nil || !strings.Contains(err.Error(), "model not supported") {
			t.Fatalf("tool failure was accepted: %v", err)
		}
	})
	t.Run("interactive approval", func(t *testing.T) {
		withQualityProbeFakes(t, func(_ context.Context, _ string, _ []string, directory string, _ []string) ([]byte, []byte, error) {
			output, err := qualityProbeFixtureOutput(directory)
			return output, []byte("approval required"), err
		})
		if _, err := newQualityProbeTestManager(t, "gpt-test").ProbeQualityRuntime(context.Background()); err == nil || !strings.Contains(err.Error(), "interactive approval") {
			t.Fatalf("interactive approval was accepted: %v", err)
		}
	})
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
