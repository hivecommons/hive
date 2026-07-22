package repair

import (
	"strings"
	"testing"
)

func TestProviderEnvironmentRejectsHostedAndReleaseAuthority(t *testing.T) {
	blocked := map[string]string{
		"HIVE_HOSTED_STATE_KEY":            "state-signing-authority",
		"HIVE_HOSTED_CODEX":                "/controller/provider",
		"HIVE_HOSTED_ARBITRARY":            "controller-only",
		"HIVE_RELEASE_CODEX_AUTH_JSON_B64": "raw-release-auth",
		"OPENAI_API_KEY":                   "model-api-key",
		"GITHUB_TOKEN":                     "github-authority",
		"GH_TOKEN":                         "github-cli-authority",
		"UNRELATED_PRIVATE_KEY_CONTROLLER": "private-key-authority",
	}
	for name, value := range blocked {
		t.Setenv(name, value)
	}
	t.Setenv("HIVE_PROVIDER_SAFE_SENTINEL", "preserved")

	got := map[string]string{}
	for _, pair := range providerEnvironment() {
		name, value, _ := strings.Cut(pair, "=")
		got[strings.ToUpper(name)] = value
	}
	for name := range blocked {
		if _, exists := got[name]; exists {
			t.Fatalf("provider inherited blocked controller authority %s", name)
		}
	}
	if got["HIVE_PROVIDER_SAFE_SENTINEL"] != "preserved" {
		t.Fatalf("provider environment dropped an unrelated safe value: %+v", got)
	}
}
