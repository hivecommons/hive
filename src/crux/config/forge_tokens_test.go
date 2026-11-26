package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForgeCredentialsGovernorResolve(t *testing.T) {
	root := t.TempDir()
	defer AllowSecretFileRoot(root)()
	write := func(name, body string) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	filePath := write("token", "from-file\n")
	emptyPath := write("empty", " \n")
	outside := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FORGE_TEST_DEFAULT", "from-default-env")
	t.Setenv("FORGE_TEST_NAMED", "from-named-env")

	cases := []struct {
		name       string
		tokens     TokenSource
		wantToken  string
		wantSource string
		wantErr    string
	}{
		{"neither set reads the default env", TokenSource{}, "from-default-env", "env:FORGE_TEST_DEFAULT", ""},
		{"named env", TokenSource{TokenEnv: "FORGE_TEST_NAMED"}, "from-named-env", "env:FORGE_TEST_NAMED", ""},
		{"named env that is unset passes an empty token", TokenSource{TokenEnv: "FORGE_TEST_UNSET"}, "", "env:FORGE_TEST_UNSET", ""},
		{"file", TokenSource{TokenFile: filePath}, "from-file", "file:" + filePath, ""},
		{"file wins over env", TokenSource{TokenEnv: "FORGE_TEST_NAMED", TokenFile: filePath}, "from-file", "file:" + filePath, ""},
		{"path outside the secret dirs", TokenSource{TokenEnv: "FORGE_TEST_NAMED", TokenFile: outside},
			"", "", "token_file \"" + outside + "\" is outside the allowed secret dirs"},
		{"relative path", TokenSource{TokenFile: "token"}, "", "", "is outside the allowed secret dirs"},
		{"missing file does not fall back to env", TokenSource{TokenEnv: "FORGE_TEST_NAMED", TokenFile: filepath.Join(root, "missing")},
			"", "", "is unreadable"},
		{"empty file", TokenSource{TokenFile: emptyPath}, "", "", "is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ForgeCredentials{DefaultEnv: "FORGE_TEST_DEFAULT", Tokens: ForgeTokens{TokenSource: tc.tokens}}
			token, source, err := c.Governor().Resolve()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				if token != "" {
					t.Fatalf("token = %q on error, want empty", token)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if token != tc.wantToken || source != tc.wantSource {
				t.Fatalf("got (%q, %q), want (%q, %q)", token, source, tc.wantToken, tc.wantSource)
			}
		})
	}
}

// The strip registry reads its names from EnvNames, so it must list the
// default when the block leaves token_env unset, and keep a configured name.
func TestForgeCredentialsEnvNames(t *testing.T) {
	c := ForgeCredentials{DefaultEnv: "FORGE_DEFAULT"}
	if got := c.EnvNames(); len(got) != 1 || got[0] != "FORGE_DEFAULT" {
		t.Fatalf("default EnvNames = %v", got)
	}
	c.Tokens.TokenEnv = "MY_TOKEN"
	if got := c.EnvNames(); len(got) != 1 || got[0] != "MY_TOKEN" {
		t.Fatalf("configured EnvNames = %v", got)
	}
	c.Tokens.TokenEnv, c.Tokens.TokenFile = "", "/secrets/forge/token"
	if got := c.EnvNames(); len(got) != 1 || got[0] != "FORGE_DEFAULT" {
		t.Fatalf("token_file only: EnvNames = %v, want the default so the strip registry still lists it", got)
	}
}
