package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// extractCopilotToken must handle both value shapes the CLI writes, honor the
// active identity, and refuse ambiguous multi-account configs.
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
	// The selected identity wins even when another account also has a token.
	if got := extractCopilotToken(write(`{"lastLoggedInUser":{"host":"https://github.com","login":"licensed"},"copilotTokens":{"https://github.com:other":"gho_other","https://github.com:licensed":"gho_licensed"}}`)); got != "gho_licensed" {
		t.Errorf("active identity: got %q, want gho_licensed", got)
	}
	// A selected identity with no usable token must not fall through to a
	// different account, and a config with no selected identity is safe only
	// when all usable entries represent the same token.
	if got := extractCopilotToken(write(`{"lastLoggedInUser":"https://github.com:missing","copilotTokens":{"https://github.com:other":"gho_other"}}`)); got != "" {
		t.Errorf("missing active identity: got %q, want \"\"", got)
	}
	if got := extractCopilotToken(write(`{"copilotTokens":{"https://github.com:a":"gho_a","https://github.com:b":"gho_b"}}`)); got != "" {
		t.Errorf("ambiguous identities: got %q, want \"\"", got)
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

// An explicitly configured COPILOT_GITHUB_TOKEN has higher precedence than
// credentials in config.json. The reconciler must preserve that direction
// instead of promoting a stale, potentially unlicensed CLI identity over it.
func TestSyncCopilotToken_AuthoritativeTokenReplacesCLIIdentity(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	dur := filepath.Join(dir, "durable")
	if err := os.WriteFile(cfg, []byte(copilotConfigHeader+`{"lastLoggedInUser":{"host":"https://github.com","login":"old"},"loggedInUsers":[{"host":"https://github.com","login":"old"}],"copilotTokens":{"https://github.com:old":"gho_unlicensed"}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	origLookup := githubTokenLogin
	githubTokenLogin = func(token string) string {
		if token == "ghu_licensed" {
			return "licensed"
		}
		return ""
	}
	defer func() { githubTokenLogin = origLookup }()

	m := testManager(5)
	m.copilotAuthToken = "ghu_licensed"
	m.copilotAuthTokenAuthoritative = true
	if act := m.syncCopilotToken(cfg, dur); act != copilotSyncSeed {
		t.Fatalf("action = %v, want seed", act)
	}
	if got := extractCopilotToken(cfg); got != "ghu_licensed" {
		t.Fatalf("active config token = %q, want ghu_licensed", got)
	}
	parsed, err := readCopilotConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := copilotIdentityKey(parsed["lastLoggedInUser"]); got != "https://github.com:licensed" {
		t.Fatalf("active identity = %q, want licensed identity", got)
	}
	if _, err := os.Stat(dur); !os.IsNotExist(err) {
		t.Fatal("authoritative seed must not overwrite the durable source")
	}
}

func TestNewManagerMarksEnvironmentCopilotTokenAuthoritative(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "ghu_from_env")
	m := NewManager(map[string]config.AgentConfig{}, testManager(5).logger, ProjectContext{ACMMLevel: 5})
	if got := m.CopilotToken(); got != "ghu_from_env" {
		t.Fatalf("manager token = %q, want ghu_from_env", got)
	}
	if !m.copilotAuthTokenAuthoritative {
		t.Fatal("COPILOT_GITHUB_TOKEN must be authoritative over shared CLI config")
	}
}

func TestActivateCopilotTokenUpdatesMemoryWhenConfigWriteFails(t *testing.T) {
	m := testManager(5)
	badPath := filepath.Join(t.TempDir(), "missing", "config.json")
	origPath := sharedCopilotConfigPath
	sharedCopilotConfigPath = badPath
	defer func() { sharedCopilotConfigPath = origPath }()

	if err := m.ActivateCopilotToken("ghu_fresh"); err == nil {
		t.Fatal("ActivateCopilotToken should report the config write failure")
	}
	if got := m.CopilotToken(); got != "ghu_fresh" {
		t.Fatalf("in-memory token = %q, want ghu_fresh", got)
	}
	if !m.copilotAuthTokenAuthoritative {
		t.Fatal("fresh dashboard token must remain authoritative for a retry")
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

// --- #6500: logout must not leave an authoritative EMPTY token ---------------

// A dashboard logout calls SetCopilotToken(""). If that marked the empty token
// authoritative, syncCopilotToken's SEED branch would return noop forever
// (authoritative && held == ""), permanently disabling PROMOTE: an operator who
// logs out and then runs /login inside an agent would never have that login
// mirrored to the durable store, losing it on the next roll.
func TestSetCopilotTokenEmptyIsNeverAuthoritative(t *testing.T) {
	m := testManager(5)

	m.SetCopilotToken("gho_real")
	if !m.copilotAuthTokenAuthoritative {
		t.Fatal("a non-empty explicit token must be authoritative")
	}

	// The logout path.
	m.SetCopilotToken("")
	if m.copilotAuthTokenAuthoritative {
		t.Fatal("an empty token must never be authoritative (#6500)")
	}

	// Whitespace is not a token either.
	m.SetCopilotToken("gho_real")
	m.SetCopilotToken("   ")
	if m.copilotAuthTokenAuthoritative {
		t.Fatal("a whitespace-only token must never be authoritative")
	}
}

// End-to-end shape of the regression: log out, then log in inside an agent.
// The in-agent login must still be promoted to the durable store.
func TestSyncCopilotToken_PromoteStillWorksAfterLogout(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	dur := filepath.Join(dir, "durable")
	if err := os.WriteFile(cfg, []byte(copilotConfigHeader+`{"copilotTokens":{"https://github.com:me":"gho_fromcli"}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	m := testManager(5)
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}

	// An operator had logged in via the dashboard, then logged out.
	m.SetCopilotToken("gho_dashboard")
	m.SetCopilotToken("")

	// Now someone runs /login inside an agent; the CLI config holds that token.
	if act := m.syncCopilotToken(cfg, dur); act != copilotSyncPromote {
		t.Fatalf("action = %v, want promote after logout (#6500)", act)
	}
	if b, _ := os.ReadFile(dur); string(b) != "gho_fromcli" {
		t.Errorf("durable file = %q, want gho_fromcli", string(b))
	}
	if m.CopilotToken() != "gho_fromcli" {
		t.Errorf("in-memory token = %q, want gho_fromcli", m.CopilotToken())
	}
}

// The fix must NOT weaken #6514: a real dashboard/env token still outranks a
// stale identity sitting in the shared CLI config.
func TestSyncCopilotToken_AuthoritativeStillWinsAfterFix(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	dur := filepath.Join(dir, "durable")
	if err := os.WriteFile(cfg, []byte(copilotConfigHeader+`{"copilotTokens":{"https://github.com:stale":"gho_stale"}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	m := testManager(5)
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.SetCopilotToken("gho_licensed")

	if act := m.syncCopilotToken(cfg, dur); act != copilotSyncSeed {
		t.Fatalf("action = %v, want seed (authoritative token must replace stale CLI identity)", act)
	}
	if got := extractCopilotToken(cfg); got != "gho_licensed" {
		t.Errorf("config token = %q, want gho_licensed", got)
	}
}

// --- #6767: authoritative-but-known-bad token must not clobber recovery -----

// The exact scenario @MikeSpreitzer reported on v4 commit 917713cf: the hive
// boots with a licensed-at-the-time COPILOT_GITHUB_TOKEN (authoritative). The
// upstream Copilot seat for that identity later stops being licensed, so every
// copilot agent prints "You are not licensed to use Copilot" (Request ID …).
// An operator opens the agent Terminal and runs /login with a DIFFERENT,
// currently-licensed identity — the CLI writes the fresh token into the shared
// config.json. Before this fix the next syncCopilotToken tick (~30 s later)
// unconditionally rewrote that fresh token back to the stale-authoritative
// one, so agents worked briefly and then reverted to "not licensed" — the
// exact "went back to saying" loop Mike posted on 2026-09-11.
//
// After the fix: once markProviderErrorLocked has recorded a copilot agent as
// BackendAuthUnlicensed, syncCopilotToken must treat a differing real cliTok
// as an in-agent recovery /login and PROMOTE it over the known-bad token.
func TestSyncCopilotToken_AuthoritativeYieldsToRecoveryAfterUnlicensed_6767(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	dur := filepath.Join(dir, "durable")
	// The recovering operator's /login has just written this token.
	if err := os.WriteFile(cfg, []byte(copilotConfigHeader+`{"copilotTokens":{"https://github.com:mike":"gho_recovery"}}`), 0o660); err != nil {
		t.Fatal(err)
	}

	m := testManager(5)
	scanner := &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.agents["scanner"] = scanner
	// Fleet was configured with a token that WAS licensed at boot.
	m.SetCopilotToken("gho_stale_authoritative")

	// Fleet-wide "not licensed to use Copilot" — the message from the issue.
	// markProviderErrorLocked is called under m.mu.
	m.mu.Lock()
	m.markProviderErrorLocked(scanner, providerErrorMatch{
		Class: "auth",
		Line:  "✗ You are not licensed to use Copilot. (Request ID: 2686:2B2C94:EA7FE5:105C478:6AA463E1)",
	}, time.Now())
	m.mu.Unlock()

	// The reconciler tick that used to clobber the recovery /login.
	act := m.syncCopilotToken(cfg, dur)
	if act != copilotSyncPromote {
		t.Fatalf("action = %v, want promote (recovery /login must survive a known-bad authoritative token) — #6767", act)
	}
	if got := extractCopilotToken(cfg); got != "gho_recovery" {
		t.Errorf("config token = %q, want gho_recovery (recovery login preserved)", got)
	}
	if got, _ := os.ReadFile(dur); string(got) != "gho_recovery" {
		t.Errorf("durable file = %q, want gho_recovery (recovery login promoted)", string(got))
	}
	if got := m.CopilotToken(); got != "gho_recovery" {
		t.Errorf("in-memory token = %q, want gho_recovery", got)
	}
}

// The rejection latch is one-shot per token: once a subsequent
// setCopilotToken installs a different value (dashboard re-login, env change,
// or the PROMOTE above), authoritative precedence is rearmed. Without this
// the rejected flag would sit true forever and #6514's stale-account
// protection would be permanently defeated after the first upstream 403.
func TestCopilotAuthTokenRejectedClearsOnNewToken_6767(t *testing.T) {
	m := testManager(5)
	m.SetCopilotToken("gho_first")
	m.mu.Lock()
	m.copilotAuthTokenRejected = true
	m.mu.Unlock()

	// Same value must NOT clear the latch — the operator hasn't recovered.
	m.SetCopilotToken("gho_first")
	m.mu.RLock()
	stillRejected := m.copilotAuthTokenRejected
	m.mu.RUnlock()
	if !stillRejected {
		t.Fatal("re-setting the same token must not clear the rejection latch")
	}

	// A truly new token means the operator supplied fresh credentials.
	m.SetCopilotToken("gho_recovered")
	m.mu.RLock()
	cleared := !m.copilotAuthTokenRejected
	auth := m.copilotAuthTokenAuthoritative
	m.mu.RUnlock()
	if !cleared {
		t.Error("installing a different token must clear the rejection latch")
	}
	if !auth {
		t.Error("a fresh non-empty token must be authoritative again after recovery")
	}
}

// #6514 must still hold when there is NO upstream evidence the authoritative
// token is bad: a stale account sitting in the shared CLI config still loses
// to a real dashboard/env token. This is the "did the fix regress the
// previous fix?" guard.
func TestSyncCopilotToken_AuthoritativeStillWinsWithoutRejection_6767(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	dur := filepath.Join(dir, "durable")
	if err := os.WriteFile(cfg, []byte(copilotConfigHeader+`{"copilotTokens":{"https://github.com:stale":"gho_stale"}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	m := testManager(5)
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.SetCopilotToken("gho_licensed")
	// No copilot agent has reported "not licensed" — nothing latched.

	if act := m.syncCopilotToken(cfg, dur); act != copilotSyncSeed {
		t.Fatalf("action = %v, want seed (authoritative token still wins when it hasn't been rejected)", act)
	}
	if got := extractCopilotToken(cfg); got != "gho_licensed" {
		t.Errorf("config token = %q, want gho_licensed", got)
	}
}
