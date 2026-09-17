package agent

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
)

// Copilot credential store (#7303 stage 1).
//
// Everything here reads, parses, rewrites or persists the credential FILES the
// Copilot and Claude CLIs keep on disk — as opposed to the in-memory token the
// Manager hands to agents, which stays in manager.go. Split out of manager.go
// unchanged: this is a pure move, so `go build` and the existing pkg/agent
// tests are the proof of equivalence.
//
// The cohesion is the file format, not the caller. These helpers all have to
// know that the Copilot CLI's config is a JSON object keyed by host, that its
// managed header must be preserved on rewrite, and that a token value can
// arrive as several shapes — knowledge that has no business being interleaved
// with tmux plumbing and kick orchestration.

// configHasTokens returns true if either the Copilot config or Claude
// credentials file holds a credential a restart can still use. Used to decide
// whether an agent stuck on a login prompt can be auto-restarted.
//
// claude.HasUsableToken, not HasValidToken: the single most common reason a
// Claude agent sits at "Please run /login" is that its access token aged out
// under a long-lived tmux session. Claude Code pins the token it read at
// startup for the life of the process — it neither re-reads the file nor
// refreshes mid-session — so the pane 401s while the refresh grant on disk is
// still good for weeks. That is EXACTLY the case this heal was built for, and
// gating it on HasValidToken excluded it: the file said "expired", the heal
// stood down, and the operator was paged to redo a login that a restart would
// have made unnecessary.
func configHasTokens() bool {
	if claude.HasUsableToken(sharedClaudeCredentialPath) {
		return true
	}
	return copilotConfigHasTokens()
}

// copilotConfigHasTokens reads the shared Copilot config.json, strips single-line
// // comments (which Copilot CLI sometimes writes), parses the JSON, and returns
// true if the "copilotTokens" field has at least one entry.
func copilotConfigHasTokens() bool {
	return copilotCredentialFileHasTokens(sharedCopilotConfigPath)
}

// copilotCredentialFileHasTokens is copilotConfigHasTokens for an ARBITRARY
// path, so the per-agent auth probe can read the same file shapes under an
// agent's own per-UID home instead of only the shared legacy location.
//
// Two shapes are accepted because the Copilot CLI uses both:
//   - .copilot/config.json — token map under the "copilotTokens" key.
//   - .config/github-copilot/{apps,hosts}.json — a flat map keyed by host,
//     each entry carrying an oauth_token. Any non-empty top-level map counts.
func copilotCredentialFileHasTokens(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	// Strip single-line // comments that Copilot CLI sometimes adds.
	var cleaned []byte
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		cleaned = append(cleaned, []byte(line+"\n")...)
	}

	var cfg map[string]interface{}
	if err := json.Unmarshal(cleaned, &cfg); err != nil {
		return false
	}
	if tokens, ok := cfg["copilotTokens"]; ok {
		tokensMap, ok := tokens.(map[string]interface{})
		if !ok {
			return false
		}
		// Count only USABLE tokens. The CLI masks a token it refuses to use
		// (foreign/expired) as a literal run of asterisks; a config holding
		// only masked entries must read as empty so the sync takes the SEED
		// path (restore the valid durable token) instead of the PROMOTE path
		// — promoting would overwrite the durable file with "******" and
		// destroy the one good credential (seen live on the EPM hive).
		for _, v := range tokensMap {
			switch t := v.(type) {
			case string:
				if copilotTokenValueUsable(t) {
					return true
				}
			case map[string]interface{}:
				if s, ok := t["token"].(string); ok && copilotTokenValueUsable(s) {
					return true
				}
			}
		}
		return false
	}
	// apps.json / hosts.json shape: host -> {oauth_token: ...}
	if strings.HasSuffix(path, "apps.json") || strings.HasSuffix(path, "hosts.json") {
		for _, v := range cfg {
			entry, ok := v.(map[string]interface{})
			if !ok {
				continue
			}
			if tok, ok := entry["oauth_token"].(string); ok && tok != "" {
				return true
			}
		}
	}
	return false
}

// copilotConfigHeader is the two-line // preamble the Copilot CLI writes atop
// its JSONC config.json. We preserve it byte-for-byte on every rewrite so the
// file keeps reading as the CLI's own managed file rather than a foreign one.
const copilotConfigHeader = "// User settings belong in settings.json.\n// This file is managed automatically.\n"

// readCopilotConfig loads config.json, strips the CLI's // comment lines, and
// unmarshals the remainder. A read error (including a missing file) is returned
// to the caller — clearExpiredTokens relies on that to no-op when there is no
// config to clear; restoreCopilotTokens handles the missing-file case itself by
// starting from an empty map.
func readCopilotConfig(path string) (map[string]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cleaned []byte
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		cleaned = append(cleaned, []byte(line+"\n")...)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(cleaned, &cfg); err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	return cfg, nil
}

