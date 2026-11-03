package copilottoken

import (
	"os"
	"path/filepath"
	"testing"
)

// ExtractToken must handle both value shapes the CLI writes, honor the
// active identity, and refuse ambiguous multi-account configs.
func TestExtractToken(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "config.json")
		if err := os.WriteFile(p, []byte(ConfigHeader+body), 0o660); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// bare-string shape (in-agent /login on 1.0.78)
	if got := ExtractToken(write(`{"copilotTokens":{"https://github.com:me":"gho_bare"}}`)); got != "gho_bare" {
		t.Errorf("string shape: got %q, want gho_bare", got)
	}
	// object shape (restoreCopilotTokens / other CLI versions)
	if got := ExtractToken(write(`{"copilotTokens":{"github.com":{"token":"gho_obj"}}}`)); got != "gho_obj" {
		t.Errorf("object shape: got %q, want gho_obj", got)
	}
	// empty map
	if got := ExtractToken(write(`{"copilotTokens":{}}`)); got != "" {
		t.Errorf("empty map: got %q, want \"\"", got)
	}
	// missing file
	if got := ExtractToken(filepath.Join(dir, "nope.json")); got != "" {
		t.Errorf("missing file: got %q, want \"\"", got)
	}
	// masked placeholder (the CLI redacts a rejected token as asterisks) must
	// never be extracted — promoting it would overwrite the durable token.
	if got := ExtractToken(write(`{"copilotTokens":{"github.com":"******"}}`)); got != "" {
		t.Errorf("masked string shape: got %q, want \"\"", got)
	}
	if got := ExtractToken(write(`{"copilotTokens":{"github.com":{"token":"********"}}}`)); got != "" {
		t.Errorf("masked object shape: got %q, want \"\"", got)
	}
	// The selected identity wins even when another account also has a token.
	if got := ExtractToken(write(`{"lastLoggedInUser":{"host":"https://github.com","login":"licensed"},"copilotTokens":{"https://github.com:other":"gho_other","https://github.com:licensed":"gho_licensed"}}`)); got != "gho_licensed" {
		t.Errorf("active identity: got %q, want gho_licensed", got)
	}
	// A selected identity with no usable token must not fall through to a
	// different account, and a config with no selected identity is safe only
	// when all usable entries represent the same token.
	if got := ExtractToken(write(`{"lastLoggedInUser":"https://github.com:missing","copilotTokens":{"https://github.com:other":"gho_other"}}`)); got != "" {
		t.Errorf("missing active identity: got %q, want \"\"", got)
	}
	if got := ExtractToken(write(`{"copilotTokens":{"https://github.com:a":"gho_a","https://github.com:b":"gho_b"}}`)); got != "" {
		t.Errorf("ambiguous identities: got %q, want \"\"", got)
	}
}

func TestWriteDurableToken(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "copilot-user-token")
	if err := WriteDurableToken(p, "ghu_dur"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "ghu_dur" {
		t.Errorf("durable file = %q, want ghu_dur", string(b))
	}
	// blank is a no-op (must not create/overwrite)
	if err := WriteDurableToken(p, "   "); err != nil {
		t.Fatalf("blank should no-op, got %v", err)
	}
	b, _ = os.ReadFile(p)
	if string(b) != "ghu_dur" {
		t.Error("blank write must not overwrite an existing token")
	}
}

// restoreCopilotTokens must store the seeded token under the preserved login
// identity when one is on file — the string shape a real /login writes — so
// the interactive CLI recognizes the seed as a signed-in session instead of
// showing "Please use /login" over a valid token. With no identity, it falls
// back to the host-keyed object shape.
func TestRestoreTokens_IdentityShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	lookup := func(string) string { return "" }

	// Identity present (preserved by clearExpiredTokens) → token stored under
	// the identity key as a bare string.
	if err := os.WriteFile(path, []byte(ConfigHeader+`{"copilotTokens":{},"loggedInUsers":["https://github.com:alice"],"lastLoggedInUser":"https://github.com:alice"}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := RestoreTokens(path, "gho_seeded", 0o660, lookup); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	toks, _ := cfg["copilotTokens"].(map[string]interface{})
	if got, _ := toks["https://github.com:alice"].(string); got != "gho_seeded" {
		t.Errorf("identity-keyed token = %q, want gho_seeded under the identity key; tokens=%v", got, toks)
	}

	// Object-shaped identity ({"host","login"} — what Copilot 1.0.78 actually
	// writes) → token stored under "<host>:<login>".
	if err := os.WriteFile(path, []byte(ConfigHeader+`{"copilotTokens":{},"lastLoggedInUser":{"host":"https://github.com","login":"bob"}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := RestoreTokens(path, "gho_obj", 0o660, lookup); err != nil {
		t.Fatal(err)
	}
	cfg, err = ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	toks, _ = cfg["copilotTokens"].(map[string]interface{})
	if got, _ := toks["https://github.com:bob"].(string); got != "gho_obj" {
		t.Errorf("object-identity token = %q, want gho_obj under https://github.com:bob; tokens=%v", got, toks)
	}

	// JUNK identity (a bare "github.com" string, observed live from the
	// polluted shared-config lineage) must NOT be used as a key: with the
	// owner lookup also failing, fall back to the legacy shape.
	if err := os.WriteFile(path, []byte(ConfigHeader+`{"copilotTokens":{},"lastLoggedInUser":"github.com"}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := RestoreTokens(path, "gho_junkid", 0o660, lookup); err != nil {
		t.Fatal(err)
	}
	cfg, err = ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	toks, _ = cfg["copilotTokens"].(map[string]interface{})
	if _, bad := toks["github.com"].(string); bad {
		t.Error("junk identity must not become a bare string token key")
	}

	// No valid identity but the owner lookup SUCCEEDS → full canonical
	// identity written from the token's true owner.
	lookup = func(string) string { return "alice" }
	if err := os.WriteFile(path, []byte(ConfigHeader+`{"copilotTokens":{}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := RestoreTokens(path, "gho_resolved", 0o660, lookup); err != nil {
		t.Fatal(err)
	}
	cfg, err = ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	toks, _ = cfg["copilotTokens"].(map[string]interface{})
	if got, _ := toks["https://github.com:alice"].(string); got != "gho_resolved" {
		t.Errorf("resolved owner should key the token, got %v", toks)
	}
	id, _ := cfg["lastLoggedInUser"].(map[string]interface{})
	if id["login"] != "alice" || id["host"] != "https://github.com" {
		t.Errorf("canonical identity not written: %v", cfg["lastLoggedInUser"])
	}
	lookup = func(string) string { return "" }

	// No identity → legacy host-keyed object shape (unchanged behavior).
	if err := os.WriteFile(path, []byte(ConfigHeader+`{"copilotTokens":{}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := RestoreTokens(path, "gho_plain", 0o660, lookup); err != nil {
		t.Fatal(err)
	}
	cfg, err = ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	toks, _ = cfg["copilotTokens"].(map[string]interface{})
	obj, _ := toks["github.com"].(map[string]interface{})
	if got, _ := obj["token"].(string); got != "gho_plain" {
		t.Errorf("no-identity token = %q, want gho_plain under github.com object shape; tokens=%v", got, toks)
	}
}
