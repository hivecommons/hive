// Copilot/Claude credential file plumbing: shared config token probes,
// durable copilot user token restore/promote, identity keying, and the
// copilot login diagnostic.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
)

// sharedCopilotConfigPath and sharedClaudeCredentialPath are vars (not consts)
// solely so tests can redirect them to temp files and exercise the config/token
// helpers (copilotConfigHasTokens, clearExpiredTokens, configHasTokens,
// fixSharedConfigPerms) without a real /data volume. Production values are
// unchanged; nothing on the launch path mutates them.
var (
	sharedCopilotConfigPath    = "/data/home/.copilot/config.json"
	sharedClaudeCredentialPath = "/data/home/.claude/.credentials.json"
)

const (
	sharedConfigDesiredMode = 0o660
	// agyDefaultEffort is the reasoning effort passed alongside agy's --model.
	// agy requires --effort whenever --model is given and otherwise ignores the
	// model entirely; "low" is the effort agy defaults to on its own, so this
	// makes the configured model take effect without changing behaviour. Hive
	// has no per-agent reasoning-effort setting yet — when it grows one, this
	// is the constant it should replace.
	agyDefaultEffort = "low"

	tokenRestartCooldownSec = 60 // minimum seconds between token-triggered restarts per agent
	// loginPromptTailLines bounds the pane region the login-prompt detector
	// reads: a prompt the CLI is stuck at sits at the pane bottom, while
	// echoed kick text and startup flashes live in scrollback (see the poller).
	loginPromptTailLines = 15
	// loginStreakRestartMin is how many consecutive polls (~3s apart) must see
	// the login prompt before a token-triggered restart may fire — filters the
	// CLI's transient startup "/login" flash.
	loginStreakRestartMin = 3
	// tokenRestartMaxAttempts bounds CONSECUTIVE token-triggered restarts that
	// fail to clear the login prompt.
	//
	// The three guards above answer WHEN to restart; none of them answered HOW
	// MANY TIMES, so a restart that could never work was retried forever at the
	// cooldown interval. #4596 is precisely that shape: the shared credential is
	// valid (so configHasTokens() is true) while $HOME/.claude.json has lost its
	// oauthAccount (so the CLI shows the login menu regardless), and each
	// restart re-launched a CLI that rewrote the same contended file and asked
	// again. Restarts are not free — they destroy in-flight work, which is the
	// failure the kick grace above was added for.
	//
	// Three is deliberately generous: one restart genuinely does fix the case
	// this feature was built for (an operator authenticates in one agent's
	// terminal and the others need a nudge), so the cap only engages on a
	// theory that has now failed repeatedly.
	tokenRestartMaxAttempts = 3
	// tokenRestartKickGrace suppresses token-triggered restarts after a kick
	// delivery so the restart can never destroy just-delivered work.
	tokenRestartKickGrace      = 10 * time.Minute
	expiredTokenHangTimeoutSec = 180 // blank pane after this many seconds triggers token purge + restart
	tlsErrorRestartCooldownSec = 120 // minimum seconds between TLS-error-triggered restarts per agent
)

// CopilotUserTokenPath is where the dashboard's device-flow login persists
// the Copilot OAuth token; injected into agents as COPILOT_GITHUB_TOKEN.
const CopilotUserTokenPath = "/data/copilot-user-token"

var copilotUserTokenWatchPath = CopilotUserTokenPath

// copilotUserTokenProbePath is the same location as consulted by the
// AgentAuthState file probe. A var (not the const directly) purely as a TEST
// SEAM, matching sharedCopilotConfigPath above: on a live hive host the real
// /data/copilot-user-token exists, and a probe that cannot be redirected makes
// every "no copilot credentials" test assert against production login state.
var copilotUserTokenProbePath = CopilotUserTokenPath

