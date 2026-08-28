package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBobConfigResolveAPIKey covers the resolution order — configured file,
// then the k8s Secret mount default, then the PVC default, then api_key_env,
// then DefaultBobAPIKeyEnv — and confirms that a nil/empty config resolves to
// "" rather than panicking.
func TestBobConfigResolveAPIKey(t *testing.T) {
	tests := []struct {
		name string
		// fileContents is written to a temp file whose path is put in APIKeyFile.
		fileContents string
		// envName/envValue is set for the duration of the case.
		envName  string
		envValue string
		// apiKeyEnv is the configured api_key_env value.
		apiKeyEnv  string
		want       string
		wantSource string
	}{
		{
			name: "unconfigured resolves empty",
			want: "", wantSource: "",
		},
		{
			name:         "key file wins",
			fileContents: "key-from-file",
			apiKeyEnv:    "HIVE_BOB_TEST_KEY",
			envName:      "HIVE_BOB_TEST_KEY",
			envValue:     "key-from-env",
			want:         "key-from-file",
			wantSource:   "file:", // prefix check; the path is a temp dir
		},
		{
			name:      "configured env var used when no file",
			apiKeyEnv: "HIVE_BOB_TEST_KEY",
			envName:   "HIVE_BOB_TEST_KEY",
			envValue:  "key-from-env",
			want:      "key-from-env", wantSource: "env:HIVE_BOB_TEST_KEY",
		},
		{
			name:     "default env var used when nothing configured",
			envName:  DefaultBobAPIKeyEnv,
			envValue: "key-from-default-env",
			want:     "key-from-default-env", wantSource: "env:" + DefaultBobAPIKeyEnv,
		},
		{
			name:         "whitespace is trimmed",
			fileContents: "  key-with-space\n",
			want:         "key-with-space", wantSource: "file:",
		},
		{
			name:         "empty file falls through to env",
			fileContents: "   \n",
			envName:      DefaultBobAPIKeyEnv,
			envValue:     "key-from-default-env",
			want:         "key-from-default-env", wantSource: "env:" + DefaultBobAPIKeyEnv,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Clear the default env var unless the case sets it, so an inherited
			// value in the developer's shell cannot make a case pass or fail.
			t.Setenv(DefaultBobAPIKeyEnv, "")
			if tc.envName != "" {
				t.Setenv(tc.envName, tc.envValue)
			}

			c := &BobConfig{APIKeyEnv: tc.apiKeyEnv}
			if tc.fileContents != "" {
				path := filepath.Join(t.TempDir(), "bob_api_key")
				if err := os.WriteFile(path, []byte(tc.fileContents), 0o600); err != nil {
					t.Fatal(err)
				}
				c.APIKeyFile = path
			}

			if got := c.ResolveAPIKey(); got != tc.want {
				t.Errorf("ResolveAPIKey() = %q, want %q", got, tc.want)
			}

			source := c.ResolveAPIKeySource()
			switch tc.wantSource {
			case "":
				if source != "" {
					t.Errorf("ResolveAPIKeySource() = %q, want empty", source)
				}
			case "file:":
				if len(source) < len("file:") || source[:len("file:")] != "file:" {
					t.Errorf("ResolveAPIKeySource() = %q, want a file: source", source)
				}
			default:
				if source != tc.wantSource {
					t.Errorf("ResolveAPIKeySource() = %q, want %q", source, tc.wantSource)
				}
			}

			// The source must never contain the key value itself — it is logged.
			if tc.want != "" && source != "" && source == tc.want {
				t.Error("ResolveAPIKeySource() must not expose the key value")
			}
		})
	}
}

// TestBobConfigNilReceiver guards the nil path: callers hold *BobConfig and a
// nil receiver must resolve to "" rather than panic.
func TestBobConfigNilReceiver(t *testing.T) {
	var c *BobConfig
	if got := c.ResolveAPIKey(); got != "" {
		t.Errorf("nil BobConfig ResolveAPIKey() = %q, want empty", got)
	}
	if got := c.ResolveAPIKeySource(); got != "" {
		t.Errorf("nil BobConfig ResolveAPIKeySource() = %q, want empty", got)
	}
}

// TestBobConstants pins the externally-defined names. BobAPIKeyEnvVar,
// BobAuthTypeEnvVar and BobAuthTypeAPIKey are dictated by the bobshell bundle,
// not by us: changing them silently breaks headless auth, so they are asserted
// rather than assumed.
func TestBobConstants(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"bob's own env var name", BobAPIKeyEnvVar, "BOBSHELL_API_KEY"},
		{"bob's auth-type env var name", BobAuthTypeEnvVar, "BOBSHELL_DEFAULT_AUTH_TYPE"},
		{"bob's api-key auth type value", BobAuthTypeAPIKey, "api-key"},
		{"bob's hidden auth-method flag", BobAuthMethodFlag, "--auth-method"},
		{"bob's persisted settings path", BobSettingsRelPath, ".bob/settings.json"},
		{"settings security key", BobSettingsSecurityKey, "security"},
		{"settings auth key", BobSettingsAuthKey, "auth"},
		{"settings selectedType key", BobSettingsSelectedTypeKey, "selectedType"},
		{"settings enforcedType key", BobSettingsEnforcedTypeKey, "enforcedType"},
		{"hive-side default env var", DefaultBobAPIKeyEnv, "HIVE_BOB_API_KEY"},
		{"k8s Secret mount default path", DefaultBobAPIKeyFile, "/secrets/bob_api_key"},
		{"PVC default path", WritableBobAPIKeyFile, "/data/secrets/bob_api_key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}
