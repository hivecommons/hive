// Agent environment and prompt-preamble assembly: bootstrap prompt,
// ACMM fragments, project preamble, coverage preamble, secret env
// application, env filtering, and git-remote sanitization.
package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func (m *Manager) buildBootstrapPrompt(agent *AgentProcess) string {
	// No boot prompt — the governor's first eval cycle (10s after startup)
	// kicks all due agents via BuildKickMessages with fully substituted
	// templates. Sending a boot prompt here caused unsubstituted ${ISSUE_LIST}
	// and other vars to reach the agent. The policy-file path list this
	// function used to assemble was dead code once the boot prompt was
	// removed, so it is gone too.
	_ = agent // signature kept for the call site; the arg is no longer read
	return ""
}

// shellEnvVar formats KEY='value' with single-quoting so values containing
// spaces, parentheses, or other shell metacharacters are safe in inline env
// var assignments sent to tmux via send-keys.
func shellEnvVar(key, value string) string {
	quoted := strings.ReplaceAll(value, "'", "'\"'\"'")
	return fmt.Sprintf("%s='%s'", key, quoted)
}

// applySecretEnv pushes only the Secret pairs into the agent's tmux session via
// set-environment. Values are passed as exec args (never through a shell), so
// they are not word-split and never land in the pane or in bash history.
// Failures are ignored for the same reason ensureTmuxSession ignores them: a
// missing session is handled by the launch path, not here.
func (m *Manager) applySecretEnv(agent *AgentProcess) {
	if agent == nil || agent.tmuxSession == "" {
		return
	}
	for _, p := range m.agentEnvPairs(agent) {
		if !p.Secret {
			continue
		}
		_ = m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, p.Key, p.Value).Run()
	}
}

func (m *Manager) buildEnvPrefix(agent *AgentProcess) string {
	pairs := m.agentEnvPairs(agent)
	var parts []string
	for _, p := range pairs {
		if p.Secret {
			continue
		}
		parts = append(parts, shellEnvVar(p.Key, p.Value))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " "
}

// embeddedTokenRe matches git remote URLs with embedded credentials:
// https://x-access-token:TOKEN@github.com/org/repo.git
var embeddedTokenRe = regexp.MustCompile(`^https://[^@]+@(github\.com/.+)$`)

// sanitizeGitRemotes strips embedded tokens from git remote URLs in all repos
// under the agent's work directory. Copilot CLI embeds the GitHub App token
// directly in the remote URL when it clones, bypassing both the credential
// helper (Layer 1) and env var filtering (Layer 2).
func (m *Manager) sanitizeGitRemotes(agent *AgentProcess) {
	if m.agentMode(agent).CanPush() {
		return
	}
	agentDir := m.workDir + "/" + agent.Name
	_ = filepath.WalkDir(agentDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.Name() != ".git" || !d.IsDir() {
			return nil
		}
		repoDir := filepath.Dir(path)
		out, err := exec.Command("git", "-C", repoDir, "remote", "get-url", "origin").Output()
		if err != nil {
			return filepath.SkipDir
		}
		url := strings.TrimSpace(string(out))
		if match := embeddedTokenRe.FindStringSubmatch(url); match != nil {
			clean := "https://" + match[1]
			_ = exec.Command("git", "-C", repoDir, "remote", "set-url", "origin", clean).Run()
			m.logger.Info("stripped embedded token from git remote",
				"agent", agent.Name, "repo", repoDir)
		}
		return filepath.SkipDir
	})
}

// agentEnvPair is an unquoted key-value environment variable.
type agentEnvPair struct {
	Key   string
	Value string
	// Secret vars are set via tmux set-environment only, never on the command line.
	Secret bool
}

// inferenceQuietCLIEnv is the set of Claude CLI switches exported to
// inference-routed sessions so the CLI stops emitting non-inference traffic
// (telemetry, error reporting, nonessential lookups) to its Anthropic host.
var inferenceQuietCLIEnv = []string{
	"DISABLE_TELEMETRY",
	"DISABLE_ERROR_REPORTING",
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
}

