package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/kubestellar/hive/pkg/claude"
	"github.com/kubestellar/hive/pkg/config"
)

// This file computes the per-agent CLI auth state that drives the dashboard's
// 🔑 "needs login" badge.
//
// WHY IT EXISTS (the bug it fixes)
//
// BackendAuthAvailable answers a BACKEND-level question by stat'ing ONE shared
// path: /data/home/.claude/.credentials.json (claude.CredentialsPath). That was
// correct when every agent ran as the same UID out of the same HOME. The fleet
// now runs PER-AGENT UIDs — each agent has its own UID, its own tmux socket
// (/tmp/tmux-<uid>/<name>) and its OWN HOME — so on a per-UID spoke the shared
// legacy locations are empty even while every agent is authenticated and
// working out of its own home. The backend probe then answered
// (available=false, known=true), which the dashboard renders as
// `authKnown && !authAvailable` → 🔑 Login on agents that are demonstrably
// running, being kicked, and passing /api/health/deep.
//
// PRECEDENCE RULE implemented by AgentAuthState (in order — first match wins):
//
//  1. METHOD GATE. If the agent's backend does not use interactive login at
//     all (inference/API-key backends: litellm, vllm, llm-d — auth is a key in
//     config, and bob when its API key is present), the answer is
//     "authenticated, known". A badge here is ALWAYS wrong: there is no login
//     for the operator to perform.
//  2. POSITIVE EVIDENCE OF HEALTH beats absence-of-file. An agent that is
//     StateRunning and NOT sitting at a login prompt is, by observation, doing
//     work — that is the same signal /api/health/deep reports as `pass`. A
//     missing credentials FILE cannot outrank a working process (the CLI may
//     hold a live session in memory, or its credentials may live under a home
//     this process cannot see). Report unknown so the UI shows no badge.
//  3. TRUE LOGIN SIGNAL is preserved. proc.NeedsLogin (the pane poller having
//     literally seen a login prompt on the agent's terminal) is authoritative
//     and is handled by the callers ahead of this probe — it is never masked
//     by rules 1 or 2 for interactive backends.
//  4. Only then does the FILE PROBE run, and it looks under the AGENT'S OWN
//     home first (per-UID layout) before falling back to the shared legacy
//     path. Absence is reported as "needs login" ONLY when the method requires
//     interactive auth AND the agent is not successfully running.

// interactiveAuthBackends are the CLI backends whose credentials come from an
// INTERACTIVE login flow (OAuth / device flow) that a human must complete. Only
// these can ever legitimately show a login badge.
var interactiveAuthBackends = map[string]bool{
	"claude":  true,
	"copilot": true,
	"codex":   true,
	"gemini":  true,
}

// BackendRequiresInteractiveAuth reports whether a backend authenticates via an
// interactive login the operator must perform. Model-gateway backends
// (config.InferenceBackends: litellm, vllm, llm-d, watsonx) authenticate with
// an API key supplied by config — watsonx with an IAM bearer minted from that
// key — and therefore NEVER require a login. Showing them a 🔑 badge is always
// a false alarm.
func BackendRequiresInteractiveAuth(backend string) bool {
	if config.IsInferenceBackend(backend) {
		return false
	}
	return interactiveAuthBackends[backend]
}

// AgentHome returns the HOME directory an agent's CLI runs with. It mirrors
// exactly what buildAgentEnv exports as HOME on the launch path, so the auth
// probe reads the same filesystem location the CLI writes to. Agents with no
// allocated UID (UID == 0) run as the hive user out of the process HOME.
func AgentHome(agentName string, uid int, backend string) string {
	if uid <= 0 {
		if h := os.Getenv("HOME"); h != "" {
			return h
		}
		return "/data/home"
	}
	if IsInferenceBackend(backend) {
		return inferenceHomePath(agentName)
	}
	return "/data/home"
}

// agentClaudeCredentialPaths returns the .credentials.json locations to try for
// one agent, agent-own home FIRST (per-UID layout) then the shared legacy path.
// Ordering matters: the per-UID file is the one the agent's CLI actually writes.
func agentClaudeCredentialPaths(agentName string, uid int, backend string) []string {
	paths := []string{}
	if uid > 0 && backend == "claude" {
		// Per-agent CLAUDE_CONFIG_DIR first (#4596): under that layout this is
		// the file the agent's CLI actually reads and writes. Note the config
		// dir REPLACES ~/.claude, so the credential sits at its top level —
		// not under a .claude/ subdirectory.
		paths = append(paths, filepath.Join(claudeConfigDirPath(agentName), ".credentials.json"))
	}
	if home := AgentHome(agentName, uid, backend); home != "" {
		paths = append(paths, filepath.Join(home, ".claude", ".credentials.json"))
	}
	if !containsPath(paths, sharedClaudeCredentialPath) {
		paths = append(paths, sharedClaudeCredentialPath)
	}
	return paths
}

