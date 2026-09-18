package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// AppTokenMinter is implemented by github.AppAuth to mint per-agent scoped tokens.
type AppTokenMinter interface {
	WriteAgentToken(ctx context.Context, agentName, tier string, agentUID int) error
}

// AgentMintIssuer is implemented by mint.AgentMinter. It is the OPT-IN,
// ADDITIONAL short-lived-credential path: when the mint is enabled it issues a
// scoped OIDC/JWT for an agent (subject=agent, scopes from its tier) that the
// agent can present ALONGSIDE its GitHub App token to a WIF broker. It never
// replaces WriteAgentToken. Enabled() reports false (and MintAgentToken returns
// "" with no error) when the mint is off, so wiring stays a strict no-op by
// default.
type AgentMintIssuer interface {
	Enabled() bool
	MintAgentToken(agentName, tier string) (string, error)
}

// ReloadClaudeToken re-reads the Claude credentials file and updates the
// cached token. Called by the dashboard after a successful OAuth login.
func (m *Manager) ReloadClaudeToken() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claudeAuthToken = claude.ReadAccessToken(claude.CredentialsPath)
}

// Copilot token sources, as reported by CopilotTokenSource. Operator-facing
// phrases: they are spliced verbatim into the model-picker notice and the
// "rejected by upstream" log line, so each one has to read as an answer to
// "which credential did GitHub just reject?".
const (
	// CopilotTokenSourceEnv: the COPILOT_GITHUB_TOKEN the hive process was
	// started with (a provisioned/machine token, typically).
	CopilotTokenSourceEnv = "the COPILOT_GITHUB_TOKEN environment variable"
	// CopilotTokenSourceDashboardLogin: a device-flow login completed on the
	// dashboard during this process's lifetime.
	CopilotTokenSourceDashboardLogin = "the dashboard Copilot login"
	// CopilotTokenSourceDurableFile: a dashboard login persisted by an earlier
	// hive process and read back at startup.
	CopilotTokenSourceDurableFile = "a dashboard Copilot login persisted at " + CopilotUserTokenPath
	// CopilotTokenSourceCLIConfig: an in-agent `/login` the reconciler promoted
	// out of the shared Copilot CLI config.
	CopilotTokenSourceCLIConfig = "an in-agent /login promoted from the shared Copilot CLI config"
)

// SetCopilotToken updates the cached Copilot token injected into agent
// environments as COPILOT_GITHUB_TOKEN. A caller setting the token explicitly
// makes it authoritative over an older token left in the shared CLI config.
func (m *Manager) SetCopilotToken(token string) {
	m.installCopilotToken(token, true, CopilotTokenSourceDashboardLogin)
}

// installCopilotToken is setCopilotToken plus the fleet-side half of a token
// change: a new credential that only ever reaches memory and the on-disk
// stores is invisible to the agents that are already running on the old one.
// See propagateCopilotToken.
func (m *Manager) installCopilotToken(token string, authoritative bool, source string) {
	if m.setCopilotToken(token, authoritative, source) {
		m.propagateCopilotToken()
	}
}

// setCopilotToken caches token and reports whether the cached VALUE changed.
// The bool is what gates propagateCopilotToken: re-asserting the same token
// (the common reconciler outcome) must not churn agent sessions.
func (m *Manager) setCopilotToken(token string, authoritative bool, source string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := strings.TrimSpace(token) != strings.TrimSpace(m.copilotAuthToken)
	// A newly-installed token — whether a dashboard re-login, an env var, or
	// a promoted in-agent /login — represents fresh operator intent, so any
	// prior "authoritative token rejected by Copilot" latch must be cleared.
	// Without this, the recovery path #6767 depends on would only fire once.
	if changed {
		m.copilotAuthTokenRejected = false
	}
	m.copilotAuthToken = token
	// No token, no source: a logout must not leave the picker blaming "the
	// dashboard Copilot login" for a credential that no longer exists.
	if strings.TrimSpace(token) == "" {
		source = ""
	}
	m.copilotAuthTokenSource = source
	// INVARIANT: an EMPTY token is never authoritative (#6500).
	//
	// "Authoritative" means "prefer this over whatever identity the shared CLI
	// config holds". That claim is meaningless without a token to prefer, and
	// asserting it for "" is actively harmful: syncCopilotToken's SEED branch
	// returns noop when authoritative && held == "", which permanently disables
	// the PROMOTE direction. An operator who logs out via the dashboard and then
	// runs /login inside an agent would have that login never mirrored to the
	// durable store, so it would be lost on the next roll — exactly the gap
	// PROMOTE was added to close.
	//
	// Enforced here rather than at the call site so no present or future caller
	// can reintroduce the state. m.copilotAuthTokenAuthoritative is only ever
	// true alongside a non-empty token.
	m.copilotAuthTokenAuthoritative = authoritative && strings.TrimSpace(token) != ""
	return changed
}

// ActivateCopilotToken makes token the active shared CLI identity as well as
// the token injected into agent environments. The in-memory update happens
// even if the config write fails, so the periodic reconciler retries in the
// authoritative direction instead of restoring the superseded CLI token.
func (m *Manager) ActivateCopilotToken(token string) error {
	err := replaceCopilotTokens(sharedCopilotConfigPath, token)
	m.installCopilotToken(token, true, CopilotTokenSourceDashboardLogin)
	return err
}

