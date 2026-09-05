package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

func LaunchCmdDeclaredBackend(cmd string) string {
	fields := strings.Fields(strings.TrimSpace(cmd))
	if len(fields) == 0 {
		return ""
	}
	i := 0
	for i < len(fields) && strings.Contains(fields[i], "=") && !strings.HasPrefix(fields[i], "-") {
		i++
	}
	if i >= len(fields) {
		return ""
	}
	bin := filepath.Base(fields[i])
	if bin == "agent-launch.sh" {
		for j := i + 1; j < len(fields); j++ {
			if fields[j] == "--backend" && j+1 < len(fields) {
				return fields[j+1]
			}
		}
		return ""
	}
	if IsCLIBackend(bin) {
		return bin
	}
	return ""
}

func (g GovernorConfig) ValidateLaunchCmdBackend(backend, launchCmd string) error {
	declared := LaunchCmdDeclaredBackend(launchCmd)
	if declared == "" || backend == "" {
		return nil
	}
	if strings.EqualFold(declared, backend) {
		return nil
	}
	if IsCLIBackend(backend) {
		return fmt.Errorf("backend %q contradicts launch_cmd %q (it launches %q): the agent would be launched as %s but health-checked and diagnosed as %s, then relaunched as \"hung\" forever — set backend to %s or fix launch_cmd",
			backend, launchCmd, declared, declared, backend, declared)
	}
	if IsInferenceBackend(backend) || g.isGatewayName(backend) {
		if declared == "claude" {
			return nil
		}
		return fmt.Errorf("backend %q routes through the claude CLI but launch_cmd %q launches %q — clear launch_cmd or point it at claude",
			backend, launchCmd, declared)
	}
	return nil
}

func (g GovernorConfig) isGatewayName(backend string) bool {
	for _, gw := range g.ResolvedGateways() {
		if gw.Name != "" && strings.EqualFold(gw.Name, backend) {
			return true
		}
	}
	return false
}
