package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/claude"
	"github.com/hivecommons/hive/pkg/config"
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
//
// Per-UID interactive agents get a PER-AGENT home (#4596 — the shared
// /data/home let any agent's atomic .claude.json rewrite sign out the whole
// fleet); inference agents keep their /tmp-based per-agent home; the
// HIVE_SHARED_AGENT_HOME=1 escape hatch restores the legacy shared layout.
func AgentHome(agentName string, uid int, backend string) string {
	if uid <= 0 {
		if h := os.Getenv("HOME"); h != "" {
			return h
		}
		return sharedAgentHome
	}
	if IsInferenceBackend(backend) {
		return inferenceHomePath(agentName)
	}
	if sharedAgentHomeForced() {
		return sharedAgentHome
	}
	return interactiveHomePath(agentName)
}

// agentClaudeCredentialPaths returns the .credentials.json locations to try for
// one agent, agent-own home FIRST (per-UID layout) then the shared legacy path.
// Ordering matters: the per-UID file is the one the agent's CLI actually writes.
func agentClaudeCredentialPaths(agentName string, uid int, backend string) []string {
	paths := []string{}
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

// ClaudeCredentialCandidatePaths returns every .credentials.json worth trying
// for a BACKEND-level "is claude authenticated on this hive?" question —
// per-UID agent homes first, the shared legacy path last.
//
// It exists so that question has exactly ONE answer (#4699). The per-UID fix
// this file's header describes was applied here and NOT to the dashboard's
// model-discovery probe, which kept stat'ing only the shared path. On a hive
// where the two disagree the operator sees a self-contradicting UI: the agent
// card reads "✓ logged in" from this probe while every model in the dropdown
// reads "(common alias, unverified)" from that one. Both now resolve paths
// through this function, so they cannot drift apart again.
//
// Backend-level, so ANY agent's fresh token answers it: a model catalog is a
// property of the provider, not of the agent that asked. Ordering is
// deterministic (agents sorted by name) because m.agents iterates randomly and
// a probe that consulted a different credential on each call would be
// untestable and would make an expired token an intermittent failure.
//
// A nil Manager yields the shared path alone, which is the pre-per-UID
// behavior — callers without an agent manager are no worse off than before.
func (m *Manager) ClaudeCredentialCandidatePaths() []string {
	if m == nil {
		return []string{sharedClaudeCredentialPath}
	}

	type agentRef struct {
		name    string
		backend string
		uid     int
	}
	m.mu.RLock()
	refs := make([]agentRef, 0, len(m.agents))
	for name, proc := range m.agents {
		if proc == nil {
			continue
		}
		backend := proc.Config.Backend
		if proc.BackendOverride != "" {
			backend = proc.BackendOverride
		}
		if backend != "claude" {
			continue
		}
		refs = append(refs, agentRef{name: name, backend: backend, uid: proc.UID})
	}
	m.mu.RUnlock()
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })

	paths := []string{}
	for _, r := range refs {
		for _, p := range agentClaudeCredentialPaths(r.name, r.uid, r.backend) {
			if !containsPath(paths, p) {
				paths = append(paths, p)
			}
		}
	}
	// Always reachable even with no claude agents registered: a hive that has
	// not started its agents yet still has a shared login worth checking.
	if !containsPath(paths, sharedClaudeCredentialPath) {
		paths = append(paths, sharedClaudeCredentialPath)
	}
	return paths
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

	// (4) FILE PROBE, agent-own home first, shared legacy path second. The
	// POSITIVE half lives in credentialFileProves so the login detector can ask
	// the same question without inheriting rules 1-3 (#5291).
	proven := m.credentialFileProves(agentName, uid, backend)
	switch backend {
	case "claude":
		if proven {
			return true, true
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
		if proven {
			return true, true
		}
		return false, true
	case "codex":
		if proven {
			return true, true
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

// credentialFileProves answers ONLY the positive half of the file probe: is
// there, right now, on-disk (or in-process) evidence that this agent's backend
// is authenticated?
//
// It is deliberately one-directional. `false` means "no proof", NOT "logged
// out" — Claude in particular can be authenticated with no credentials file
// this process can read (a live in-memory session, a per-UID HOME, a keychain),
// which is why AgentAuthState reports UNKNOWN rather than needs-login when this
// comes back false for claude.
//
// Split out of AgentAuthState for kubestellar/hive#5291. The login detector
// needs this question and must NOT get AgentAuthState's answer, whose
// precedence rules are built for a dashboard badge: rule 2 short-circuits to
// "unknown" for any RUNNING agent (the detector only ever scans running
// agents), and rule 3 lets the pane's own login text outrank the credential
// file (which is precisely the evidence the detector must not trust on its
// own). Sharing the code rather than copying it keeps the two answers from
// drifting the way #4699 describes.
func (m *Manager) credentialFileProves(agentName string, uid int, backend string) bool {
	switch backend {
	case "claude":
		for _, p := range agentClaudeCredentialPaths(agentName, uid, backend) {
			if claude.HasUsableToken(p) {
				return true
			}
		}
	case "copilot":
		if m != nil {
			m.mu.RLock()
			tok := m.copilotAuthToken
			m.mu.RUnlock()
			if tok != "" {
				return true
			}
		}
		if _, err := os.Stat(copilotUserTokenProbePath); err == nil {
			return true
		}
		for _, p := range agentCopilotConfigPaths(agentName, uid, backend) {
			if copilotCredentialFileHasTokens(p) {
				return true
			}
		}
	case "codex":
		if codexEnvHasCredentials() {
			return true
		}
		for _, p := range agentCodexAuthPaths(agentName, uid, backend) {
			if codexAuthFileHasCredentials(p) {
				return true
			}
		}
	}
	return false
}

// AgentHasValidCredential reports whether this agent's backend is DEMONSTRABLY
// authenticated right now (kubestellar/hive#5291).
//
// Positive evidence only, and that asymmetry is the whole point. The login
// detector uses it to decide when NOT to pause: proof of a working credential
// means a login prompt on screen is residue or a stuck CLI, which is the
// token-restart heal's job, not an operator's. Anything less than proof —
// including "we cannot check this backend at all" — returns false and leaves
// the detector's existing behaviour untouched.
//
// One honest limitation: only claude's credential carries an expiry this can
// verify (claude.HasUsableToken). Copilot and codex are checked for the
// PRESENCE of tokens, so a stale-but-present copilot token reads as valid here.
// That is the same trade the manager's own token-restart heal already makes in
// configHasTokens(), and it fails in the safe direction for this caller: the
// heal restarts the CLI, which is a recovery attempt, rather than the detector
// pausing the agent, which is not.
//
// For claude the question asked is "can a restart still use this?", not "is
// the access token live right now?". An access token that has aged out under a
// long-running session leaves a refresh grant that the next CLI start redeems,
// so proof survives a routine expiry — which is the state a busy hive spends
// part of every day in.
func (m *Manager) AgentHasValidCredential(agentName string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	proc := m.agents[agentName]
	m.mu.RUnlock()
	if proc == nil {
		return false
	}
	backend := proc.Config.Backend
	if proc.BackendOverride != "" {
		backend = proc.BackendOverride
	}
	return m.credentialFileProves(agentName, proc.UID, backend)
}