// copilotTokenEnvVar carries the Copilot OAuth credential into an agent
// session. It is the Copilot CLI's HIGHEST-precedence credential — it outranks
// whatever identity sits in the shared config.json — which is why a stale
// value here can defeat a perfectly good recovery /login (see
// propagateCopilotToken).
const copilotTokenEnvVar = "COPILOT_GITHUB_TOKEN"

// copilotTokenRefreshTmuxArgs returns the ONE tmux invocation that puts the
// hive's current Copilot token into session's environment.
//
// -r, not -u, for the empty case: COPILOT_GITHUB_TOKEN is also in the hive
// PROCESS environment on any hive started with one (NewManager reads it), so
// the tmux server holds it globally. -u deletes only the SESSION entry and the
// global value shows straight through to the next pane, which would mean a
// dashboard logout silently keeps authenticating every later CLI as the
// account the operator just logged out of. -r records a removal that hides the
// global value from everything the server forks afterwards, and a later plain
// set overrides it. Same reasoning — and the same tmux 3.5a behaviour — as
// inheritedCredentialEnvVars in applySessionEnv.
func copilotTokenRefreshTmuxArgs(session, token string) []string {
	if strings.TrimSpace(token) == "" {
		return []string{"set-environment", "-t", session, "-r", copilotTokenEnvVar}
	}
	return []string{"set-environment", "-t", session, copilotTokenEnvVar, token}
}

// copilotTokenPropagation is one agent's share of the work a Copilot token
// change implies: the session-environment push, and whether this agent's LIVE
// CLI has to be relaunched to pick the new credential up.
type copilotTokenPropagation struct {
	Name     string
	Agent    *AgentProcess
	TmuxArgs []string
	Relaunch bool
	// BackendAuth is the verdict that justified Relaunch, snapshotted under
	// m.mu so the logging below never reads AgentProcess state unlocked.
	BackendAuth string
}

// copilotSessionCarriesRejectedToken reports whether an agent's last observed
// backend-auth verdict means the CLI running in its pane RIGHT NOW is using a
// credential Copilot has already refused. The hard-auth verdicts count:
// unlicensed, token-expired, and a bare 403 forbidden (cause undetermined but
// still an upstream rejection a token swap can plausibly clear). Quota and
// unreachable are not credential problems, and swapping the token cannot help
// them.
func copilotSessionCarriesRejectedToken(a *AgentProcess) bool {
	if a == nil || a.Config.Backend != "copilot" {
		return false
	}
	switch a.BackendAuth.Status {
	case BackendAuthUnlicensed, BackendAuthTokenExpired, BackendAuthForbidden:
		return true
	default:
		return false
	}
}

// planCopilotTokenPropagationLocked computes what a just-changed Copilot token
// implies for each agent. Split out from propagateCopilotToken so the rule is
// unit-testable without a tmux server, the same way linearRefreshTmuxArgs and
// decideTokenRestart are. Caller must hold m.mu (read is enough).
func (m *Manager) planCopilotTokenPropagationLocked() []copilotTokenPropagation {
	token := m.copilotAuthToken
	// An EMPTY token can un-stick nobody: a relaunch would only bring the CLI
	// back with no credential at all. The env removal below still runs, so the
	// logout is not silently undone on the next pane.
	canRelaunch := strings.TrimSpace(token) != ""

	out := make([]copilotTokenPropagation, 0, len(m.agents))
	for name, a := range m.agents {
		if a == nil || a.Config.Backend != "copilot" || a.tmuxSession == "" {
			continue
		}
		out = append(out, copilotTokenPropagation{
			Name:        name,
			Agent:       a,
			TmuxArgs:    copilotTokenRefreshTmuxArgs(a.tmuxSession, token),
			Relaunch:    canRelaunch && a.State == StateRunning && copilotSessionCarriesRejectedToken(a),
			BackendAuth: a.BackendAuth.Status,
		})
	}
	return out
}

// propagateCopilotToken pushes a just-installed Copilot token out to the
// agents (#6500).
//
// The gap this closes: every earlier fix on this issue reconciled the two
// STORES — the shared CLI config.json and the hive's durable token file — and
// stopped there. Neither store is what an already-running agent is
// authenticating with. COPILOT_GITHUB_TOKEN is injected into each session once,
// when the session is created, and the Copilot CLI reads it at process start;
// it also outranks config.json. So an operator who does the documented recovery
// — /login inside an agent terminal with a licensed identity — gets the token
// promoted to the durable store and gets nothing else: every CLI in the fleet,
// including the one they just logged in from, keeps presenting the token
// GitHub already rejected and keeps printing "You are not licensed to use
// Copilot". The existing auto-restart detector does not cover it either,
// because that one is gated on the pane showing a /login PROMPT, and an
// unlicensed pane shows a licence error instead.
//
// So: push the new value into every copilot agent's session environment (so
// nothing forked later inherits the superseded one), and relaunch exactly the
// agents whose pane already carries a rejected-credential verdict. Healthy
// agents are deliberately left alone — their next ordinary relaunch picks the
// new token up from applySecretEnv, and a token rotation is no reason to
// destroy work in flight.
//
// Runs only on a CHANGE of value (see installCopilotToken), so it cannot loop:
// after the change the reconciler's next tick is a no-op.
func (m *Manager) propagateCopilotToken() {
	m.mu.RLock()
	plan := m.planCopilotTokenPropagationLocked()
	m.mu.RUnlock()

	for _, p := range plan {
		// Failures are ignored for the same reason applySecretEnv ignores
		// them: a missing session is the launch path's problem, not ours.
		_ = m.tmuxCmd(p.Agent, p.TmuxArgs...).Run()
		if !p.Relaunch {
			continue
		}
		m.logger.Info("relaunching copilot agent onto the refreshed token",
			"agent", p.Name, "backend_auth", p.BackendAuth)
		go func(name string) {
			// context.Background(), not a caller context: this runs from the
			// session reconciler and from dashboard HTTP handlers, and a
			// relaunch must outlive the request that triggered it.
			if err := m.RestartWithReason(context.Background(), name, "copilot token refreshed"); err != nil {
				m.logger.Warn("copilot token relaunch failed", "agent", name, "error", err)
			}
		}(p.Name)
	}
}