func (m *Manager) agentEnvPairs(agent *AgentProcess) []agentEnvPair {
	model := agent.Config.Model
	if agent.ModelOverride != "" {
		model = agent.ModelOverride
	}
	backend := agent.Config.Backend
	if agent.BackendOverride != "" {
		backend = agent.BackendOverride
	}
	displayName := agent.Config.DisplayName
	if displayName == "" {
		displayName = agent.Name
	}
	vars := []agentEnvPair{
		{"HIVE_AGENT", agent.Name, false},
		{"HIVE_AGENT_DISPLAY_NAME", displayName, false},
		{"HIVE_BACKEND", backend, false},
		{"HIVE_MODEL", model, false},
	}
	if hiveID := os.Getenv("HIVE_ID"); hiveID != "" {
		vars = append(vars, agentEnvPair{"HIVE_ID", hiveID, false})
	}
	vars = append(vars, agentEnvPair{"HIVE_ACMM_LEVEL", fmt.Sprintf("%d", m.project.ACMMLevel), false})

	mode := m.agentMode(agent)
	if agent.Config.Tools != nil {
		if effectiveMode := agent.Config.Tools.EffectiveMode(); effectiveMode != "" {
			vars = append(vars, agentEnvPair{"HIVE_AGENT_MODE", effectiveMode, false})
		} else {
			vars = append(vars, agentEnvPair{"HIVE_AGENT_MODE", mode.String(), false})
		}
	} else {
		vars = append(vars, agentEnvPair{"HIVE_AGENT_MODE", mode.String(), false})
	}
	modeFile := filepath.Join(agentStateDir, ".hive-mode-"+agent.Name)
	if err := writeAgentStateFile(modeFile, []byte(mode.String())); err != nil {
		m.logger.Warn("agentBootstrapEnv: mode file write failed", "file", modeFile, "error", err)
	}
	m.writeAgentCapsFile(agent.Name, m.agentCapabilities(agent))
	// Plain proxy URL without userinfo — Claude Code's native binary fails
	// to open a socket when the URL contains username:password@ (FailedToOpenSocket).
	// Agent identification uses UID-based /proc/net/tcp lookup instead of
	// Proxy-Authorization headers. GIT_TERMINAL_PROMPT=0 prevents git from
	// prompting for proxy credentials.
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", proxyListenPort)
	vars = append(vars, agentEnvPair{"HTTPS_PROXY", proxyURL, false})
	vars = append(vars, agentEnvPair{"HTTP_PROXY", proxyURL, false})
	vars = append(vars, agentEnvPair{"HIVE_PROXY_AGENT", agent.Name, false})
	vars = append(vars, agentEnvPair{"GIT_TERMINAL_PROMPT", "0", false})
	vars = append(vars, agentEnvPair{"NODE_EXTRA_CA_CERTS", proxyCACertPath, false})
	if sha := os.Getenv("HIVE_SHA"); sha != "" {
		vars = append(vars, agentEnvPair{"HIVE_SHA", sha, false})
	}
	if advisory := os.Getenv("HIVE_ADVISORY_ISSUE"); advisory != "" {
		vars = append(vars, agentEnvPair{"HIVE_ADVISORY_ISSUE", advisory, false})
	}
	// HIVE_REPO / HIVE_REPOS: the shipped policy templates instruct agents to
	// run `gh issue create --repo "$HIVE_REPO"`, but nothing ever exported it
	// to hosted agents (only the OSS scheduler set a hardcoded "<org>/hive").
	// Root-caused on a live hosted hive (2026-08-20): the sec-check agent saw
	// HIVE_REPO unset, fell back to the git remote of its own workdir, and
	// silently scanned only the primary repo — the other project repos were
	// never touched. Export the primary repo and the full project repo list so
	// templates and agents can target every configured repo.
	if m.project.Org != "" && len(m.project.Repos) != 0 {
		primary := m.project.PrimaryRepo()
		if primary == "" {
			primary = m.project.Repos[0]
		}
		vars = append(vars, agentEnvPair{"HIVE_REPO", m.project.Org + "/" + primary, false})
		full := make([]string, len(m.project.Repos))
		for i, r := range m.project.Repos {
			full[i] = m.project.Org + "/" + r
		}
		vars = append(vars, agentEnvPair{"HIVE_REPOS", strings.Join(full, ","), false})
	}
	// GH_HOST: point the gh CLI at the configured forge host for GHE spokes.
	// See ProjectContext.GHHost. The gh wrapper pairs this with
	// GH_ENTERPRISE_TOKEN so the per-agent scoped token authenticates there.
	if m.project.GHHost != "" {
		vars = append(vars, agentEnvPair{"GH_HOST", m.project.GHHost, false})
	}
	if m.routableBackend(backend) {
		const inferenceTranslatePort = 18444
		vars = append(vars, agentEnvPair{"ANTHROPIC_API_KEY", "sk-hive-" + agent.Name, false})
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", inferenceTranslatePort)
		vars = append(vars, agentEnvPair{"ANTHROPIC_BASE_URL", baseURL, false})
		vars = append(vars, agentEnvPair{"NO_PROXY", "127.0.0.1,localhost", false})
		// Cap the CLI output-token budget at a value every commercial model
		// litellm may front will accept. A prior 128000 (chosen so verbose
		// OSS models would not truncate) exceeds Azure GPT-4o's 16384
		// completion-token cap, so every request 400s with
		// "max_tokens is too large: 128000. This model supports at most
		// 16384 completion tokens". See inferenceMaxOutputTokensDefault.
		// TODO: the gateway 400 body names the model's real cap ("supports
		// at most N completion tokens"); a future enhancement could parse it
		// to auto-adjust per-model instead of using a universal floor.
		vars = append(vars, agentEnvPair{"CLAUDE_CODE_MAX_OUTPUT_TOKENS", strconv.Itoa(inferenceMaxOutputTokensDefault), false})
		// The Claude CLI sends telemetry batches, error reports, and other
		// non-inference traffic to its configured Anthropic host. Routed at
		// an OpenAI-compatible gateway that traffic has nowhere useful to go
		// (the proxy now answers it locally rather than forwarding it — see
		// classifyInferencePath), so switch it off at the source. Only for
		// inference-routed sessions: subscription/Anthropic-direct sessions
		// keep Anthropic's own telemetry.
		for _, v := range inferenceQuietCLIEnv {
			vars = append(vars, agentEnvPair{v, "1", false})
		}
	}
	if m.copilotAuthToken != "" {
		vars = append(vars, agentEnvPair{"COPILOT_GITHUB_TOKEN", m.copilotAuthToken, true})
	}
	// Point the GitHub MCP server at the App installation token so PRs, issue
	// comments, and merges are authored by the App bot ("<slug>[bot]") — NOT by
	// the Copilot login user. COPILOT_GITHUB_TOKEN above stays as the Copilot
	// OAuth token because it authenticates the AI model (a separate concern from
	// GitHub write identity); leaving it untouched keeps the Copilot CLI login
	// working. The Copilot CLI reads GH_TOKEN / GITHUB_TOKEN for GitHub API auth
	// (per its README: GH_TOKEN or GITHUB_TOKEN, in that precedence), so setting
	// GITHUB_TOKEN here makes the built-in GitHub MCP server act as the App bot.
	//
	// Gated on the opt-in flag first (default OFF → no behavior change on any
	// hive that has not explicitly enabled App-bot authorship), then on CanPush():
	// advisory agents are deliberately kept GITHUB_TOKEN-less (see the -u
	// GITHUB_TOKEN strip after the env loop) so they cannot write; only push-
	// capable tiers — the ones that legitimately open/merge PRs — get the App
	// token. m.appAuth != nil means an App is configured. The value is the
	// per-agent tier-SCOPED App token, and refreshAgentTokens re-pushes it hourly
	// so it never goes stale.
	if m.project.AppAuthoredPRs && m.appAuth != nil && agent.UID > 0 && m.agentMode(agent).CanPush() {
		if data, err := os.ReadFile(ghpkg.AgentTokenCachePath(agent.Name)); err == nil {
			if tok := strings.TrimSpace(string(data)); tok != "" {
				vars = append(vars, agentEnvPair{"GITHUB_TOKEN", tok, true})
			}
		}
	}
	// Linear write credential for ISSUES_ONLY+ agents — see linearEnvPairs.
	// Nil for advisory agents and for hives with no Linear credential, so a
	// GitHub-only hive sees no change.
	vars = append(vars, m.linearEnvPairs(agent)...)
	// CLAUDE_CODE_OAUTH_TOKEN is a LAST RESORT, not the normal delivery path.
	//
	// Claude Code treats this variable as a static bearer token: when it is
	// set the CLI uses it verbatim, never opens ~/.claude/.credentials.json,
	// and therefore never refreshes. Measured in-container (2026-09-01): with
	// the variable set to a bad value and a perfectly good credentials file
	// beside it, the CLI answered "401 OAuth access token is invalid" — there
	// is no fallback to the file.
	//
	// m.claudeAuthToken is a snapshot of the SHORT-LIVED access token, taken
	// once at manager construction and refreshed only by ReloadClaudeToken()
	// after a dashboard login. Injecting it therefore pinned every claude
	// agent to the remaining life of whatever access token happened to be on
	// disk when the container started — Claude access tokens live 8h, so the
	// whole fleet 401'd within a day of every restart and the only recovery
	// hive offered was an operator re-login, once per agent. That is the daily
	// re-authentication treadmill of #5454.
	//
	// It is also unnecessary since per-agent homes (#4619): every agent's
	// ~/.claude is a symlink to the shared /data/home/.claude, so the CLI can
	// read the credential itself — and redeem its refresh grant on start,
	// which is the one thing the env var makes impossible.
	//
	// So inject ONLY when the agent has no credential file it can read. That
	// keeps the variable doing the job it was added for (#c5648bc9: deliver a
	// dashboard-obtained token to an agent that cannot see the file) and stops
	// it overriding a credential that can still refresh itself.
	if m.claudeAuthToken != "" && backend == "claude" && !claudeCredentialReachable(agent, backend) {
		vars = append(vars, agentEnvPair{"CLAUDE_CODE_OAUTH_TOKEN", m.claudeAuthToken, true})
	}
	// bob reads its key from BOBSHELL_API_KEY. Secret: true keeps the value off
	// the shell command line (out of `ps`, bash history, and pane scrollback);
	// it reaches the CLI via tmux set-environment only. Gated on the backend so
	// no other CLI's environment carries an IBM credential it has no use for.
	if backend == bobBackend {
		if key := m.bobAPIKey(); key != "" {
			vars = append(vars, agentEnvPair{config.BobAPIKeyEnvVar, key, true})
		}
		// BOBSHELL_DEFAULT_AUTH_TYPE is what actually selects API-key auth;
		// without it bob defaults to W3ID SSO and parks at the interactive key
		// prompt forever. Deliberately NOT Secret: the value is the literal
		// non-credential string "api-key", and secret pairs only reach a
		// freshly-created pane shell via tmux set-environment, whereas
		// non-secret pairs are re-applied on EVERY launch through
		// buildEnvPrefix. That asymmetry is exactly what caused the sibling
		// bug fixed in #2228, so the auth type must ride the always-reapplied
		// path or a relaunch into an existing session loses it.
		vars = append(vars, agentEnvPair{config.BobAuthTypeEnvVar, config.BobAuthTypeAPIKey, false})
	}
	// BD_DIR tells the `bd` CLI where to read/write beads. Without this,
	// bd falls back to cwd (/data/agents/<name>) instead of the configured
	// beads_dir (/data/beads/<name>), causing a path mismatch with the
	// dashboard and advisory digest.
	if agent.Config.BeadsDir != "" {
		vars = append(vars, agentEnvPair{"BD_DIR", agent.Config.BeadsDir, false})
	}
	if agent.Config.CavemanMode != "" {
		vars = append(vars, agentEnvPair{"HIVE_CAVEMAN_MODE", agent.Config.CavemanMode, false})
	}
	// Export the RESOLVED explain mode, not the raw config value, so an agent's
	// skills and helper scripts see the same answer the kick suffix acted on
	// (including inheritance from the hive-wide default and the off fallback for
	// an invalid value). Always exported, off included, so a script can branch on
	// it without having to re-derive the precedence rules itself.
	vars = append(vars, agentEnvPair{config.ExplainModeEnvVar, resolveExplainMode(agent.Config, m.explainModeDefault()), false})
	// GIT_SSL_CAINFO only — NOT SSL_CERT_FILE (that breaks Copilot API TLS)
	vars = append(vars, agentEnvPair{"GIT_SSL_CAINFO", proxyCACertPath, false})
	if agent.UID > 0 {
		vars = append(vars, agentEnvPair{"HIVE_AGENT_TOKEN_CACHE", ghpkg.AgentTokenCachePath(agent.Name), false})
	}
	if agent.UID > 0 {
		// Per-UID agents get a per-agent HOME (#4596) — AgentHome is the single
		// source of truth so the auth probe and this export can never diverge.
		vars = append(vars, agentEnvPair{"HOME", AgentHome(agent.Name, agent.UID, backend), false})

		// Under the per-agent-UID layout the global npm prefix is owned by the
		// image's build user, so the Claude Code CLI's self-updater fails on
		// every launch with "✘ Auto-update failed: no write permission to npm
		// prefix" — a red line in every agent pane for an update the agent must
		// not perform anyway (the CLI version is managed by the image, not by
		// an in-pod npm write). Disabling the updater removes the failure at its
		// source; a per-agent npm prefix would instead let an agent drift off
		// the pinned image version.
		vars = append(vars, agentEnvPair{"DISABLE_AUTOUPDATER", "1", false})
	}

	// Codex CLI 0.144.1's in-process app-server performs OWNER-gated operations
	// on files under CODEX_HOME (helper-binary "PATH alias" symlinks under
	// tmp/arg0, sqlite state). The shared /data/home/.codex is owned by dev
	// (the entrypoint chowns it group-writable + setgid), which claude/copilot
	// tolerate but Codex does not — every non-owner agent UID fails with
	// "failed to start embedded app server: Operation not permitted (os error 1)".
	// The manager launches the codex binary DIRECTLY (not via agent-launch.sh),
	// so CODEX_HOME must be set here. Give each agent its own dir; codex will
	// NOT create it (it errors "CODEX_HOME ... does not exist"), so it is
	// pre-created AS the agent below in setupCodexHome.
	if backend == codexBackend {
		vars = append(vars, agentEnvPair{"CODEX_HOME", codexHomePath(agent.Name), false})
	}

	for _, conn := range agent.Config.Connections {
		if conn.Type != "api" {
			continue
		}
		envName := conn.EnvName
		if envName == "" {
			envName = "HIVE_CONN_" + strings.ToUpper(strings.ReplaceAll(conn.Name, "-", "_")) + "_URL"
		}
		vars = append(vars, agentEnvPair{envName, conn.URI, false})
		if conn.Auth != nil && conn.Auth.Type == "env" && conn.Auth.EnvVar != "" {
			if tokenVal := os.Getenv(conn.Auth.EnvVar); tokenVal != "" {
				vars = append(vars, agentEnvPair{conn.Auth.EnvVar, tokenVal, true})
			}
		}
	}

	return vars
}