// writeCopilotConfig marshals cfg back to config.json with the CLI's // header
// preserved and the CLI-expected mode. Written via a temp file + rename so a
// concurrent CLI read never sees a half-written file.
func writeCopilotConfig(path string, cfg map[string]interface{}) error {
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	content := copilotConfigHeader + string(out)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), sharedConfigDesiredMode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// clearExpiredTokens removes stored copilot tokens from config.json.
// An expired gho_ token causes copilot to hang during auth through the
// MITM proxy instead of falling through to the /login prompt.
//
// The login IDENTITY (loggedInUsers / lastLoggedInUser) is deliberately
// PRESERVED: an expired token does not change who was logged in, and the
// interactive CLI refuses to consider itself signed in without an identity —
// a later restoreCopilotTokens seed of a perfectly valid token still showed
// "Please use /login" because this function had wiped the identity alongside
// the token (hivecommons/hive, 2026-08-22).
func clearExpiredTokens() error {
	cfg, err := readCopilotConfig(sharedCopilotConfigPath)
	if err != nil {
		return err
	}
	cfg["copilotTokens"] = map[string]interface{}{}
	return writeCopilotConfig(sharedCopilotConfigPath, cfg)
}

// restoreCopilotTokens writes token into config.json's copilotTokens map so the
// Copilot CLI has a usable credential without an interactive /login.
//
// This closes the loop that leaves agents stuck at "Please use /login" while a
// VALID user token exists: clearExpiredTokens (and a config rewrite on roll)
// leave copilotTokens EMPTY, and CLI 1.0.78 does NOT re-populate it from the
// injected COPILOT_GITHUB_TOKEN on its own — it just prompts /login. Seeding
// copilotTokens from the durable user token gives the CLI the credential it
// would otherwise wait for a human to supply. It never performs a device-flow
// login (that stays the operator's manual path); it only re-uses a token the
// operator already provided.
//
// The token is stored under the "github.com" host key in the object shape the
// CLI reads ({"github.com":{"token":"…"}}) — the same shape the credential
// reader (copilotCredentialFileHasTokens) already accepts. A blank token is a
// no-op (nothing to restore); use clearExpiredTokens to empty the map.
func restoreCopilotTokens(path, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	cfg, err := readCopilotConfig(path)
	if err != nil {
		// A missing/unreadable config is not fatal here: we are writing the
		// token store fresh. A malformed-but-present file returns a non-IsNot‐
		// Exist error we still honor rather than clobbering unknown content.
		if !os.IsNotExist(err) {
			return err
		}
		cfg = map[string]interface{}{}
	}
	// When the config still carries a VALID login identity (preserved by
	// clearExpiredTokens), store the token under the "<host>:<login>" key a
	// real /login writes, so the interactive CLI recognizes the seeded
	// credential as a signed-in session rather than showing "Please use
	// /login" over a valid token. The CLI has written lastLoggedInUser in two
	// shapes across versions — a bare "https://github.com:user" string and a
	// {"host":…,"login":…} object (the shape observed in a working 1.0.78
	// config) — accept both.
	//
	// With no valid identity on file (missing, or junk inherited from the
	// shared config's polluted lineage), resolve the token's TRUE owner from
	// the GitHub API and write the full canonical identity. This makes the
	// seed self-sufficient: whatever garbage the file has decayed into, a
	// valid token always produces a signed-in config. Only when the lookup
	// itself fails (offline, revoked token) fall back to the legacy host-keyed
	// object shape — no worse than before.
	if key := copilotIdentityKey(cfg["lastLoggedInUser"]); key != "" {
		cfg["copilotTokens"] = map[string]interface{}{key: token}
	} else if login := githubTokenLogin(token); login != "" {
		identity := map[string]interface{}{"host": "https://github.com", "login": login}
		cfg["copilotTokens"] = map[string]interface{}{"https://github.com:" + login: token}
		cfg["lastLoggedInUser"] = identity
		cfg["loggedInUsers"] = []interface{}{identity}
	} else {
		cfg["copilotTokens"] = map[string]interface{}{
			"github.com": map[string]interface{}{"token": token},
		}
	}
	return writeCopilotConfig(path, cfg)
}

