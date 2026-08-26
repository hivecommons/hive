package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguredGatewayBackendLaunchesClaude(t *testing.T) {
	binDir := t.TempDir()
	claudePath := filepath.Join(binDir, "claude")
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	m := &Manager{}
	m.SetGatewayBackendChecker(func(backend string) bool {
		return backend == "openrouter" || backend == "corp-gateway"
	})

	for _, backend := range []string{"openrouter", "corp-gateway"} {
		t.Run(backend, func(t *testing.T) {
			binary, err := m.backendBinary(backend)
			if err != nil {
				t.Fatalf("configured gateway backend %q cannot launch: %v", backend, err)
			}
			if binary != claudePath {
				t.Errorf("backend %q resolved to %q, want claude CLI %q", backend, binary, claudePath)
			}
		})
	}
}

func TestUnconfiguredGatewayBackendStillRejectedAtLaunch(t *testing.T) {
	m := &Manager{}
	m.SetGatewayBackendChecker(func(backend string) bool { return backend == "openrouter" })

	_, err := m.backendBinary("other-gateway")
	if err == nil || !strings.Contains(err.Error(), "unknown backend") {
		t.Fatalf("unconfigured gateway backend error = %v, want unknown backend", err)
	}
}

func TestGatewayMissingBinaryBannerNamesClaude(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	m := &Manager{logger: discardLogger()}
	m.SetGatewayBackendChecker(func(backend string) bool { return backend == "openrouter" })
	agent := &AgentProcess{Name: "guide", BackendOverride: "openrouter"}

	if err := m.launchInTmux(t.Context(), agent); err != nil {
		t.Fatalf("launchInTmux returned error: %v", err)
	}
	if agent.State != StateFailed {
		t.Fatalf("State = %q, want %q", agent.State, StateFailed)
	}
	if !strings.Contains(agent.lastLaunchFailureBanner, "Gateway backends run through the claude CLI") {
		t.Errorf("gateway failure banner does not identify the claude CLI: %q", agent.lastLaunchFailureBanner)
	}
	if strings.Contains(agent.lastLaunchFailureBanner, "The CLI for this backend") {
		t.Errorf("gateway failure banner incorrectly implies a dedicated OpenRouter CLI: %q", agent.lastLaunchFailureBanner)
	}
}
