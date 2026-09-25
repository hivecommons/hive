package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHeadlessCredentialEnvCopilotReadsDurableToken(t *testing.T) {
	t.Setenv(copilotTokenEnvVar, "")
	old := copilotUserTokenProbePath
	copilotUserTokenProbePath = filepath.Join(t.TempDir(), "copilot-token")
	t.Cleanup(func() { copilotUserTokenProbePath = old })
	if err := os.WriteFile(copilotUserTokenProbePath, []byte(" cp_tok \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := HeadlessCredentialEnv("copilot")
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env[0] != copilotTokenEnvVar+"=cp_tok" {
		t.Fatalf("env = %v", env)
	}
}

func TestHeadlessCredentialEnvCopilotPrefersProcessEnv(t *testing.T) {
	t.Setenv(copilotTokenEnvVar, "from-env")
	env, err := HeadlessCredentialEnv("copilot")
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env[0] != copilotTokenEnvVar+"=from-env" {
		t.Fatalf("env = %v", env)
	}
}