// CopilotToken returns the cached Copilot (GitHub OAuth) token, or "" if
// none is set. Used by the dashboard's model-discovery probe to authenticate
// against the Copilot models endpoint. The value is a secret and must never
// be logged.
func (m *Manager) CopilotToken() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.copilotAuthToken
}

// CopilotTokenSource names where the cached Copilot token came from (one of
// the CopilotTokenSource* phrases), or "" when no token is held. Safe to
// display and log: it says which credential hive is presenting, never the
// credential itself.
func (m *Manager) CopilotTokenSource() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if strings.TrimSpace(m.copilotAuthToken) == "" {
		return ""
	}
	return m.copilotAuthTokenSource
}

// BackendAuthAvailable reports whether shared credentials exist for a CLI
// backend, so the dashboard can show honest auth state even for agents with
// no running pane (e.g. on-demand agents that never launched). Claude checks
// the credentials file (a live access token, or an expired one whose refresh
// grant is still good — see claude.HasUsableToken); Copilot checks the cached
// token. For backends we cannot introspect it returns (false, false) = unknown.
func (m *Manager) BackendAuthAvailable(backend string) (available, known bool) {
	switch backend {
	case "claude":
		return claude.HasUsableToken(claude.CredentialsPath), true
	case "copilot":
		m.mu.RLock()
		tok := m.copilotAuthToken
		m.mu.RUnlock()
		if tok != "" {
			return true, true
		}
		return configHasTokens(), true
	case bobBackend:
		// bob has exactly one usable credential in a pod: the API key. Its
		// presence is therefore a complete answer, so report it as known.
		return m.bobAPIKey() != "", true
	default:
		return false, false
	}
}

// SetBobAPIKeyResolver injects the resolver for the IBM bobshell API key.
// Called from main.go with a closure over the live config, so a key added
// after boot is picked up on the next agent launch. The resolver must return
// the key VALUE or "" — the value is never logged.
func (m *Manager) SetBobAPIKeyResolver(fn func() string) {
	// Atomic store — no m.mu — so bobAPIKey can be read lock-free from the
	// lock-holding launch path (see bobAPIKeyResolver docs).
	m.bobAPIKeyResolver.Store(&fn)
}

// bobAPIKey returns the configured bobshell API key, or "" when none is
// configured (or no resolver was injected, as in tests/bare setups).
// Safe to call while holding m.mu.
func (m *Manager) bobAPIKey() string {
	fnp := m.bobAPIKeyResolver.Load()
	if fnp == nil || *fnp == nil {
		return ""
	}
	return (*fnp)()
}

// LinearCredential is what an agent authenticates to api.linear.app with.
// Exactly one of the two is expected to be set: AccessToken is the installed
// Linear agent app's OAuth token (sent as `Authorization: Bearer …`, so writes
// are authored by the app identity — the parity of GitHub's App bot); APIKey
// is the work-source personal API key (sent bare as `Authorization: …`), the
// fallback when no workspace is connected. Empty means "inject nothing".
type LinearCredential struct {
	AccessToken string
	APIKey      string
}

// Empty reports whether no credential is available.
func (c LinearCredential) Empty() bool { return c.AccessToken == "" && c.APIKey == "" }

// SetLinearCredentialResolver injects the resolver for the Linear write
// credential. Called from main.go with a closure over the live Linear agent
// install + config, so a workspace connected after boot is picked up on the
// next launch or hourly refresh. The resolver returns credential VALUES,
// which are never logged. A nil fn clears it (no Linear credential injected).
func (m *Manager) SetLinearCredentialResolver(fn func() LinearCredential) {
	// Atomic store — no m.mu — so linearCredential can be read lock-free from
	// the lock-holding launch path (see bobAPIKeyResolver docs).
	if fn == nil {
		m.linearCredentialResolver.Store(nil)
		return
	}
	m.linearCredentialResolver.Store(&fn)
}

// linearCredential returns the current Linear credential, or the zero value
// when none is configured (or no resolver was injected, as in tests/bare
// setups). Safe to call while holding m.mu.
func (m *Manager) linearCredential() LinearCredential {
	fnp := m.linearCredentialResolver.Load()
	if fnp == nil || *fnp == nil {
		return LinearCredential{}
	}
	return (*fnp)()
}