// claudeCredentialReachable reports whether a usable Claude credential exists
// at the locations this agent's CLI will look — its per-UID home first, then
// the shared path its ~/.claude symlink resolves to.
//
// It is a REACHABILITY check, not a permission check, and the distinction is
// worth stating: this runs in the hive process, so it proves the file is there
// and parseable, not that the agent's UID can open it. The deployment keeps
// those the same — the entrypoint's inotify guard chowns /data/home/.claude to
// dev:node and holds it group-readable on every write, precisely so every
// agent UID can read it (#4619). If that ever drifts, an agent lands at a login
// prompt with no injected token instead of a working one; that is a loud,
// alerting state, not a silent one, which is the right direction to fail in.
//
// HasUsableToken, not HasValidToken: an access token that has aged out is
// exactly the case the CLI fixes for itself on start, by redeeming the refresh
// grant beside it. Treating that state as "no credential here" would re-inject
// the static override precisely when the CLI was about to recover, which is the
// failure this guard exists to prevent.
func claudeCredentialReachable(agent *AgentProcess, backend string) bool {
	if agent == nil {
		return false
	}
	for _, p := range agentClaudeCredentialPaths(agent.Name, agent.UID, backend) {
		if claude.HasUsableToken(p) {
			return true
		}
	}
	return false
}

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
// the token (kubestellar/hive, 2026-08-22).
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

// githubTokenLogin resolves the GitHub login that owns token via GET /user, or
// "" on any failure. Short-timeout, one call — used only on the rare seed path
// where the config lacks a valid identity. Overridable in tests.
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
// live — kubestellar/hive, 2026-08-22 — inherited from stale rewrites), and a
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