func containsPath(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// agentCopilotConfigPaths returns the Copilot credential locations to try for
// one agent: the per-UID config.json and token dir first, then the shared path.
func agentCopilotConfigPaths(agentName string, uid int, backend string) []string {
	paths := []string{}
	if home := AgentHome(agentName, uid, backend); home != "" {
		paths = append(paths,
			filepath.Join(home, ".copilot", "config.json"),
			filepath.Join(home, ".config", "github-copilot", "apps.json"),
			filepath.Join(home, ".config", "github-copilot", "hosts.json"),
		)
	}
	if !containsPath(paths, sharedCopilotConfigPath) {
		paths = append(paths, sharedCopilotConfigPath)
	}
	return paths
}

// agentCodexAuthPaths returns the auth.json locations to try for one Codex
// agent: the per-agent CODEX_HOME first, then the shared login location.
func agentCodexAuthPaths(agentName string, uid int, backend string) []string {
	paths := []string{}
	if uid > 0 {
		paths = append(paths, filepath.Join(codexHomePath(agentName), "auth.json"))
	} else if home := AgentHome(agentName, uid, backend); home != "" {
		paths = append(paths, filepath.Join(home, ".codex", "auth.json"))
	}
	if !containsPath(paths, codexSharedAuthFile) {
		paths = append(paths, codexSharedAuthFile)
	}
	return paths
}

func codexAuthFileHasCredentials(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return false
	}
	nonEmpty := func(v any) bool {
		s, ok := v.(string)
		return ok && strings.TrimSpace(s) != ""
	}
	if tokens, ok := data["tokens"].(map[string]any); ok {
		for _, key := range []string{"access_token", "refresh_token", "id_token"} {
			if nonEmpty(tokens[key]) {
				return true
			}
		}
	}
	for _, key := range []string{"OPENAI_API_KEY", "api_key", "openai_api_key"} {
		if nonEmpty(data[key]) {
			return true
		}
	}
	return false
}

func codexEnvHasCredentials() bool {
	return strings.TrimSpace(os.Getenv("CODEX_API_KEY")) != "" ||
		strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != ""
}

// AgentAuthState reports the auth state for ONE agent, applying the precedence
// rule documented at the top of this file. `known == false` means "no opinion"
// and the dashboard must not render a login badge.
//
// running is true when the agent's process state is running (the same signal
// /api/health/deep folds into `pass`); needsLogin is the pane poller's
// observation that the terminal is literally showing a login prompt.
func (m *Manager) AgentAuthState(agentName string, uid int, backend string, running, needsLogin bool) (available, known bool) {
	// (1) METHOD GATE — no interactive login exists for this backend, so there
	// is nothing an operator could do and a badge is always a false alarm.
	if !BackendRequiresInteractiveAuth(backend) {
		if backend == bobBackend {
			// bob's only credential is its API key; its presence is a complete
			// answer either way.
			return m.bobAPIKey() != "", true
		}
		if config.IsInferenceBackend(backend) {
			// Inference backends carry their key in config/proxy routing.
			// Authenticated by construction as far as the login badge cares.
			return true, true
		}
		// Backend we cannot introspect and that has no login flow: no opinion.
		return false, false
	}

	// (3) TRUE LOGIN SIGNAL — the pane literally shows a login prompt. This is
	// direct observation of the agent's own terminal and outranks any file.
	if needsLogin {
		return false, true
	}

	// (2) POSITIVE EVIDENCE beats absence-of-file. A running agent that is not
	// at a login prompt is working; a missing credentials file must not
	// reclassify it as "needs login". Report unknown → no badge.
	if running {
		return false, false
	}

	// (4) FILE PROBE, agent-own home first, shared legacy path second.
	switch backend {
	case "claude":
		for _, p := range agentClaudeCredentialPaths(agentName, uid, backend) {
			if claude.HasValidToken(p) {
				return true, true
			}
		}
		// A found+valid token is positive proof (above). Its ABSENCE, however, is
		// NOT proof of "needs login" for Claude: unlike copilot/codex, Claude can
		// be authenticated with NO credentials file on disk — a live in-memory
		// session, or credentials under a home this probe cannot read (per-UID
		// HOME, keychain). Reporting `known=true` here painted the amber 🔑
		// needs-login badge on Claude agents that were demonstrably authenticated
		// and mid-work — the false positive was hit whenever such an agent's
		// process state flapped to not-running (e.g. a frequently-restarting
		// agent) so the (2) running-guard above didn't fire. Report UNKNOWN
		// (no opinion → no badge) instead: a missing Claude cred file is
		// inconclusive, and inventing a login prompt that does not exist is worse
		// than showing nothing. A REAL Claude login prompt is still caught by the
		// pane-scan needsLogin signal at (3), which outranks this.
		return false, false
	case "copilot":
		m.mu.RLock()
		tok := m.copilotAuthToken
		m.mu.RUnlock()
		if tok != "" {
			return true, true
		}
		if _, err := os.Stat(CopilotUserTokenPath); err == nil {
			return true, true
		}
		for _, p := range agentCopilotConfigPaths(agentName, uid, backend) {
			if copilotCredentialFileHasTokens(p) {
				return true, true
			}
		}
		return false, true
	case "codex":
		if codexEnvHasCredentials() {
			return true, true
		}
		for _, p := range agentCodexAuthPaths(agentName, uid, backend) {
			if codexAuthFileHasCredentials(p) {
				return true, true
			}
		}
		return false, true
	default:
		// gemini and any other interactive backend: we have no reliable probe,
		// so express no opinion rather than inventing a false "needs login".
		return false, false
	}
}

// AgentAuthAvailable is the per-agent provider the dashboard registers. It
// resolves the agent's live process state itself so callers only need a name.
func (m *Manager) AgentAuthAvailable(agentName string) (available, known bool) {
	m.mu.RLock()
	proc := m.agents[agentName]
	m.mu.RUnlock()
	if proc == nil {
		return false, false
	}
	backend := proc.Config.Backend
	if proc.BackendOverride != "" {
		backend = proc.BackendOverride
	}
	proc.paneMu.RLock()
	needsLogin := proc.NeedsLogin
	proc.paneMu.RUnlock()
	return m.AgentAuthState(agentName, proc.UID, backend, proc.State == StateRunning, needsLogin)
}
