package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// extractCopilotToken must handle both value shapes the CLI writes: a bare
// string and a {"token":…} object; and return "" when there is none.
func TestExtractCopilotToken(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "config.json")
		if err := os.WriteFile(p, []byte(copilotConfigHeader+body), 0o660); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// bare-string shape (in-agent /login on 1.0.78)
	if got := extractCopilotToken(write(`{"copilotTokens":{"https://github.com:me":"gho_bare"}}`)); got != "gho_bare" {
		t.Errorf("string shape: got %q, want gho_bare", got)
	}
	// object shape (restoreCopilotTokens / other CLI versions)
	if got := extractCopilotToken(write(`{"copilotTokens":{"github.com":{"token":"gho_obj"}}}`)); got != "gho_obj" {
		t.Errorf("object shape: got %q, want gho_obj", got)
	}
	// empty map
	if got := extractCopilotToken(write(`{"copilotTokens":{}}`)); got != "" {
		t.Errorf("empty map: got %q, want \"\"", got)
	}
	// missing file
	if got := extractCopilotToken(filepath.Join(dir, "nope.json")); got != "" {
		t.Errorf("missing file: got %q, want \"\"", got)
	}
	// masked placeholder (the CLI redacts a rejected token as asterisks) must
	// never be extracted — promoting it would overwrite the durable token.
	if got := extractCopilotToken(write(`{"copilotTokens":{"github.com":"******"}}`)); got != "" {
		t.Errorf("masked string shape: got %q, want \"\"", got)
	}
	if got := extractCopilotToken(write(`{"copilotTokens":{"github.com":{"token":"********"}}}`)); got != "" {
		t.Errorf("masked object shape: got %q, want \"\"", got)
	}
}

func TestWriteDurableCopilotToken(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "copilot-user-token")
	if err := writeDurableCopilotToken(p, "ghu_dur"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "ghu_dur" {
		t.Errorf("durable file = %q, want ghu_dur", string(b))
	}
	// blank is a no-op (must not create/overwrite)
	if err := writeDurableCopilotToken(p, "   "); err != nil {
		t.Fatalf("blank should no-op, got %v", err)
	}
	b, _ = os.ReadFile(p)
	if string(b) != "ghu_dur" {
		t.Error("blank write must not overwrite an existing token")
	}
}

