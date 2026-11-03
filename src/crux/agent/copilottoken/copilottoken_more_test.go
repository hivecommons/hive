package copilottoken

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTokenValueUsable(t *testing.T) {
	cases := map[string]bool{
		"":           false,
		"   ":        false,
		"******":     false,
		"  ****  ":   false,
		"gho_abc":    true,
		" gho_abc ":  true,
		"*gho_abc*":  true,
		"********1*": true,
	}
	for in, want := range cases {
		if got := TokenValueUsable(in); got != want {
			t.Errorf("TokenValueUsable(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTokenFromValue_UnknownType(t *testing.T) {
	if got := TokenFromValue(42); got != "" {
		t.Errorf("non-string/object value should yield \"\", got %q", got)
	}
	if got := TokenFromValue(map[string]interface{}{"token": 7}); got != "" {
		t.Errorf("non-string token field should yield \"\", got %q", got)
	}
}

func TestCredentialFileHasTokens(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct {
		name string
		file string
		body string
		want bool
	}{
		{"missing file", "", "", false},
		{"malformed json", "config.json", "{not json", false},
		{"copilotTokens not a map", "config.json", `{"copilotTokens":"gho_x"}`, false},
		{"empty copilotTokens", "config.json", `{"copilotTokens":{}}`, false},
		{"string token with comment header", "config.json", ConfigHeader + `{"copilotTokens":{"https://github.com:me":"gho_x"}}`, true},
		{"object token", "config.json", `{"copilotTokens":{"github.com":{"token":"gho_x"}}}`, true},
		{"masked string token", "config.json", `{"copilotTokens":{"github.com":"******"}}`, false},
		{"masked object token", "config.json", `{"copilotTokens":{"github.com":{"token":"****"}}}`, false},
		{"unexpected value type", "config.json", `{"copilotTokens":{"github.com":17}}`, false},
		{"config.json without copilotTokens", "config.json", `{"github.com":{"oauth_token":"gho_x"}}`, false},
		{"apps.json with oauth_token", "apps.json", `{"github.com:Iv1.abc":{"oauth_token":"gho_x","user":"me"}}`, true},
		{"hosts.json with oauth_token", "hosts.json", `{"github.com":{"oauth_token":"gho_x"}}`, true},
		{"hosts.json empty oauth_token", "hosts.json", `{"github.com":{"oauth_token":""}}`, false},
		{"hosts.json non-object entry", "hosts.json", `{"github.com":"gho_x"}`, false},
		{"hosts.json empty map", "hosts.json", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "does-not-exist.json")
			if tc.file != "" {
				path = write(tc.file, tc.body)
			}
			if got := CredentialFileHasTokens(path); got != tc.want {
				t.Errorf("CredentialFileHasTokens = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfigHasTokens(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "credentials.json")
	copilotPath := filepath.Join(dir, "config.json")

	// Neither file exists → false.
	if ConfigHasTokens(claudePath, copilotPath) {
		t.Error("no files should report no tokens")
	}

	// Only a usable Copilot token → true.
	if err := os.WriteFile(copilotPath, []byte(`{"copilotTokens":{"github.com":{"token":"gho_x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ConfigHasTokens(claudePath, copilotPath) {
		t.Error("copilot token alone should count")
	}

	// Only a usable (non-expiring) Claude token → true.
	if err := os.Remove(copilotPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudePath, []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-x","expiresAt":0,"scopes":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ConfigHasTokens(claudePath, copilotPath) {
		t.Error("claude token alone should count")
	}

	// Claude access token expired with no refresh grant and no Copilot token → false.
	if err := os.WriteFile(claudePath, []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-x","expiresAt":1,"scopes":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ConfigHasTokens(claudePath, copilotPath) {
		t.Error("expired claude token without refresh grant should not count")
	}
}

func TestReadConfig_Errors(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadConfig(filepath.Join(dir, "missing.json")); !os.IsNotExist(err) {
		t.Errorf("missing file should return IsNotExist error, got %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(bad); err == nil {
		t.Error("malformed json should error")
	}
	null := filepath.Join(dir, "null.json")
	if err := os.WriteFile(null, []byte(ConfigHeader+"null"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(null)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Error("JSON null should yield an empty, non-nil map")
	}
}

func TestWriteConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := WriteConfig(path, map[string]interface{}{"a": "b"}, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != ConfigHeader+"{\n  \"a\": \"b\"\n}" {
		t.Errorf("unexpected file body:\n%s", data)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file should be renamed away")
	}

	// Unmarshalable value → marshal error, nothing written.
	if err := WriteConfig(path, map[string]interface{}{"f": func() {}}, 0o600); err == nil {
		t.Error("unmarshalable config should error")
	}
	// Unwritable directory → write error.
	if err := WriteConfig(filepath.Join(dir, "nodir", "config.json"), map[string]interface{}{}, 0o600); err == nil {
		t.Error("missing parent dir should error")
	}
}

func TestClearExpiredTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Missing config → the read error is surfaced and nothing is created.
	if err := ClearExpiredTokens(path, 0o660); !os.IsNotExist(err) {
		t.Errorf("missing config should return IsNotExist, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("ClearExpiredTokens must not create a config")
	}

	body := ConfigHeader + `{"copilotTokens":{"https://github.com:alice":"gho_old"},"loggedInUsers":["https://github.com:alice"],"lastLoggedInUser":"https://github.com:alice","other":1}`
	if err := os.WriteFile(path, []byte(body), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := ClearExpiredTokens(path, 0o660); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	toks, ok := cfg["copilotTokens"].(map[string]interface{})
	if !ok || len(toks) != 0 {
		t.Errorf("copilotTokens should be an empty map, got %v", cfg["copilotTokens"])
	}
	// Login identity and unrelated keys are preserved.
	if cfg["lastLoggedInUser"] != "https://github.com:alice" {
		t.Errorf("lastLoggedInUser must be preserved, got %v", cfg["lastLoggedInUser"])
	}
	if users, _ := cfg["loggedInUsers"].([]interface{}); len(users) != 1 {
		t.Errorf("loggedInUsers must be preserved, got %v", cfg["loggedInUsers"])
	}
	if cfg["other"] != float64(1) {
		t.Errorf("unrelated keys must be preserved, got %v", cfg["other"])
	}
	raw, _ := os.ReadFile(path)
	if string(raw[:len(ConfigHeader)]) != ConfigHeader {
		t.Error("CLI header must be preserved on rewrite")
	}
	if CredentialFileHasTokens(path) {
		t.Error("cleared config must report no tokens")
	}
}

func TestReplaceTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	noLookup := func(string) string { return "" }

	// Blank token is a no-op and does not create a file.
	if err := ReplaceTokens(path, "  ", 0o660, noLookup); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("blank token must not create a config")
	}

	// Missing config + successful lookup → fresh config with canonical identity.
	if err := ReplaceTokens(path, " gho_new ", 0o660, func(tok string) string {
		if tok != "gho_new" {
			t.Errorf("lookup received %q, want trimmed gho_new", tok)
		}
		return "carol"
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	toks, _ := cfg["copilotTokens"].(map[string]interface{})
	if got, _ := toks["https://github.com:carol"].(string); got != "gho_new" {
		t.Errorf("token should be keyed by resolved identity, got %v", toks)
	}
	id, _ := cfg["lastLoggedInUser"].(map[string]interface{})
	if id["host"] != "https://github.com" || id["login"] != "carol" {
		t.Errorf("canonical identity not written: %v", cfg["lastLoggedInUser"])
	}
	if users, _ := cfg["loggedInUsers"].([]interface{}); len(users) != 1 {
		t.Errorf("loggedInUsers should hold the single new identity, got %v", cfg["loggedInUsers"])
	}
	if got := ExtractToken(path); got != "gho_new" {
		t.Errorf("ExtractToken after replace = %q, want gho_new", got)
	}

	// Existing stale identity + failed lookup → the previous identity is
	// deliberately DROPPED (unlike RestoreTokens) and the legacy shape used.
	stale := ConfigHeader + `{"copilotTokens":{"https://github.com:stale":"gho_stale"},"loggedInUsers":[{"host":"https://github.com","login":"stale"}],"lastLoggedInUser":{"host":"https://github.com","login":"stale"},"keep":true}`
	if err := os.WriteFile(path, []byte(stale), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceTokens(path, "gho_env", 0o660, noLookup); err != nil {
		t.Fatal(err)
	}
	cfg, err = ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	toks, _ = cfg["copilotTokens"].(map[string]interface{})
	if len(toks) != 1 {
		t.Errorf("stale token must be removed, got %v", toks)
	}
	obj, _ := toks["github.com"].(map[string]interface{})
	if got, _ := obj["token"].(string); got != "gho_env" {
		t.Errorf("legacy shape token = %q, want gho_env; tokens=%v", got, toks)
	}
	if _, has := cfg["lastLoggedInUser"]; has {
		t.Error("lastLoggedInUser must be removed when the owner cannot be resolved")
	}
	if _, has := cfg["loggedInUsers"]; has {
		t.Error("loggedInUsers must be removed when the owner cannot be resolved")
	}
	if cfg["keep"] != true {
		t.Error("unrelated keys must be preserved")
	}

	// Malformed-but-present config is honored as an error, not clobbered.
	if err := os.WriteFile(path, []byte("{"), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceTokens(path, "gho_x", 0o660, noLookup); err == nil {
		t.Error("malformed config should error")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "{" {
		t.Error("malformed config must not be overwritten")
	}
}

func TestRestoreTokens_Errors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	noLookup := func(string) string { return "" }

	// Blank token no-op.
	if err := RestoreTokens(path, "", 0o660, noLookup); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("blank token must not create a config")
	}
	// Malformed config is an error and left untouched.
	if err := os.WriteFile(path, []byte("{"), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := RestoreTokens(path, "gho_x", 0o660, noLookup); err == nil {
		t.Error("malformed config should error")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "{" {
		t.Error("malformed config must not be overwritten")
	}
}

func TestIdentityKey(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"nil", nil, ""},
		{"number", 3, ""},
		{"canonical string", " https://github.com:alice ", "https://github.com:alice"},
		{"bare host string", "github.com", ""},
		{"http without login", "https://github.com", ""},
		{"object", map[string]interface{}{"host": "https://github.com", "login": "bob"}, "https://github.com:bob"},
		{"object junk host", map[string]interface{}{"host": "github.com", "login": "bob"}, ""},
		{"object blank login", map[string]interface{}{"host": "https://github.com", "login": " "}, ""},
		{"object missing fields", map[string]interface{}{}, ""},
	}
	for _, tc := range cases {
		if got := IdentityKey(tc.in); got != tc.want {
			t.Errorf("%s: IdentityKey(%v) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestExtractToken_NoTokenMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"copilotTokens":"gho_x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ExtractToken(path); got != "" {
		t.Errorf("non-map copilotTokens should yield \"\", got %q", got)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ExtractToken(path); got != "" {
		t.Errorf("absent copilotTokens should yield \"\", got %q", got)
	}
}

func TestWriteDurableToken_Error(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDurableToken(filepath.Join(dir, "nodir", "token"), "ghu_x"); err == nil {
		t.Error("missing parent dir should error")
	}
}