// replaceCopilotTokens makes token the sole active credential in config.json.
// Unlike restoreCopilotTokens, it deliberately does not preserve the previous
// lastLoggedInUser: this path is used when an explicit environment token or a
// newly completed dashboard login must replace a stale shared CLI account.
func replaceCopilotTokens(path, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	cfg, err := readCopilotConfig(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		cfg = map[string]interface{}{}
	}
	if login := githubTokenLogin(token); login != "" {
		identity := map[string]interface{}{"host": "https://github.com", "login": login}
		cfg["copilotTokens"] = map[string]interface{}{"https://github.com:" + login: token}
		cfg["lastLoggedInUser"] = identity
		cfg["loggedInUsers"] = []interface{}{identity}
	} else {
		cfg["copilotTokens"] = map[string]interface{}{
			"github.com": map[string]interface{}{"token": token},
		}
		delete(cfg, "lastLoggedInUser")
		delete(cfg, "loggedInUsers")
	}
	return writeCopilotConfig(path, cfg)
}

// GitHubTokenLogin resolves the GitHub login that owns token via GET /user, or
// "" on any failure. Exported for the dashboard's model-discovery notice,
// which names the account behind a rejected Copilot credential (#7302).
func GitHubTokenLogin(token string) string {
	return githubTokenLogin(token)
}

// githubTokenLogin resolves the GitHub login that owns token via GET /user, or
// "" on any failure. Short-timeout, one call — used only on the rare seed path
// where the config lacks a valid identity and on the model-discovery rejection
// path. Overridable in tests.
var githubTokenLogin = func(token string) string {
	req, err := http.NewRequest("GET", "https://api.github.com/user", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		Login string `json:"login"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return ""
	}
	return strings.TrimSpace(body.Login)
}

// copilotIdentityKey renders a lastLoggedInUser value — string or
// {"host","login"} object — as the "<host>:<login>" copilotTokens key the CLI
// uses for a signed-in session, or "" when there is no usable identity.
//
// VALIDATION is the point, not just shape conversion: the shared config's
// lineage accumulates junk identities (a bare "github.com" string was observed
// live — hivecommons/hive, 2026-08-22 — inherited from stale rewrites), and a
// junk identity keyed a seeded VALID token under a key the CLI rejects, leaving
// every agent at "Please use /login" over working credentials. Only a
// "https://<host>:<login>" string (scheme + host + login = at least two
// colons) or a {host,login} object whose host looks like a URL qualifies.
func copilotIdentityKey(v interface{}) string {
	switch id := v.(type) {
	case string:
		s := strings.TrimSpace(id)
		if strings.HasPrefix(s, "http") && strings.Count(s, ":") >= 2 {
			return s
		}
	case map[string]interface{}:
		host, _ := id["host"].(string)
		login, _ := id["login"].(string)
		if strings.HasPrefix(strings.TrimSpace(host), "http") && strings.TrimSpace(login) != "" {
			return host + ":" + login
		}
	}
	return ""
}

// extractCopilotToken returns the active usable token from config.json. The
// Copilot CLI stores entries in two shapes across versions/login routes — a
// bare string ({"host:user":"gho_…"}) and an object
// ({"github.com":{"token":"gho_…"}}) — and this accepts both.
//
// A valid lastLoggedInUser is authoritative. Without one, a legacy config is
// accepted only when it contains exactly one distinct usable token. Refusing
// an ambiguous multi-account map prevents Go's randomized map iteration from
// promoting an unrelated (and possibly unlicensed) account fleet-wide.
func extractCopilotToken(path string) string {
	cfg, err := readCopilotConfig(path)
	if err != nil {
		return ""
	}
	tokens, ok := cfg["copilotTokens"].(map[string]interface{})
	if !ok {
		return ""
	}
	if key := copilotIdentityKey(cfg["lastLoggedInUser"]); key != "" {
		return copilotTokenFromValue(tokens[key])
	}
	var found string
	for _, v := range tokens {
		token := copilotTokenFromValue(v)
		if token == "" {
			continue
		}
		if found != "" && token != found {
			return ""
		}
		found = token
	}
	return found
}

func copilotTokenFromValue(v interface{}) string {
	var token string
	switch t := v.(type) {
	case string:
		token = t
	case map[string]interface{}:
		token, _ = t["token"].(string)
	}
	token = strings.TrimSpace(token)
	if !copilotTokenValueUsable(token) {
		return ""
	}
	return token
}

// copilotTokenValueUsable reports whether a copilotTokens value is a real
// credential. The CLI redacts tokens it has rejected by rewriting them as a
// run of asterisks ("******"); treating that placeholder as a token let the
// promote path mirror garbage over the durable user token.
func copilotTokenValueUsable(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return strings.ContainsFunc(s, func(r rune) bool { return r != '*' })
}

// writeDurableCopilotToken persists token to durablePath via a temp file +
// rename, matching the dashboard login's saveCopilotToken write. The production
// caller passes CopilotUserTokenPath — the file that survives upgrade rolls and
// that the hive reads at boot into m.copilotAuthToken; the path is a parameter
// so tests can target a temp file.
func writeDurableCopilotToken(durablePath, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	tmp := durablePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(token), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, durablePath)
}
