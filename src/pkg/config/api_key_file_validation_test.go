package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func keyFileTestRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(SetSecretFileRootsForTest(root))
	return root
}

// TestAPIKeyFileWarnings covers hive#18533 boot validation: every litellm and
// gateway api_key_file outside the allowed dirs yields one message naming the
// field, the path, the allowed dirs and the mount remedy.
func TestAPIKeyFileWarnings(t *testing.T) {
	root := keyFileTestRoot(t)
	g := GovernorConfig{}
	g.LiteLLM.APIKeyFile = "/secrets-cb/litellm_api_key"
	g.Gateways = []GatewayConfig{
		{Name: "ok", APIKeyFile: filepath.Join(root, "gw")},
		{Name: "corp", APIKeyFile: "/etc/corp/key"},
		{Name: "envonly", APIKeyEnv: "X"},
	}
	warnings := g.APIKeyFileWarnings()
	if len(warnings) != 2 {
		t.Fatalf("warnings = %q, want 2", warnings)
	}
	if !strings.Contains(warnings[0], "governor.litellm.api_key_file") || !strings.Contains(warnings[0], "/secrets-cb/litellm_api_key") {
		t.Fatalf("litellm warning = %q", warnings[0])
	}
	if !strings.Contains(warnings[1], "governor.gateways[corp].api_key_file") || !strings.Contains(warnings[1], root) {
		t.Fatalf("gateway warning = %q", warnings[1])
	}
	if got := (GovernorConfig{}).APIKeyFileWarnings(); len(got) != 0 {
		t.Fatalf("empty config warned: %q", got)
	}
}

// TestValidateWarnsOnOutsideAPIKeyFile: boot validation WARNS, it does not
// fail, on an outside path (see APIKeyFileWarnings for why).
func TestValidateWarnsOnOutsideAPIKeyFile(t *testing.T) {
	keyFileTestRoot(t)
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	cfg := validatableConfig(map[string]AgentConfig{})
	cfg.Governor.LiteLLM.APIKeyFile = "/secrets-cb/litellm_api_key"
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err != nil {
		t.Fatalf("an outside api_key_file must warn, not fail boot: %v", err)
	}
	if !strings.Contains(buf.String(), "WARNING: governor.litellm.api_key_file \"/secrets-cb/litellm_api_key\" is outside the allowed secret dirs") {
		t.Fatalf("boot warning missing:\n%s", buf.String())
	}
}

func TestResolveLiteLLMInferenceKeyWithSource(t *testing.T) {
	root := keyFileTestRoot(t)
	gwKey := filepath.Join(root, "gw_key")
	if err := os.WriteFile(gwKey, []byte("gw-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(DefaultLiteLLMAPIKeyEnv, "legacy-env-key")

	legacy := GovernorConfig{}
	if key, src := legacy.ResolveLiteLLMInferenceKeyWithSource("litellm"); key != "legacy-env-key" || src != "env:"+DefaultLiteLLMAPIKeyEnv {
		t.Fatalf("legacy = (%q, %q)", key, src)
	}
	if legacy.ResolveLiteLLMInferenceKey("litellm") != "legacy-env-key" {
		t.Fatal("ResolveLiteLLMInferenceKey disagrees with its WithSource twin")
	}

	withGateway := GovernorConfig{}
	withGateway.Gateways = []GatewayConfig{{Name: "litellm", Kind: GatewayKindLiteLLM, APIKeyFile: gwKey}}
	if key, src := withGateway.ResolveLiteLLMInferenceKeyWithSource("litellm"); key != "gw-key" || src != "file:"+gwKey {
		t.Fatalf("gateway = (%q, %q)", key, src)
	}
}