// linearEnvPairs renders the Linear credential as env pairs for one agent, or
// nil when the agent may not hold one. Gated on CanCreateIssues() — the
// ISSUES_ONLY floor — because that is the tier at which the proxy's Linear
// gate (pkg/proxy/linear_rules.go) first permits a mutation: below it the
// credential could only ever be used for reads the hive already performs on
// the agent's behalf, and GitHub parity says advisory agents stay token-less.
// Both values are Secret so they never reach the shell command line.
func (m *Manager) linearEnvPairs(agent *AgentProcess) []agentEnvPair {
	if !m.agentMode(agent).CanCreateIssues() {
		return nil
	}

	cred := m.linearCredential()
	switch {
	case cred.AccessToken != "":
		return []agentEnvPair{{linearAccessTokenEnvVar, cred.AccessToken, true}}
	case cred.APIKey != "":
		return []agentEnvPair{{linearAPIKeyEnvVar, cred.APIKey, true}}
	}
	return nil
}

// linearRefreshTmuxArgs returns the tmux invocations the refresh tick applies
// to one agent's session for the Linear credential: set-environment pushes of
// the current credential for ISSUES_ONLY+ agents, or explicit unsets ("-u") of
// BOTH credential vars for agents below the floor. The unsets exist because
// the creation-time strip in ensureTmuxSession fires only when the session is
// created (it early-returns on an existing session): a live agent downgraded
// below ISSUES_ONLY keeps its session, and tmux forks a fresh CLI per turn, so
// without them every post-downgrade turn would inherit the last pushed value
// indefinitely — LINEAR_API_KEY never expires. They are unconditional on prior
// state (set-environment -r is idempotent) so a downgrade is closed on the
// first tick regardless of which variable, if any, was pushed before.
//
// -r, not -u: -u only deletes the SESSION entry, and when the variable also
// sits in the server's GLOBAL environment (inherited from the hive process —
// LINEAR_API_KEY is exactly such a variable) the global value shows through
// to the next pane. -r records a removal that hides the global value from
// every process the server forks afterwards; a later plain set overrides it,
// so the ISSUES_ONLY+ push above is unaffected. See applySessionEnv.
// Callers must skip agents with no tmux session.
func (m *Manager) linearRefreshTmuxArgs(a *AgentProcess) [][]string {
	if !m.agentMode(a).CanCreateIssues() {
		return [][]string{
			{"set-environment", "-t", a.tmuxSession, "-r", linearAccessTokenEnvVar},
			{"set-environment", "-t", a.tmuxSession, "-r", linearAPIKeyEnvVar},
		}
	}
	var out [][]string
	for _, p := range m.linearEnvPairs(a) {
		out = append(out, []string{"set-environment", "-t", a.tmuxSession, p.Key, p.Value})
	}
	return out
}

// Environment variables carrying the Linear credential into an agent session.
// LINEAR_API_KEY is the name Linear's own SDK, MCP server, and CLI read for a
// personal key; LINEAR_ACCESS_TOKEN is the OAuth (Bearer) form. The kick's
// work-tracker section (pkg/scheduler) tells the agent which header each one
// maps to.
const (
	linearAccessTokenEnvVar = "LINEAR_ACCESS_TOKEN"
	linearAPIKeyEnvVar      = "LINEAR_API_KEY"
)

// SetBobKeySourceResolver injects the resolver reporting WHERE the bob key was
// found. The returned string is safe to log ("file:<path>" / "env:<NAME>") and
// never contains the key value.
func (m *Manager) SetBobKeySourceResolver(fn func() string) {
	m.bobKeySourceResolver.Store(&fn)
}

// bobKeyFilePath returns the FILE the bob key resolved from, or "" when it came
// from an env var (nothing to permission-check) or is unconfigured.
// Safe to call while holding m.mu.
func (m *Manager) bobKeyFilePath() string {
	fnp := m.bobKeySourceResolver.Load()
	if fnp == nil || *fnp == nil {
		return ""
	}
	source := (*fnp)()
	// Only "file:" sources have a path whose permissions can be checked; an
	// env-var key is inherited through tmux set-environment and needs none.
	const filePrefix = "file:"
	if !strings.HasPrefix(source, filePrefix) {
		return ""
	}
	return strings.TrimPrefix(source, filePrefix)
}

// SetAppAuth injects the GitHub App auth provider for per-agent scoped tokens.
func (m *Manager) SetAppAuth(auth AppTokenMinter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appAuth = auth
}

// SetAgentMint injects the opt-in mint issuer for additional short-lived
// scoped OIDC tokens. Passing a disabled/nil issuer keeps the mint path a strict
// no-op — the GitHub App token path is unaffected either way.
func (m *Manager) SetAgentMint(issuer AgentMintIssuer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.agentMint = issuer
}

// agentMintIssuerLocked reads the attached mint issuer WITHOUT taking m.mu, so
// it is safe from callers that already hold m.mu (e.g. Start holds
// m.mu.Lock()). m.agentMint is injected once via SetAgentMint at wiring time —
// before any agent Start — and never mutated afterward, so the lock-free read is
// race-free. A locked accessor would DEADLOCK on the Start path: Start →
// issueAgentMintToken → (locked read) re-locks a non-reentrant RWMutex on the
// same goroutine, wedging every Manager operation (SendKick, AllStatuses/
// heartbeat, ResolveAgent) behind the held write lock → heartbeat stall →
// liveness 503 → crash-loop.
func (m *Manager) agentMintIssuerLocked() AgentMintIssuer {
	return m.agentMint
}

