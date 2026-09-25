package agent

import (
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/claude"
)

// HeadlessCredentialEnv returns the backend credential environment used by
// non-interactive, hub-launched CLIs. It reads the same durable token files as
// Manager startup, without constructing a Manager.
func HeadlessCredentialEnv(backend string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(backend)) {
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
	}
	return nil, nil
}
