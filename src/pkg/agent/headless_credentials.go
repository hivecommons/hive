package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/pkg/claude"
	"github.com/hivecommons/hive/pkg/config"
)

// HeadlessCredentialSources carries the credential inputs a headless launch
// cannot read without a Manager. BobAPIKey mirrors Manager.bobAPIKey and is
// only consulted for the bob backend.
type HeadlessCredentialSources struct {
	BobAPIKey func() string
}

// HeadlessCredentialEnv returns the backend credential environment used by
// non-interactive, hub-launched CLIs. It reads the same durable credential
// sources as Manager startup (agentEnvPairs), without constructing a Manager.
// Backends whose CLI reads its own login state from disk return nil.
func HeadlessCredentialEnv(backend string, src HeadlessCredentialSources) ([]string, error) {
	switch b := strings.ToLower(strings.TrimSpace(backend)); b {
	case "copilot":
		if tok := strings.TrimSpace(os.Getenv(copilotTokenEnvVar)); tok != "" {
			return []string{copilotTokenEnvVar + "=" + tok}, nil
		}
		data, err := os.ReadFile(copilotUserTokenProbePath)
		if err != nil {
			return nil, err
		}
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return []string{copilotTokenEnvVar + "=" + tok}, nil
		}
	case "claude":
		if tok := claude.ReadAccessToken(claude.CredentialsPath); tok != "" {
			return []string{"CLAUDE_CODE_OAUTH_TOKEN=" + tok}, nil
		}
	case codexBackend:
		// The shared codex login lives beside codexSharedAuthFile; per-agent
		// CODEX_HOMEs symlink to it (setupCodexHome).
		return []string{"CODEX_HOME=" + filepath.Dir(codexSharedAuthFile)}, nil
	case bobBackend:
		key := ""
		if src.BobAPIKey != nil {
			key = strings.TrimSpace(src.BobAPIKey())
		}
		if key == "" {
			return nil, fmt.Errorf("bob requires %s for headless operation", config.BobAPIKeyEnvVar)
		}
		return []string{
			config.BobAPIKeyEnvVar + "=" + key,
			config.BobV2APIKeyEnvVar + "=" + key,
			config.BobAuthTypeEnvVar + "=" + config.BobAuthTypeAPIKey,
		}, nil
	default:
		if IsInferenceBackend(b) {
			return nil, fmt.Errorf("backend %s requires hive inference routing, which headless launches do not provide", b)
		}
	}
	return nil, nil
}