// extractCopilotToken pulls the first usable token string out of a config.json
// copilotTokens map, or "" when there is none. The Copilot CLI stores entries
// in two shapes across versions/login routes — a bare string
// ({"host:user":"gho_…"}) and an object ({"github.com":{"token":"gho_…"}}) —
// and this accepts both. It is the inverse of restoreCopilotTokens: the reader
// side of promoting a CLI-written token back to the hive's durable store.
func extractCopilotToken(path string) string {
	cfg, err := readCopilotConfig(path)
	if err != nil {
		return ""
	}
	tokens, ok := cfg["copilotTokens"].(map[string]interface{})
	if !ok {
		return ""
	}
	for _, v := range tokens {
		switch t := v.(type) {
		case string:
			if s := strings.TrimSpace(t); copilotTokenValueUsable(s) {
				return s
			}
		case map[string]interface{}:
			if s, ok := t["token"].(string); ok {
				if s = strings.TrimSpace(s); copilotTokenValueUsable(s) {
					return s
				}
			}
		}
	}
	return ""
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

// Diagnostic pacing. Vars, not consts, so the pkg/agent TestMain can shrink
// them (see the pacing block near deliverStartupKick). Production values
// unchanged.
var (
	diagnosticTimeoutSec = 20
	diagnosticPollSec    = 2
)

// authErrorPatterns indicate the stored token was DEFINITIVELY rejected by the
// server and should be cleared. These are server-side rejections, not CLI
// prompts. A bare interactive login/"re-authenticate" prompt is intentionally
// NOT here: on a slow cold start after an upgrade the Copilot CLI can surface a
// login/device-flow prompt while the token on disk is still valid, and clearing
// it there destroys a good token and forces the user to re-login on every
// upgrade. Only a genuine credential rejection purges the token.
var authErrorPatterns = []string{
	"Bad credentials",
	"401 Unauthorized",
	"token found but could not be validated",
	"Failed to fetch OAuth user login",
}

// matchesAuthError reports whether copilot diagnostic output shows a definitive
// server-side credential rejection that justifies purging the stored token. A
// bare login/"re-authenticate" prompt does NOT match — that is handled by
// paneShowsLoginPrompt (a non-destructive "needs login" UI signal) so a slow
// cold start after an upgrade cannot destroy a still-valid token.
func matchesAuthError(output string) bool {
	for _, pat := range authErrorPatterns {
		if strings.Contains(output, pat) {
			return true
		}
	}
	return false
}

func (m *Manager) runCopilotDiagnostic(ctx context.Context, agent *AgentProcess) {
	m.tmuxSendKeysForAgent(agent, "C-c", "")
	time.Sleep(paneCaptureSleep)
	// Only sweep by UID when isolation gave this agent a real per-agent UID.
	// agent.UID==0 (isolation off or agent missing from the UID map) would
	// otherwise ask killAgentProcesses to match root — the internal floor guard
	// blocks it, but skipping the call makes the intent explicit.
	if agent.UID > 0 {
		killAgentProcesses(agent.UID, m.logger)
	}
	_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()

	if err := m.ensureTmuxSession(agent); err != nil {
		m.logger.Warn("diagnostic: failed to create tmux session", "agent", agent.Name, "error", err)
		return
	}

	binary, err := backendBinary("copilot")
	if err != nil {
		m.logger.Warn("diagnostic: copilot binary not found", "error", err)
		return
	}
	m.tmuxSendLiteralForAgent(agent, fmt.Sprintf("HOME=/data/home %s", binary))
	time.Sleep(textToEnterDelay)
	m.tmuxSendEntersForAgent(agent)

	m.logger.Info("diagnostic: launched bare copilot to capture error", "agent", agent.Name)

	deadline := time.After(time.Duration(diagnosticTimeoutSec) * time.Second)
	ticker := time.NewTicker(time.Duration(diagnosticPollSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			m.logger.Warn("diagnostic: timed out waiting for copilot error output", "agent", agent.Name)
			agent.LastError = "copilot hung with no output (diagnostic timed out)"
			agent.State = StateFailed
			m.audit(AuditAgentStartFailed, agent.Name, auditFields(
				"outcome", "failure",
				"backend", agent.effectiveBackend(),
				"model", agent.effectiveModel(),
				"error", agent.LastError,
			))
			return
		case <-ticker.C:
			output := m.captureTmuxPaneForAgent(agent)
			if output == "" {
				continue
			}
			if matchesAuthError(output) {
				agent.LastError = "auth token expired or invalid"
				// Prefer to RESTORE the stored token over merely clearing it: an
				// empty copilotTokens leaves CLI 1.0.78 stuck at /login (it does
				// not re-populate from the injected env token), and every roll
				// re-hits this. If we hold a durable user token, seed it so the
				// relaunch below comes up authenticated; otherwise fall back to
				// the historical clear (which lets the CLI reach /login instead
				// of hanging on the MITM proxy on a stale token).
				m.mu.RLock()
				userTok := m.copilotAuthToken
				m.mu.RUnlock()
				if strings.TrimSpace(userTok) != "" {
					m.logger.Warn("diagnostic: auth error detected, restoring token from durable user token",
						"agent", agent.Name, "output_snippet", truncateStr(output, 200))
					if err := restoreCopilotTokens(sharedCopilotConfigPath, userTok); err != nil {
						m.logger.Warn("diagnostic: failed to restore tokens, clearing instead", "error", err)
						_ = clearExpiredTokens()
					}
				} else {
					m.logger.Warn("diagnostic: auth error detected, clearing token (no durable token to restore)",
						"agent", agent.Name, "output_snippet", truncateStr(output, 200))
					if err := clearExpiredTokens(); err != nil {
						m.logger.Warn("diagnostic: failed to clear tokens", "error", err)
					}
				}
			} else if paneShowsCLIReady(strings.Split(output, "\n")) {
				m.logger.Info("diagnostic: copilot started successfully in bare mode", "agent", agent.Name)
				agent.LastError = ""
			} else {
				continue
			}

			if agent.UID > 0 {
				killAgentProcesses(agent.UID, m.logger)
			}
			_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()
			agent.forceRelaunch = true
			if err := m.Restart(ctx, agent.Name); err != nil {
				m.logger.Warn("diagnostic: restart failed", "agent", agent.Name, "error", err)
			}
			return
		}
	}
}

// fixSharedConfigPerms ensures /data/home/.copilot/config.json is group-readable
// before launching an agent. Copilot CLI rewrites this file with 600 perms on
// token refresh, locking out other agent UIDs that share the same HOME.
func (m *Manager) fixSharedConfigPerms(agent *AgentProcess) {
	info, err := os.Stat(sharedCopilotConfigPath)
	if err != nil {
		return
	}
	if info.Mode().Perm() == sharedConfigDesiredMode {
		return
	}
	m.logger.Warn("fixing shared config.json perms before launch",
		"agent", agent.Name,
		"was", fmt.Sprintf("%04o", info.Mode().Perm()),
		"fix", fmt.Sprintf("%04o", sharedConfigDesiredMode))
	if err := os.Chmod(sharedCopilotConfigPath, sharedConfigDesiredMode); err != nil {
		m.logger.Warn("failed to fix config.json perms", "error", err)
	}
}
