package sandbox

import "testing"

// IsCredentialName is the exported form of the rule every launcher applies;
// pkg/kubejob relies on it matching isCredentialName exactly.
func TestIsCredentialNameMatchesInternalRule(t *testing.T) {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN", "MY_SECRET", "SOME_TOKEN_X", "GIT_ASKPASS", "HIVE_CLINE_PROVIDERS_B64", "PATH", "HOME", "ANTHROPIC_MODEL"} {
		if IsCredentialName(name) != isCredentialName(name) {
			t.Errorf("IsCredentialName(%q) diverged from isCredentialName", name)
		}
	}
	if !IsCredentialName("GITHUB_TOKEN") || !IsCredentialName("HIVE_CLINE_PROVIDERS_B64") || IsCredentialName("PATH") {
		t.Error("IsCredentialName must reject GitHub tokens and Cline provider material, and allow PATH")
	}
}

func TestContextWindowIsNotCredential(t *testing.T) {
	const name = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"
	if IsCredentialName(name) || !IsCredentialName(name+"_SECRET") || !IsCredentialName("OTHER_TOKEN") {
		t.Fatal("only the exact context-window name must be exempt from credential filtering")
	}
	env := SanitizedEnv(nil, nil, map[string]string{name: "262144", "OTHER_TOKEN": "secret"})
	if len(env) != 1 || env[0] != name+"=262144" {
		t.Fatalf("sanitized environment = %v", env)
	}
}

func TestClineProviderMaterialNeverEntersPodmanEnvOrArgv(t *testing.T) {
	const key = "HIVE_CLINE_PROVIDERS_B64"
	explicit := map[string]string{key: "must-not-copy"}
	for _, entry := range SanitizedEnv([]string{key + "=must-not-copy"}, []string{key}, explicit) {
		if entry == key+"=must-not-copy" {
			t.Fatal("provider material bypassed the credential deny-list")
		}
	}
	args, err := PodmanArgs(LaunchSpec{Image: "image", Workspace: "/workspace", Env: explicit})
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range args {
		if arg == key {
			t.Fatal("Podman may not inherit provider transport even by name")
		}
	}
}
