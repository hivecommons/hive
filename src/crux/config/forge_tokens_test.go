package config

import "testing"

func TestForgeCredentialsGovernorResolve(t *testing.T) {
	t.Setenv("FORGE_TEST_DEFAULT", "from-default-env")
	t.Setenv("FORGE_TEST_NAMED", "from-named-env")

	cases := []struct {
		name       string
		tokenEnv   string
		wantToken  string
		wantSource string
	}{
		{"unset token_env reads the default env", "", "from-default-env", "env:FORGE_TEST_DEFAULT"},
		{"named env", "FORGE_TEST_NAMED", "from-named-env", "env:FORGE_TEST_NAMED"},
		{"named env that is unset passes an empty token", "FORGE_TEST_UNSET", "", "env:FORGE_TEST_UNSET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ForgeCredentials{DefaultEnv: "FORGE_TEST_DEFAULT", Tokens: ForgeTokens{TokenSource: TokenSource{TokenEnv: tc.tokenEnv}}}
			token, source := c.Governor().Resolve()
			if token != tc.wantToken || source != tc.wantSource {
				t.Fatalf("got (%q, %q), want (%q, %q)", token, source, tc.wantToken, tc.wantSource)
			}
		})
	}
}

// The strip registry reads its names from EnvNames, so it must list the
// default when the block leaves token_env unset and the configured name otherwise.
func TestForgeCredentialsEnvNames(t *testing.T) {
	c := ForgeCredentials{DefaultEnv: "FORGE_DEFAULT"}
	if got := c.EnvNames(); len(got) != 1 || got[0] != "FORGE_DEFAULT" {
		t.Fatalf("default EnvNames = %v", got)
	}
	c.Tokens.TokenEnv = "MY_TOKEN"
	if got := c.EnvNames(); len(got) != 1 || got[0] != "MY_TOKEN" {
		t.Fatalf("configured EnvNames = %v", got)
	}
}