// agentTokenCacheDir is the directory holding per-agent credential caches. It
// mirrors the App-token cache dir (pkg/github) so both credentials live under
// one agent-owned tree.
//
// A var (not const) so tests can point the cache at a writable temp dir,
// matching the ModeFileGlob/SharedRepoParent/GooseLogsDir seam convention
// (/var/run is not writable in CI). Production value is unchanged and nothing
// on the launch path mutates it.
var agentTokenCacheDir = "/var/run/hive-metrics/agent-tokens"

// agentTokenCachePerms restricts a per-agent credential file to owner-only.
const agentTokenCachePerms = 0o600

// AgentMintTokenCachePath returns the per-agent mint-token cache file path. It
// sits beside the GitHub App token cache but is a distinct file so the two
// credentials never collide — an agent reads the App token for GitHub and the
// mint token for WIF exchange.
func AgentMintTokenCachePath(agentName string) string {
	return agentTokenCacheDir + "/mint-token-" + agentName + ".cache"
}

// issueAgentMintToken mints an additional scoped OIDC token for the agent (when
// the mint is enabled) and writes it to a per-agent, agent-owned cache file. It
// is fail-safe and additive: a disabled mint, a mint error, or a write error is
// logged and swallowed — it NEVER blocks the GitHub App token path or the
// launch. tier is the same trust tier used for the App token, so scopes stay
// consistent across both credentials.
// issueAgentMintToken resolves the mint issuer WITHOUT holding m.mu itself, so
// it is safe to call from Start (which holds m.mu.Lock()). m.agentMint is set
// once at wiring time and never mutated after agents start, so the lock-free
// read is race-free. See agentMintIssuerLocked for the deadlock this avoids.
func (m *Manager) issueAgentMintToken(agentName, tier string, agentUID int) {
	issuer := m.agentMintIssuerLocked()
	if issuer == nil || !issuer.Enabled() {
		return
	}
	token, err := issuer.MintAgentToken(agentName, tier)
	if err != nil {
		m.logger.Warn("mint token issuance failed (App token unaffected)",
			"agent", agentName, "tier", tier, "error", err)
		return
	}
	if token == "" {
		return
	}
	if err := writeAgentCredFile(AgentMintTokenCachePath(agentName), token, agentUID); err != nil {
		m.logger.Warn("writing mint token cache failed (App token unaffected)",
			"agent", agentName, "error", err)
		return
	}
	m.logger.Info("per-agent mint token issued", "agent", agentName, "tier", tier, "uid", agentUID)
}

// writeAgentCredFile atomically writes a credential to path with 0600 perms,
// chowning to agentUID (>0) so only that agent can read it. It mirrors the
// App-token write path (temp file + chown + rename) so a partial write is never
// left in place.
func writeAgentCredFile(path, token string, agentUID int) error {
	if err := os.MkdirAll(agentTokenCacheDir, 0o755); err != nil {
		return fmt.Errorf("creating agent token dir: %w", err)
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(token), agentTokenCachePerms); err != nil {
		return fmt.Errorf("writing cred cache: %w", err)
	}
	if agentUID > 0 {
		if err := os.Chown(tmpPath, agentUID, -1); err != nil {
			_ = os.Remove(tmpPath) // best-effort cleanup; the chown error is what's returned
			return fmt.Errorf("chown cred cache: %w", err)
		}
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath) // best-effort cleanup; the rename error is what's returned
		return fmt.Errorf("rename cred cache: %w", err)
	}
	return nil
}

const (
	// defaultAgentTokenRefreshInterval is how often per-agent scoped token
	// cache files are rewritten. Installation tokens expire after 1 hour;
	// refreshing at 40-minute intervals keeps a valid token on disk with a
	// 20-minute safety margin.
	defaultAgentTokenRefreshInterval = 40 * time.Minute
	// AgentTokenRefreshIntervalEnv overrides the refresh interval with a Go
	// duration string (e.g. "30m"). Invalid or non-positive values fall back
	// to the default.
	AgentTokenRefreshIntervalEnv = "HIVE_AGENT_TOKEN_REFRESH_INTERVAL"
)

// agentTokenRefreshInterval resolves the per-agent token refresh interval
// from AgentTokenRefreshIntervalEnv, falling back to the default.
func agentTokenRefreshInterval() time.Duration {
	if v := os.Getenv(AgentTokenRefreshIntervalEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultAgentTokenRefreshInterval
}

// StartAgentTokenRefresh refreshes per-agent scoped tokens for all running
// agents on a timer. Tokens expire after 1 hour; this refreshes at 40-minute
// intervals (configurable via HIVE_AGENT_TOKEN_REFRESH_INTERVAL) so there's
// always a valid token on disk.
//
// Safe to start even before an App auth is wired: the Linear credential
// push/strip in refreshAgentTokens runs regardless, and the GitHub App token
// portion skips itself while m.appAuth is nil, so hives whose GitHub App
// credentials arrive AFTER boot (heartbeat delivery, config API reinit, config
// reload) start refreshing as soon as SetAppAuth is called. Previously this loop was only started when
// the App was configured at boot, so hosted spokes never refreshed per-agent
// caches: agent sessions outlived their token, `gh` 401'd and printed
// "gh auth login", and the login-detector auto-paused the agent.
func (m *Manager) StartAgentTokenRefresh(ctx context.Context) {
	ticker := time.NewTicker(agentTokenRefreshInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refreshAgentTokens(ctx)
		}
	}
}