// syncCopilotToken PROMOTE: a token in config but none held by the hive → the
// token is mirrored to the durable file AND SetCopilotToken updates memory.
// This is the "logged in inside the agent" case the operator hit.
func TestSyncCopilotToken_Promote(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	dur := filepath.Join(dir, "durable")
	if err := os.WriteFile(cfg, []byte(copilotConfigHeader+`{"copilotTokens":{"https://github.com:me":"gho_fromcli"}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	m := testManager(5)
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.copilotAuthToken = "" // hive holds nothing (in-agent login bypassed the hive)

	if act := m.syncCopilotToken(cfg, dur); act != copilotSyncPromote {
		t.Fatalf("action = %v, want promote", act)
	}
	b, _ := os.ReadFile(dur)
	if string(b) != "gho_fromcli" {
		t.Errorf("durable file = %q, want gho_fromcli (promoted from config)", string(b))
	}
	if m.CopilotToken() != "gho_fromcli" {
		t.Errorf("in-memory token = %q, want gho_fromcli (SetCopilotToken)", m.CopilotToken())
	}
}

// PROMOTE no-op when the hive already holds exactly the CLI's token.
func TestSyncCopilotToken_PromoteNoopWhenAlreadyHeld(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	dur := filepath.Join(dir, "durable")
	if err := os.WriteFile(cfg, []byte(copilotConfigHeader+`{"copilotTokens":{"github.com":{"token":"gho_same"}}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	m := testManager(5)
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.copilotAuthToken = "gho_same"
	if act := m.syncCopilotToken(cfg, dur); act != copilotSyncNoop {
		t.Fatalf("action = %v, want noop (already held)", act)
	}
	if _, err := os.Stat(dur); !os.IsNotExist(err) {
		t.Error("durable file must not be written when nothing changed")
	}
}

// SEED: config empty but hive holds a token → config re-populated (the #4494
// direction still works through the merged path).
func TestSyncCopilotToken_Seed(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	dur := filepath.Join(dir, "durable")
	if err := os.WriteFile(cfg, []byte(copilotConfigHeader+`{"copilotTokens":{}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	m := testManager(5)
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.copilotAuthToken = "ghu_held"
	if act := m.syncCopilotToken(cfg, dur); act != copilotSyncSeed {
		t.Fatalf("action = %v, want seed", act)
	}
	if !copilotCredentialFileHasTokens(cfg) {
		t.Error("config must be re-seeded")
	}
}

// MASKED-ONLY config: the CLI has redacted its token to "******" (rejected
// credential — the live EPM shape). Must take the SEED path (restore the valid
// durable token over the placeholder), NOT promote the asterisks into the
// durable store, which would destroy the one good credential.
func TestSyncCopilotToken_MaskedConfigSeedsNotPromotes(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	dur := filepath.Join(dir, "durable")
	if err := os.WriteFile(cfg, []byte(copilotConfigHeader+`{"copilotTokens":{"github.com":"******"},"lastLoggedInUser":"https://github.com:me","loggedInUsers":["https://github.com:me"]}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dur, []byte("ghu_valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := testManager(5)
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.copilotAuthToken = "ghu_valid"
	if act := m.syncCopilotToken(cfg, dur); act != copilotSyncSeed {
		t.Fatalf("action = %v, want seed (masked placeholder is not a token)", act)
	}
	b, _ := os.ReadFile(dur)
	if string(b) != "ghu_valid" {
		t.Errorf("durable file = %q — masked garbage was promoted over the valid token", string(b))
	}
	if got := extractCopilotToken(cfg); got != "ghu_valid" {
		t.Errorf("config token after seed = %q, want ghu_valid", got)
	}
}

// Both empty → noop (genuine logout; watchdog alert + manual login covers it).
func TestSyncCopilotToken_BothEmptyNoop(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	dur := filepath.Join(dir, "durable")
	if err := os.WriteFile(cfg, []byte(copilotConfigHeader+`{"copilotTokens":{}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	m := testManager(5)
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.copilotAuthToken = ""
	if act := m.syncCopilotToken(cfg, dur); act != copilotSyncNoop {
		t.Fatalf("action = %v, want noop", act)
	}
}

// refreshCopilotSessionToken no-ops entirely without a copilot backend.
func TestRefreshCopilotSessionToken_NoCopilotBackend(t *testing.T) {
	m := testManager(5)
	m.agents["a"] = &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "claude"}}
	m.copilotAuthToken = "gho_held"
	// Should return without panicking or touching the shared/durable paths.
	m.refreshCopilotSessionToken()
}

// restoreCopilotTokens must store the seeded token under the preserved login
// identity when one is on file — the string shape a real /login writes — so
// the interactive CLI recognizes the seed as a signed-in session instead of
// showing "Please use /login" over a valid token. With no identity, it falls
// back to the host-keyed object shape.
func TestRestoreCopilotTokens_IdentityShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Identity present (preserved by clearExpiredTokens) → token stored under
	// the identity key as a bare string.
	if err := os.WriteFile(path, []byte(copilotConfigHeader+`{"copilotTokens":{},"loggedInUsers":["https://github.com:alice"],"lastLoggedInUser":"https://github.com:alice"}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := restoreCopilotTokens(path, "gho_seeded"); err != nil {
		t.Fatal(err)
	}
	cfg, err := readCopilotConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	toks, _ := cfg["copilotTokens"].(map[string]interface{})
	if got, _ := toks["https://github.com:alice"].(string); got != "gho_seeded" {
		t.Errorf("identity-keyed token = %q, want gho_seeded under the identity key; tokens=%v", got, toks)
	}

	// Object-shaped identity ({"host","login"} — what Copilot 1.0.78 actually
	// writes) → token stored under "<host>:<login>".
	if err := os.WriteFile(path, []byte(copilotConfigHeader+`{"copilotTokens":{},"lastLoggedInUser":{"host":"https://github.com","login":"bob"}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := restoreCopilotTokens(path, "gho_obj"); err != nil {
		t.Fatal(err)
	}
	cfg, err = readCopilotConfig(path)
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
	origLookup := githubTokenLogin
	githubTokenLogin = func(string) string { return "" }
	defer func() { githubTokenLogin = origLookup }()
	if err := os.WriteFile(path, []byte(copilotConfigHeader+`{"copilotTokens":{},"lastLoggedInUser":"github.com"}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := restoreCopilotTokens(path, "gho_junkid"); err != nil {
		t.Fatal(err)
	}
	cfg, err = readCopilotConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	toks, _ = cfg["copilotTokens"].(map[string]interface{})
	if _, bad := toks["github.com"].(string); bad {
		t.Error("junk identity must not become a bare string token key")
	}

	// No valid identity but the owner lookup SUCCEEDS → full canonical
	// identity written from the token's true owner.
	githubTokenLogin = func(string) string { return "alice" }
	if err := os.WriteFile(path, []byte(copilotConfigHeader+`{"copilotTokens":{}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := restoreCopilotTokens(path, "gho_resolved"); err != nil {
		t.Fatal(err)
	}
	cfg, err = readCopilotConfig(path)
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
	githubTokenLogin = func(string) string { return "" }

	// No identity → legacy host-keyed object shape (unchanged behavior).
	if err := os.WriteFile(path, []byte(copilotConfigHeader+`{"copilotTokens":{}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := restoreCopilotTokens(path, "gho_plain"); err != nil {
		t.Fatal(err)
	}
	cfg, err = readCopilotConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	toks, _ = cfg["copilotTokens"].(map[string]interface{})
	obj, _ := toks["github.com"].(map[string]interface{})
	if got, _ := obj["token"].(string); got != "gho_plain" {
		t.Errorf("no-identity token = %q, want gho_plain under github.com object shape; tokens=%v", got, toks)
	}
}
