package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestConfigHasTokensChecksBothSources pins the two-source contract that
// decides whether an agent stuck on a login prompt can be auto-restarted: a
// valid token in EITHER the Claude credentials file or the Copilot config
// counts, and only the absence of both reads as "no credentials".
//
// Getting this wrong in either direction is costly: a false negative leaves a
// working agent parked on a login prompt, and a false positive restarts it into
// the same prompt forever.
func TestConfigHasTokensChecksBothSources(t *testing.T) {
	// A Claude credentials file with a far-future expiry, so HasValidToken
	// treats it as usable rather than expired.
	const validClaudeCreds = `{"claudeAiOauth":{"accessToken":"sk-ant-test","expiresAt":99999999999999}}`
	// A Copilot config with one token entry, including the // comment line the
	// Copilot CLI writes.
	const copilotWithTokens = "// managed file\n{\n  \"copilotTokens\": {\"user\": \"gho_abc\"}\n}\n"
	const copilotNoTokens = `{"copilotTokens":{}}`

	tests := []struct {
		name string
		// claude and copilot are file contents; an empty string means the file
		// is not created at all.
		claude  string
		copilot string
		want    bool
	}{
		{
			name:   "claude credentials alone are enough",
			claude: validClaudeCreds,
			want:   true,
		},
		{
			name:    "copilot tokens alone are enough",
			copilot: copilotWithTokens,
			want:    true,
		},
		{
			name:    "either source suffices when both are present",
			claude:  validClaudeCreds,
			copilot: copilotWithTokens,
			want:    true,
		},
		{
			name:    "claude valid, copilot empty still counts",
			claude:  validClaudeCreds,
			copilot: copilotNoTokens,
			want:    true,
		},
		{
			name:    "neither source has a token",
			copilot: copilotNoTokens,
			want:    false,
		},
		{
			name: "both files missing",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			claudePath := filepath.Join(dir, "credentials.json")
			copilotPath := filepath.Join(dir, "config.json")

			if tc.claude != "" {
				if err := os.WriteFile(claudePath, []byte(tc.claude), 0o600); err != nil {
					t.Fatalf("write claude creds: %v", err)
				}
			}
			if tc.copilot != "" {
				if err := os.WriteFile(copilotPath, []byte(tc.copilot), 0o600); err != nil {
					t.Fatalf("write copilot config: %v", err)
				}
			}
			withSharedPaths(t, copilotPath, claudePath)

			if got := configHasTokens(); got != tc.want {
				t.Errorf("configHasTokens() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestConfigHasTokensFallsThroughToCopilot is the specific ordering check: an
// unusable Claude credentials file must not short-circuit the result, it must
// fall through to the Copilot config.
func TestConfigHasTokensFallsThroughToCopilot(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "credentials.json")
	copilotPath := filepath.Join(dir, "config.json")

	// Present but unparseable, so HasValidToken cannot succeed.
	if err := os.WriteFile(claudePath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write claude creds: %v", err)
	}
	if err := os.WriteFile(copilotPath,
		[]byte("{\"copilotTokens\": {\"user\": \"gho_abc\"}}"), 0o600); err != nil {
		t.Fatalf("write copilot config: %v", err)
	}
	withSharedPaths(t, copilotPath, claudePath)

	if !configHasTokens() {
		t.Error("configHasTokens() = false, want true: a broken Claude file must fall through to Copilot")
	}
}