// defaultCopilotSessionRefreshInterval is how often the Copilot session-token
// refresh loop ensures the CLI's config.json copilotTokens map is populated
// from the durable user token. Chosen well under the ~hour that a stored token
// stays fresh so an emptied map is re-seeded long before an agent next reads
// it, and low-cost (a small file read + conditional write) so a tight interval
// is cheap.
const defaultCopilotSessionRefreshInterval = 10 * time.Minute

// CopilotSessionRefreshIntervalEnv overrides the interval with a Go duration.
const CopilotSessionRefreshIntervalEnv = "HIVE_COPILOT_SESSION_REFRESH_INTERVAL"

func copilotSessionRefreshInterval() time.Duration {
	if v := os.Getenv(CopilotSessionRefreshIntervalEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultCopilotSessionRefreshInterval
}

// StartCopilotSessionRefresh keeps the Copilot CLI's config.json copilotTokens
// map populated from the durable user token, so agents never sit stuck at
// "Please use /login" while a VALID token exists.
//
// The failure it fixes: clearExpiredTokens (on an auth-error diagnostic) and a
// config rewrite on an upgrade roll leave copilotTokens EMPTY, and CLI 1.0.78
// does NOT re-populate it from the injected COPILOT_GITHUB_TOKEN on its own —
// it just prompts /login. A restart does not help (it re-reads the empty map).
// Every roll churns fresh agent pods, so absent this the whole hive can go dark
// on Copilot with a perfectly good token sitting on disk.
//
// It is NOT a login: it never runs a device flow and never mints or fetches a
// token. It only re-uses the token the operator already provided (m.copilotAuth‐
// Token / the durable file), writing it into the store the CLI reads. When the
// hive holds no Copilot token it does nothing — recovery there is still the
// operator's manual dashboard login.
//
// Gating: only runs on hives with a copilot-backend agent. Only writes when the
// store is EMPTY (or absent) — it never overwrites a token the CLI itself wrote
// and is still using, so it can't clobber a live session.
func (m *Manager) StartCopilotSessionRefresh(ctx context.Context) {
	// Run once shortly after start rather than waiting a full interval for the
	// first tick. Spokes that roll faster than the interval (ks/hive rolls every
	// ~15-30m) would otherwise seldom reach the first tick before the pod dies,
	// so a promote/seed could be perpetually deferred. The short settle delay
	// lets the agents' CLIs write their config.json first, so a login done just
	// before/at boot is visible to the promote path. Bounded by ctx so a fast
	// shutdown does not block.
	select {
	case <-ctx.Done():
		return
	case <-time.After(copilotSessionRefreshStartDelay()):
		m.refreshCopilotSessionToken()
	}

	ticker := time.NewTicker(copilotSessionRefreshInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refreshCopilotSessionToken()
		}
	}
}

// copilotSessionRefreshStartDelay is how long to wait after boot before the
// first session-token reconcile, giving agent CLIs time to write config.json.
// Env-overridable (HIVE_COPILOT_SESSION_REFRESH_START_DELAY) mainly so tests
// can drive it to near-zero.
const defaultCopilotSessionRefreshStartDelay = 30 * time.Second

// CopilotSessionRefreshStartDelayEnv overrides the start delay with a Go
// duration. Non-positive/invalid values fall back to the default.
const CopilotSessionRefreshStartDelayEnv = "HIVE_COPILOT_SESSION_REFRESH_START_DELAY"

