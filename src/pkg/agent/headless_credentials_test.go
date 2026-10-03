package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestHeadlessCredentialEnvCopilotReadsDurableToken(t *testing.T) {
	t.Setenv(copilotTokenEnvVar, "")
	old := copilotUserTokenProbePath
	copilotUserTokenProbePath = filepath.Join(t.TempDir(), "copilot-token")
	t.Cleanup(func() { copilotUserTokenProbePath = old })
	if err := os.WriteFile(copilotUserTokenProbePath, []byte(" cp_tok \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := HeadlessCredentialEnv("copilot", HeadlessCredentialSources{})
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env[0] != copilotTokenEnvVar+"=cp_tok" {
		t.Fatalf("env = %v", env)
	}
}

func TestHeadlessCredentialEnvCopilotPrefersProcessEnv(t *testing.T) {
	t.Setenv(copilotTokenEnvVar, "from-env")
	env, err := HeadlessCredentialEnv("copilot", HeadlessCredentialSources{})
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env[0] != copilotTokenEnvVar+"=from-env" {
		t.Fatalf("env = %v", env)
	}
}

func TestHeadlessCredentialEnvCopilotMissingTokenErrors(t *testing.T) {
	t.Setenv(copilotTokenEnvVar, "")
	old := copilotUserTokenProbePath
	copilotUserTokenProbePath = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { copilotUserTokenProbePath = old })
	if _, err := HeadlessCredentialEnv("copilot", HeadlessCredentialSources{}); err == nil {
		t.Fatal("missing copilot token did not error")
	}
	if err := os.WriteFile(copilotUserTokenProbePath, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := HeadlessCredentialEnv("copilot", HeadlessCredentialSources{})
	if err != nil || env != nil {
		t.Fatalf("blank token file: env=%v err=%v", env, err)
	}
}

func TestHeadlessCredentialEnvCodexUsesSharedLogin(t *testing.T) {
	env, err := HeadlessCredentialEnv(" Codex ", HeadlessCredentialSources{})
	if err != nil {
		t.Fatal(err)
	}
	want := "CODEX_HOME=" + filepath.Dir(codexSharedAuthFile)
	if len(env) != 1 || env[0] != want {
		t.Fatalf("env = %v, want [%s]", env, want)
	}
}

func TestHeadlessCredentialEnvBobKey(t *testing.T) {
	if _, err := HeadlessCredentialEnv("bob", HeadlessCredentialSources{}); err == nil {
		t.Fatal("bob without a key resolver did not error")
	}
	if _, err := HeadlessCredentialEnv("bob", HeadlessCredentialSources{BobAPIKey: func() string { return " " }}); err == nil {
		t.Fatal("bob with an empty key did not error")
	}
	env, err := HeadlessCredentialEnv("bob", HeadlessCredentialSources{BobAPIKey: func() string { return " bob-key\n" }})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		config.BobAPIKeyEnvVar + "=bob-key",
		config.BobV2APIKeyEnvVar + "=bob-key",
		config.BobAuthTypeEnvVar + "=" + config.BobAuthTypeAPIKey,
	} {
		if !slices.Contains(env, want) {
			t.Errorf("env %v missing %s", env, want)
		}
	}
}

func TestHeadlessCredentialEnvInferenceAndOtherBackends(t *testing.T) {
	for _, backend := range config.InferenceBackends {
		if _, err := HeadlessCredentialEnv(backend, HeadlessCredentialSources{}); err == nil {
			t.Errorf("inference backend %q did not error", backend)
		}
	}
	for _, backend := range []string{"gemini", "aider", "opencode"} {
		env, err := HeadlessCredentialEnv(backend, HeadlessCredentialSources{})
		if err != nil || env != nil {
			t.Errorf("%s: env=%v err=%v, want nil", backend, env, err)
		}
	}
}