func copilotSessionRefreshStartDelay() time.Duration {
	if v := os.Getenv(CopilotSessionRefreshStartDelayEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return defaultCopilotSessionRefreshStartDelay
}

// refreshCopilotSessionToken keeps the Copilot credential in sync between the
// CLI's config.json copilotTokens map and the hive's durable store, in BOTH
// directions, on a copilot-backend hive. One tick's work; safe from the timer
// or ad hoc.
//
//   - PROMOTE (config → durable): if the CLI has a token (someone logged in
//     INSIDE an agent with /login) but the hive's in-memory/durable token is
//     missing or stale, mirror the CLI's token to the durable file and memory.
//     Explicit environment and dashboard tokens remain authoritative, so a
//     stale CLI account cannot overwrite them.
//   - SEED (durable → config): if the CLI's map is EMPTY but the hive holds a
//     token, restore it so the CLI is not left stuck at /login (the #4494 case).
//
// It never runs a device flow and never mints a token; it only mirrors a token
// that already exists. The manual-login rule is intact.
func (m *Manager) refreshCopilotSessionToken() {
	if !m.backendInUse("copilot") {
		return
	}
	m.syncCopilotToken(sharedCopilotConfigPath, CopilotUserTokenPath)
}

// copilotSyncAction names what one syncCopilotToken call did, for logging and
// deterministic tests.
type copilotSyncAction int

const (
	copilotSyncNoop    copilotSyncAction = iota
	copilotSyncPromote                   // config token mirrored to the durable store
	copilotSyncSeed                      // durable token restored into an empty config
)

// syncCopilotToken reconciles the CLI's config.json copilotTokens map with the
// hive's durable/in-memory token, in both directions. Parameterized on both
// paths so it is testable without touching /data. See refreshCopilotSessionToken
// for the promote/seed rationale.
func (m *Manager) syncCopilotToken(configPath, durablePath string) copilotSyncAction {
	m.mu.RLock()
	held := strings.TrimSpace(m.copilotAuthToken)
	authoritative := m.copilotAuthTokenAuthoritative
	rejected := m.copilotAuthTokenRejected
	m.mu.RUnlock()

	if copilotCredentialFileHasTokens(configPath) {
		cliTok := extractCopilotToken(configPath)
		if cliTok != "" && cliTok == held {
			return copilotSyncNoop
		}
		// #6767: if the authoritative token has been observed being rejected
		// upstream by Copilot ("not licensed to use Copilot"), a DIFFERENT
		// real token now sitting in the shared CLI config is almost certainly
		// an operator's recovery /login — the exact case @MikeSpreitzer
		// reported. Clobbering it with the known-bad authoritative token
		// would put every agent right back at "not licensed" 30 s later,
		// which is the loop the second closure of #6500 missed. Fall through
		// to PROMOTE so the fresh token becomes the durable one.
		if authoritative && !(rejected && cliTok != "" && cliTok != held) {
			// SEED: explicit configuration and a just-completed dashboard login
			// outrank whatever identity a shared config inherited. GitHub documents
			// COPILOT_GITHUB_TOKEN as the CLI's highest-precedence credential; the
			// reconciler must not silently reverse that precedence 30 seconds later.
			if held == "" {
				return copilotSyncNoop
			}
			if err := replaceCopilotTokens(configPath, held); err != nil {
				m.logger.Warn("copilot session refresh: failed to activate authoritative token",
					"path", configPath, "error", err)
				return copilotSyncNoop
			}
			m.logger.Info("copilot session refresh: replaced stale CLI identity with authoritative token",
				"path", configPath)
			return copilotSyncSeed
		}
		// PROMOTE: without an authoritative runtime token, mirror an unambiguous
		// active CLI login to the durable store. Ambiguous multi-account configs
		// intentionally return no token rather than choosing one at random.
		if cliTok == "" {
			return copilotSyncNoop
		}
		if err := writeDurableCopilotToken(durablePath, cliTok); err != nil {
			m.logger.Warn("copilot session refresh: failed to promote CLI token to durable file",
				"path", durablePath, "error", err)
			return copilotSyncNoop
		}
		// installCopilotToken, not setCopilotToken: the promoted token is the
		// operator's recovery /login, and the agents still running on the token
		// it replaces have to be moved onto it or the whole fleet keeps
		// reporting "not licensed" against a licence that is now fine (#6500).
		m.installCopilotToken(cliTok, false, CopilotTokenSourceCLIConfig)
		m.logger.Info("copilot session refresh: promoted in-agent login token to the durable store",
			"path", durablePath)
		return copilotSyncPromote
	}

	// SEED: the CLI map is empty; restore from the held token so agents are not
	// left stuck at /login.
	if held == "" {
		// Genuinely logged out — the StartCredentialWatchdog alert covers this;
		// recovery is a manual login.
		return copilotSyncNoop
	}
	seed := restoreCopilotTokens
	if authoritative {
		seed = replaceCopilotTokens
	}
	if err := seed(configPath, held); err != nil {
		m.logger.Warn("copilot session refresh: failed to restore copilotTokens",
			"path", configPath, "error", err)
		return copilotSyncNoop
	}
	m.logger.Info("copilot session refresh: re-seeded empty copilotTokens from durable user token",
		"path", configPath)
	return copilotSyncSeed
}

// backendInUse reports whether any configured agent runs on the named backend
// (honoring a runtime BackendOverride).
func (m *Manager) backendInUse(backend string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.agents {
		if effectiveBackend(a) == backend {
			return true
		}
	}
	return false
}

// RefreshAgentTokens immediately rewrites every running agent's per-agent
// scoped token cache. Called when GitHub App auth is wired late (heartbeat
// delivery, config API reinit, config reload) so agents that were already
// running get a valid cache right away instead of waiting for the next tick.
func (m *Manager) RefreshAgentTokens(ctx context.Context) {
	m.refreshAgentTokens(ctx)
}

// RefreshAgentTokenFor re-mints and re-caches the scoped token for a single
// agent, using the same tier logic as launch. Used by the login-detector to
// attempt a token re-cache before pausing: the likeliest cause of a `gh`
// "gh auth login" prompt on an App-authenticated hive is an expired cached
// token, so refreshing it means a subsequent Resume works immediately.
// Returns an error when the agent is unknown, has no UID, or no App auth is
// wired.
func (m *Manager) RefreshAgentTokenFor(ctx context.Context, name string) error {
	m.mu.RLock()
	auth := m.appAuth
	agent, ok := m.agents[name]
	m.mu.RUnlock()

	if auth == nil {
		return fmt.Errorf("no github app auth wired")
	}
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}
	if agent.UID <= 0 {
		return fmt.Errorf("agent %s has no dedicated UID", name)
	}
	tier := TokenTierForRole(m.agentMode(agent), agent.Config.Role)
	if err := auth.WriteAgentToken(ctx, agent.Name, tier, agent.UID); err != nil {
		return fmt.Errorf("re-caching scoped token for %s: %w", name, err)
	}
	return nil
}

func (m *Manager) refreshAgentTokens(ctx context.Context) {
	m.mu.RLock()
	auth := m.appAuth
	agents := make([]*AgentProcess, 0, len(m.agents))
	for _, a := range m.agents {
		if a.State == StateRunning && a.UID > 0 {
			agents = append(agents, a)
		}
	}
	m.mu.RUnlock()

	// Re-push the Linear OAuth token alongside the GitHub one. Linear access
	// tokens expire (~24h) and liveToken refreshes them through the store;
	// the agent's CLI reads its env once per turn, so this refresh tick
	// (agentTokenRefreshInterval, 40m by default) is what keeps
	// LINEAR_ACCESS_TOKEN current inside a long-running session. For agents
	// below the ISSUES_ONLY floor the same tick actively UNSETS both
	// credential vars instead — see linearRefreshTmuxArgs for why the
	// creation-time strip in ensureTmuxSession is not enough. Independent of
	// GitHub App auth: a Linear-sourced hive with no App still needs this,
	// hence it runs before the auth == nil return below.
	for _, a := range agents {
		if a.tmuxSession == "" {
			continue
		}
		for _, args := range m.linearRefreshTmuxArgs(a) {
			_ = m.tmuxCmd(a, args...).Run()
		}
	}

	if auth == nil {
		return
	}

	for _, a := range agents {
		tier := TokenTierForRole(m.agentMode(a), a.Config.Role)
		if err := auth.WriteAgentToken(ctx, a.Name, tier, a.UID); err != nil {
			m.logger.Warn("agent token refresh failed", "agent", a.Name, "error", err)
			continue
		}
		// Refresh the opt-in mint token alongside the App token (no-op when off).
		m.issueAgentMintToken(a.Name, tier, a.UID)
		// Re-inject the freshly-minted App token into the running session so the
		// GitHub MCP server keeps authenticating as the App bot. The scoped token
		// expires hourly; WriteAgentToken above rewrites the cache FILE (which the
		// git credential helper re-reads per call), but the Copilot CLI reads
		// GITHUB_TOKEN once from its env at startup — so without this push the MCP
		// token would go stale an hour after launch and GitHub writes would start
		// 401ing. tmux set-environment only affects processes started AFTER it,
		// which is fine: the Copilot CLI is (re)spawned per agent turn, so each new
		// turn picks up the current token. Gated on the opt-in flag + CanPush() to
		// match the launch injection — advisory agents stay GITHUB_TOKEN-less, and
		// hives that have not opted in get nothing injected.
		if m.project.AppAuthoredPRs && a.tmuxSession != "" && m.agentMode(a).CanPush() {
			if data, err := os.ReadFile(ghpkg.AgentTokenCachePath(a.Name)); err == nil {
				if tok := strings.TrimSpace(string(data)); tok != "" {
					_ = m.tmuxCmd(a, "set-environment", "-t", a.tmuxSession, "GITHUB_TOKEN", tok).Run()
				}
			}
		}
	}
}

func truncateStr(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

// mintAgentTokenUnlocked mints the per-agent tier-scoped GitHub App token
// (WriteAgentToken) and the opt-in short-lived mint token for the agent. It is
// the single mint step every (re)launch path — Start, Resume, Restart,
// RestartWithBootstrap — runs before launchInTmux (#3962: previously only
// Start minted, so an agent relaunched via Resume/Restart never got a token
// until the whole process restarted; its cache file stayed 0 bytes and gh/git
// push failed silently).
//
// MUST be called with m.mu RELEASED. WriteAgentToken makes an outbound GitHub
// API call (through the MITM egress proxy, which the production incident
// showed can hang), and issueAgentMintToken can do the same. Holding m.mu
// across a hang stalls AllStatuses()/the heartbeat for the WHOLE fleet — the
// exact liveness flap Start's phase split was built to avoid. The function
// reads only immutable agent identity (Name, UID) and Config via agentMode,
// and writes no m.mu-guarded AgentProcess field, so running it lock-free is
// race-free. m.appAuth is read without the lock exactly as Start's unlocked
// phase always did: it is injected once at wiring time, before agents start.
func (m *Manager) mintAgentTokenUnlocked(ctx context.Context, agent *AgentProcess) {
	if m.appAuth == nil || agent.UID <= 0 {
		return
	}
	tier := TokenTierForRole(m.agentMode(agent), agent.Config.Role)
	if err := m.appAuth.WriteAgentToken(ctx, agent.Name, tier, agent.UID); err != nil {
		// Be precise about the blast radius. Since audit H3 the shared-cache
		// fallback is GONE: gh-wrapper.sh and git-credential-hive.sh no
		// longer fall back to /var/run/hive-metrics/gh-app-token.cache (the
		// FULL installation token, now owner-only 0600). They FAIL LOUD when
		// the per-agent scoped token is absent rather than silently
		// escalating this agent to full privilege. So a delivery failure here
		// means this agent's GitHub writes (gh + git push) will fail until
		// token delivery is repaired — not a silent privilege escalation.
		m.logger.Warn("per-agent scoped token NOT delivered — this agent's GitHub writes (gh + git push) will FAIL (no shared-cache fallback by design; see audit H3)",
			"agent", agent.Name, "tier", tier, "error", err)
	}
	// Additionally issue an opt-in short-lived mint token (no-op when the mint
	// is disabled). This is additive and fail-safe — it never blocks launch.
	m.issueAgentMintToken(agent.Name, tier, agent.UID)
}
