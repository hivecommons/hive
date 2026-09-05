// Agent launch command assembly and in-pane failure banners: backend
// launch command construction, tool-rule flags, host-state deny flags,
// the caveman installer, and startup-kick deferral.
package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// backendDefersStartupKick reports whether a backend's bootstrap prompt is
// delivered AFTER the CLI is ready (deliverStartupKick) instead of being
// embedded in the launch command.
//
// Embedding raced the CLI boot: the prompt-bearing launch line was typed into
// the pane in the same second as the (re)start (observed live: `audit: agent
// kicked trigger=startup` 60ms after `audit: agent restarting`), before the
// CLI — or even bash — was ready to consume it, so the kick text landed in
// bash and an unbalanced quote left the shell in PS2 continuation.
//
// goose is deliberately NOT in this set: `goose run` needs the embedded --text
// prompt to stay interactive at all, and it exits on the ^C that
// readiness-gated delivery sends (see deliverKickLocked). Unknown backends are
// likewise excluded — they never embed and have no verified readiness signal.
//
// bob and pi belong in this set, not the goose set. Both are long-lived
// interactive TUIs that sit at their prompts with no
// prompt argument at all, so it has no goose-style reason to embed — and
// embedding would expose it to exactly the PS2 race above. Before this it was
// in NEITHER group: it fell through to the write-a-file branch in launchInTmux,
// so its bootstrap prompt was serialized to /tmp/.hive-bootstrap-<name>.txt and
// then never read, leaving the CLI idle at its prompt after every launch.
func backendDefersStartupKick(backend string) bool {
	switch backend {
	case "claude", "copilot", "gemini", "pi", bobBackend:
		return true
	default:
		return false
	}
}

// copilotGitHubWriteDenyFlags and claudeGitHubWriteDenyFlags are defined together
// near the bottom of this file (alongside the codex/bob backend constants). v2
// independently added a copy of copilotGitHubWriteDenyFlags here; the v4 grouped
// definition (which also carries claudeGitHubWriteDenyFlags) is kept as the single
// source of truth, so this duplicate was dropped in the v2→v4 sync merge.

func (m *Manager) launchInTmux(ctx context.Context, agent *AgentProcess) error {
	if ctx == nil {
		ctx = context.Background()
	}
	backend := agent.Config.Backend
	if agent.BackendOverride != "" {
		backend = agent.BackendOverride
	}

	binary := ""
	if strings.TrimSpace(agent.Config.LaunchCmd) == "" {
		var err error
		binary, err = m.backendBinary(backend)
		if err != nil {
			agent.State = StateFailed
			m.recordStartFailureLocked(agent, backend, StartFailureBinaryMissing, err.Error())
			m.logger.Warn("backend binary not found", "name", agent.Name, "backend", backend, "error", err)
			// The tmux session already exists (Start/Restart ran ensureTmuxSession
			// before this), so without a banner the pane is a silent bare shell —
			// the operator attaches and sees a prompt, not a failure. Say which
			// binary was attempted, that it is missing, and what to do.
			m.announceLaunchFailureInPane(agent, m.backendLaunchFailureMessage(backend, err))
			return nil
		}
	}

	// bob cannot authenticate without an API key in a pod: its default W3ID SSO
	// flow opens a browser and polls a localhost callback port, then fails with
	// "Authentication timeout (3 minutes)". Refuse to launch instead of burning
	// three minutes per attempt on a flow that cannot succeed here. Mirrors the
	// missing-binary handling above: mark the agent failed, log actionably, and
	// return nil so one misconfigured agent never aborts the fleet.
	// The key VALUE is not bound to a local here — only its presence is checked.
	// It is delivered to the CLI via tmux set-environment in agentEnvPairs.
	// Any launch attempt supersedes a previous missing-key parking: either the
	// key is present now (cleared below) or we re-park on this same branch.
	// Clearing unconditionally keeps the flag from outliving the condition it
	// describes — a stale true would make a later key-save relaunch an agent
	// that is actually failed for some other reason.
	agent.awaitingBobKey = false
	if backend == bobBackend {
		if m.bobAPIKey() == "" {
			agent.State = StateFailed
			agent.awaitingBobKey = true
			m.recordStartFailureLocked(agent, backend, StartFailureCredentialMissing, config.BobAPIKeyEnvVar)
			m.logger.Warn("bob requires "+config.BobAPIKeyEnvVar+" for headless operation; ask your hub admin to configure it",
				"name", agent.Name,
				"backend", backend,
				"remedy", "set governor.bob.api_key_file (e.g. "+config.DefaultBobAPIKeyFile+
					") or the "+config.DefaultBobAPIKeyEnv+" env var on the hive pod",
			)
			// Same silent-bare-shell hazard as the missing-binary branch above:
			// the session exists, nothing was typed, and the log line is
			// invisible from the terminal. Name the missing credential (never
			// its value) and the remedy in the pane itself.
			m.announceLaunchFailureInPane(agent, fmt.Sprintf(
				"backend bob did not launch: no API key is configured (%s). Save an IBM bob API key in the dashboard under Governor -> Bob — parked bob agents relaunch automatically when a key is saved.",
				config.BobAPIKeyEnvVar))
			return nil
		}
		// Re-assert hive's ownership of the SHARED /data/home/.bob/settings.json
		// auth block BEFORE bob starts. A persisted selectedType beats
		// BOBSHELL_DEFAULT_AUTH_TYPE, so without this one agent that ever picked
		// SSO leaves every bob agent on the hive stuck at the auth prompt.
		//
		// NOTE: this writes /data/home/.bob/settings.json, which is on the NFS
		// RWX PVC — NOT "locals" as an earlier comment claimed. It takes no
		// Manager lock of its own, but Start still calls launchInTmux with m.mu
		// held (Phase 3), so an NFS stall here CAN block AllStatuses() for the
		// NFS timeout. This is the narrower residual left after the Phase-2
		// hoist (ensureTmuxSession/sanitizeGitRemotes/token writes are already
		// off the lock); moving bob's /data/home pre-flight off the lock too is
		// a follow-up for a separate maintainer decision.
		m.ensureBobAuthSettings(agent.Name, bobSharedHome)
		// The key resolved above was read by the HIVE process as dev. bob will
		// read it as the AGENT UID, which is a different question — and the one
		// that actually failed in production. Probe it and log actionably.
		// Advisory only: the Secret-mounted copy may still be readable, so this
		// never blocks a launch that might succeed.
		m.verifyBobKeyReadable(agent.Name, m.bobKeyFilePath(), agent.UID)
		// bob reports unwritable state dirs only inside its own TUI, so probe
		// them here as the agent UID and surface any failure in the hive log.
		// Advisory, like the key probe above — never blocks a launch.
		_ = m.verifyBobStateDirsWritable(agent.Name, bobSharedHome, m.workDir+"/"+agent.Name, agent.UID)
	}

	var launchCmd string
	model := agent.Config.Model
	if agent.ModelOverride != "" {
		model = agent.ModelOverride
	}
	modelIn := model
	isInference := m.routableBackend(backend)
	model = normalizeModelNameForBackend(model, backend, isInference)

	bootstrapPrompt := agent.BootstrapOverride
	if bootstrapPrompt != "" {
		m.logger.Info("using bootstrap override", "agent", agent.Name, "len", len(bootstrapPrompt))
		agent.BootstrapOverride = ""
	} else {
		bootstrapPrompt = m.buildBootstrapPrompt(agent)
	}

	mode := m.agentMode(agent)
	agent.LaunchedMode = mode
	agent.HasLaunched = true

	// Inference backends and configured gateway names use Claude Code as the CLI
	// tool and route API traffic through the proxy to the selected endpoint.
	if isInference {
		binary = "claude"
		m.ensureClaudeSettings(agent.Name, agent.UID)
		if m.inferenceRouteCallback != nil {
			// inference-model-passthrough: the model set here becomes the
			// outbound OpenAI "model" field that the gateway checks for
			// entitlement, so it must equal the configured model verbatim.
			// Log in->out (never keys) so a mismatch is greppable.
			m.logger.Info("inference route model passthrough",
				"agent", agent.Name, "backend", backend,
				"model_in", modelIn, "model_out", model)
			m.inferenceRouteCallback(agent.Name, backend, model)
		}
		backend = "claude"
	} else if m.clearInferenceRouteCallback != nil {
		m.clearInferenceRouteCallback(agent.Name)
	}
	if backend == "claude" && !isInference {
		m.ensureClaudeRemoteControlDefault(agent)
	}

	if agent.Config.CavemanMode != "" {
		m.installCavemanForAgent(agent, backend)
	}

	if strings.TrimSpace(agent.Config.LaunchCmd) != "" {
		launchCmd = strings.TrimSpace(agent.Config.LaunchCmd)
	} else if agent.Config.Tools != nil {
		launchCmd = toolRulesToLaunchCmd(binary, model, backend, agent.Config.Tools, isInference)
		if agent.Config.Tools != nil && agent.Config.Mode != "" {
			m.logger.Warn("agent has both tools and mode set; tools takes precedence", "agent", agent.Name)
		}
	} else {
		launchCmd = backendLaunchCmd(binary, model, backend, isInference)
	}

	if mcpFlags := connectionMCPFlags(agent.Config.Connections, backend); mcpFlags != "" {
		launchCmd += mcpFlags
	}

	if bootstrapPrompt == "" && isInference {
		bootstrapPrompt = "You are an AI agent. Await further instructions."
	}

	// See backendDefersStartupKick for why each backend is or is not deferred.
	deferredStartupKick := ""
	if bootstrapPrompt != "" && backendDefersStartupKick(backend) {
		deferredStartupKick = bootstrapPrompt
		bootstrapPrompt = ""
	}

	if bootstrapPrompt != "" {
		now := time.Now()
		agent.LastKick = &now
		agent.LastKickMessage = bootstrapPrompt
		agent.kickLogPending = true
		snippet := bootstrapPrompt
		const maxBootstrapSnippet = 200
		snippet = truncateStr(snippet, maxBootstrapSnippet)
		agent.KickHistory = append(agent.KickHistory, KickRecord{Timestamp: now, Agent: agent.Name, Snippet: snippet})
		m.logger.Info("audit: agent kicked",
			"name", agent.Name,
			"message_len", len(bootstrapPrompt),
			"preview", snippet,
			"trigger", "startup",
		)
		m.recordPrompt(agent.Name, "startup", bootstrapPrompt)

		// Only goose (and unknown backends, which never embed) reach this
		// block — claude/copilot/gemini/bob bootstrap prompts were deferred to
		// deliverStartupKick above, so no /tmp/.hive-bootstrap-<name>.txt is
		// written for them. bob used to land here and get a file nothing ever
		// read; that dead write is gone with the deferral above.
		promptFile := filepath.Join(agentStateDir, fmt.Sprintf(".hive-bootstrap-%s.txt", agent.Name))
		// N15: owner-only + O_NOFOLLOW (see writeAgentStateFile).
		if err := writeAgentStateFile(promptFile, []byte(bootstrapPrompt)); err != nil {
			m.logger.Warn("failed to write bootstrap prompt", "name", agent.Name, "error", err)
		} else if backend == "goose" {
			launchCmd += fmt.Sprintf(" --text \"$(cat %s)\"", promptFile)
		}
	}

	// Goose 1.37 requires --instructions or --text to stay interactive.
	// Without bootstrap, use a minimal --text prompt so goose output is
	// visible to tmux capture-pane (--instructions - uses hidden TUI).
	if backend == "goose" && bootstrapPrompt == "" {
		minimalPrompt := filepath.Join(agentStateDir, fmt.Sprintf(".hive-bootstrap-%s.txt", agent.Name))
		if err := writeAgentStateFile(minimalPrompt, []byte("You are an AI agent. Wait for instructions from the supervisor.")); err != nil {
			m.logger.Warn("failed to write minimal bootstrap prompt", "name", agent.Name, "error", err)
		}
		launchCmd += fmt.Sprintf(" --text \"$(cat %s)\"", minimalPrompt)
	}

	if !agent.forceRelaunch && m.tmuxPaneHasCLIForAgent(agent) {
		m.logger.Info("CLI already running in tmux pane, skipping launch", "name", agent.Name, "session", agent.tmuxSession)
		now := time.Now()
		agent.State = StateRunning
		agent.LastError = ""
		agent.lastLaunchFailureBanner = ""
		agent.StartedAt = &now

		agentCtx, cancel := context.WithCancel(ctx)
		agent.cancel = cancel
		go m.pollTmuxOutputForAgent(agent, agentCtx)

		if backendHasBlockingPrompts(backend) {
			go m.watchForTrustPromptForAgent(agent, agentCtx)
		}
		if isInference {
			// The surviving pane may be parked on a consent screen (e.g.
			// the hub restarted while the CLI awaited consent). The marker
			// check above cannot tell the difference, so re-arm dismissal;
			// it exits quickly once the main prompt is visible.
			go m.dismissInferencePrompts(agent)
		}
		return nil
	}
	agent.forceRelaunch = false

	// Single-CLI guarantee: reap any pre-existing or leaked CLI for this agent
	// before launching a new one. Without this a relaunch (model/backend switch,
	// crash-restart) spawns a second claude alongside the old one — the old
	// process keeps hitting the gateway on a stale model and 403-floods the
	// pane. The reaper matches by HIVE_AGENT env, so it also catches a process
	// that survived tmux kill-session by detaching from the pane. Runs on every
	// real launch (the CLI-already-running early return above skips it, keeping
	// the healthy single CLI).
	if reaped := m.reapAgentCLI(agent); reaped > 0 {
		m.logger.Info("reaped stale CLI before launch",
			"name", agent.Name, "reaped", reaped, "session", agent.tmuxSession)
		// Give the kernel a moment to tear down the killed process so the new
		// launch starts from a clean slate (no lingering socket on the gateway).
		time.Sleep(preLaunchShellClearDelay)
	}

	m.fixSharedConfigPerms(agent)

	// NOTE on Copilot's "Confirm folder trust" modal (≥1.0.78): #4563 tried to
	// pre-seed config.json trusted_folders here so it never appears. That does
	// NOT work — the shared config is rewritten wholesale by every running CLI
	// from its own stale in-memory snapshot, so seeded entries are stomped
	// within minutes (traced live 2026-08-22). The modal is instead answered
	// session-only ("1. Yes") by watchForTrustPromptForAgent, which now runs
	// for the agent's whole lifetime, and the login-detector stands down while
	// the modal is on screen (PaneShowsBlockingPrompt).

	// Re-apply SECRET env vars before every launch. ensureTmuxSession sets the
	// full env via tmux set-environment, but it returns early when the session
	// already exists, so on a relaunch (restart, model change, crash recovery)
	// those values are never refreshed. Non-secret vars survive that because
	// buildEnvPrefix re-types them on the command line each launch, and
	// COPILOT_GITHUB_TOKEN survives because it is also in the hive process env
	// and inherited — but a key resolved from a Secret/PVC FILE is in neither,
	// so without this it would reach the CLI on a session's first launch only.
	// set-environment is idempotent and never appears in the pane.
	m.applySecretEnv(agent)

	envCmd := m.buildEnvPrefix(agent)
	fullCmd := envCmd + launchCmd

	// A previously spilled kick can leave bash in PS2 quote-continuation
	// (an unbalanced quote): anything typed next is appended to the open
	// string literal instead of executing, so the launch command would be
	// silently eaten. Abort any pending continuation or partially typed
	// line before typing the launch command. The pane holds only bash at
	// this point (the CLI-already-running check above returned early), so
	// C-c cannot kill a live CLI.
	m.tmuxSendKeysForAgent(agent, "C-c")
	time.Sleep(preLaunchShellClearDelay)

	m.tmuxSendLiteralForAgent(agent, fullCmd)
	time.Sleep(textToEnterDelay)
	m.tmuxSendEntersForAgent(agent)

	if isInference {
		go m.dismissInferencePrompts(agent)
	}

	now := time.Now()
	agent.State = StateRunning
	// A prior aborted launch is history the moment a real one succeeds; a
	// stale "no API key" reason lingering after a key-save relaunch would
	// send an operator chasing a problem that no longer exists.
	agent.LastError = ""
	agent.lastLaunchFailureBanner = ""
	agent.StartedAt = &now
	agent.launchGen++
	m.logger.Info("audit: agent started",
		"name", agent.Name,
		"backend", backend,
		"model", model,
		"mode", mode.String(),
		"session", agent.tmuxSession,
	)
	m.audit(AuditAgentStarted, agent.Name, auditFields(
		"outcome", "success",
		"backend", backend,
		"model", model,
		"mode", mode.String(),
	))

	agentCtx, cancel := context.WithCancel(ctx)
	agent.cancel = cancel
	go m.pollTmuxOutputForAgent(agent, agentCtx)

	if backendHasBlockingPrompts(backend) {
		go m.watchForTrustPromptForAgent(agent, agentCtx)
	}

	// Deliver the bootstrap prompt once the CLI is ready — fire-and-forget,
	// same semantics as the old embedded delivery but gated on readiness.
	if deferredStartupKick != "" {
		go m.deliverStartupKick(agent, deferredStartupKick, agent.launchGen)
	}

	if agent.Config.CavemanMode != "" {
		switch backend {
		case "goose", "codex", "aider":
			go func(a *AgentProcess, cavemanMode string) {
				// Same readiness gate as kicks: a fixed post-launch delay
				// raced the CLI boot and could type /caveman into bash.
				if !m.waitForInputPromptForAgent(a) {
					m.logger.Warn("caveman activation skipped: CLI never reached input prompt",
						"agent", a.Name, "mode", cavemanMode)
					return
				}
				m.tmuxSendLiteralForAgent(a, "/caveman "+cavemanMode)
				time.Sleep(textToEnterDelay)
				m.tmuxSendEntersForAgent(a)
				m.logger.Info("sent caveman activation", "agent", a.Name, "mode", cavemanMode)
			}(agent, agent.Config.CavemanMode)
		}
	}

	return nil
}

// installCavemanForAgent runs the backend-specific caveman installer before
// the agent CLI starts. Auto-activating backends (claude, copilot, gemini)
// get caveman wired in so it's active from message one. Per-session backends
// (goose, codex, aider) get the skill pre-installed; activation happens via
// /caveman command sent after launch.
func (m *Manager) installCavemanForAgent(agent *AgentProcess, backend string) {
	mode := agent.Config.CavemanMode
	if mode == "" {
		return
	}

	// AgentHome so caveman's ~/.claude / ~/.copilot writes land in the same
	// HOME the agent's CLI runs with (per-agent layout, bridged to shared).
	home := AgentHome(agent.Name, agent.UID, backend)
	if agent.UID == 0 {
		home = os.Getenv("HOME")
		if home == "" {
			home = "/root"
		}
	}

	m.logger.Info("installing caveman", "agent", agent.Name, "backend", backend, "mode", mode)

	// The installer must run AS THE AGENT USER, not as the hive user. Caveman
	// spawns the backend's own CLI (`claude plugin list` / `plugin install`,
	// caveman bin/install.js), and Claude Code rewrites $HOME/.claude.json
	// wholesale on every invocation. Run as the hive user with the agent's
	// HOME, that CLI cannot read the agent-owned 0600 session file, treats
	// the home as a fresh install, and replaces the signed-in session with a
	// blank skeleton — the agent drops to the login menu on its next start.
	// This is the #4596 wholesale-rewrite clobber reintroduced from inside
	// the manager, through the per-agent home layout that #4619 built to end
	// it. su-exec here matches every other command that touches the per-agent
	// HOME (tmuxCmd, setupCodexHome).
	userSpec := ""
	if agent.UID > 0 {
		userSpec = m.agentExecUserSpec(agent)
	}
	argv := cavemanInstallArgv(backend, mode, userSpec)
	if argv == nil {
		m.logger.Info("caveman not supported for backend", "backend", backend)
		return
	}

	agentDir := filepath.Join("/data/agents", agent.Name)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = agentDir
	npmCache := cavemanNpmCachePath(agentDir, agent.UID)
	cmd.Env = append(os.Environ(), "HOME="+home, "npm_config_cache="+npmCache)
	if out, err := cmd.CombinedOutput(); err != nil {
		m.logger.Warn("caveman install failed", "agent", agent.Name, "error", err, "output", string(out))
	}
}

// cavemanInstallArgv builds the argv for a backend's caveman installer, or
// nil for backends caveman does not support. A non-empty userSpec prefixes
// the command with su-exec so the whole install — including the backend CLIs
// caveman spawns — runs as the agent user (see installCavemanForAgent for
// why that is load-bearing).
func cavemanInstallArgv(backend, mode, userSpec string) []string {
	// Pinned: unpinned HEAD broke every install on 2026-07-27 when upstream
	// removed the --mode flag. Bump deliberately, after checking `--help`.
	// v1.9.1 IS commit 0d95a81d35a9 — the SHA form is deliberately not used
	// because the `skills` CLI clones with `git clone --branch <ref>`, and
	// git cannot clone a bare SHA as a branch ("Could not find remote branch
	// 0d95a81d35a9"), which broke every install on a live hive. Tags clone
	// cleanly; bump the tag, never a raw SHA.
	const cavemanRef = "github:JuliusBrussee/caveman#v1.9.1"
	const skillsRef = "JuliusBrussee/caveman#v1.9.1"
	// Upstream replaced `--mode full|minimal` with `--all` / `--minimal`
	// (--all = hooks + init).
	modeFlag := "--all"
	if mode == "minimal" {
		modeFlag = "--minimal"
	}

	var argv []string
	switch backend {
	case "claude":
		argv = []string{"npx", "-y", cavemanRef, "--", "--only", "claude", modeFlag}
	case "copilot":
		argv = []string{"npx", "-y", cavemanRef, "--", "--only", "copilot", "--with-init", modeFlag}
	case "gemini":
		argv = []string{"npx", "-y", cavemanRef, "--", "--only", "gemini", modeFlag}
	case "goose":
		argv = []string{"npx", "-y", "skills", "add", skillsRef, "-a", "goose", "-y"}
	case "codex":
		argv = []string{"npx", "-y", "skills", "add", skillsRef, "-a", "codex", "-y"}
	case "aider":
		argv = []string{"npx", "-y", "skills", "add", skillsRef, "-a", "aider-desk", "-y"}
	default:
		return nil
	}
	if userSpec != "" {
		argv = append([]string{"su-exec", userSpec}, argv...)
	}
	return argv
}

// cavemanNpmCachePath returns the npm cache dir for an agent's caveman
// install. The shared npm cache under /data/home accumulates
// content-addressed entries owned by whichever UID wrote them first; npx
// then fails with EACCES on those shards and the agent launches without its
// proxy. A per-agent cache can never collide across UIDs.
//
// Caches written before the install ran as the agent user are owned by the
// hive user, and npm EACCESes on them the same way — so a cache owned by
// another UID is removed (best-effort; the hive user owns the old one and
// can), and one that cannot be removed is sidestepped with a per-UID path
// rather than allowed to fail the install.
func cavemanNpmCachePath(agentDir string, uid int) string {
	cache := filepath.Join(agentDir, ".npm-caveman-cache")
	if uid <= 0 {
		return cache
	}
	info, err := os.Stat(cache)
	if err != nil {
		return cache
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) == uid {
		return cache
	}
	if err := os.RemoveAll(cache); err == nil {
		return cache
	}
	return fmt.Sprintf("%s-u%d", cache, uid)
}

// launchFailurePrefix opens every in-pane launch-failure banner so the line is
// unmistakable in a pane full of shell prompts and greppable in scrollback.
const launchFailurePrefix = "HIVE LAUNCH FAILED: "

// launchFailureBanner composes the exact shell line typed into a pane when a
// launch is aborted before any CLI starts. The message is wrapped in single
// quotes; any single quotes, newlines, or carriage returns inside it are
// replaced with spaces so the line can never break out of the quoting or
// leave bash in PS2 continuation (the same hazard the C-c before every real
// launch guards against).
func launchFailureBanner(msg string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch r {
		case '\'', '\n', '\r':
			return ' '
		}
		return r
	}, msg)
	return "echo '" + launchFailurePrefix + sanitized + "'"
}

// announceLaunchFailureInPane makes an aborted launch visible IN the agent's
// tmux pane. launchInTmux's park-and-return branches (missing backend binary,
// bob with no API key) run after ensureTmuxSession has already created a
// fresh shell, so returning silently leaves a bare prompt: the operator
// attaches via ttyd, sees nothing wrong, and the only explanation is in the
// hive log they are not reading. Typing an echo into the pane puts the reason
// and the remedy exactly where the operator is looking.
//
// Best-effort by design: the send-keys errors are ignored like every other
// pane write on the launch path — a missing session must not turn a parked
// agent into a crashed manager. Caller holds m.mu (same discipline as
// launchInTmux, which is its only caller).
// It is also the single chokepoint every park-and-return branch already
// passes through, so recording the durable audit event here — rather than
// once per branch — means no launch failure can be added later that silently
// skips the audit log. This is the watsonx case: an agent configured with a
// backend the image does not support failed at every launch for a day, WARN-
// logged inside the pod and invisible in the Audit Log UI.
func (m *Manager) announceLaunchFailureInPane(agent *AgentProcess, msg string) {
	agent.lastLaunchFailureBanner = launchFailureBanner(msg)
	m.tmuxSendLiteralForAgent(agent, agent.lastLaunchFailureBanner)
	m.tmuxSendKeysForAgent(agent, "Enter")

	m.audit(AuditAgentStartFailed, agent.Name, auditFields(
		"outcome", "failure",
		"backend", agent.effectiveBackend(),
		"model", agent.effectiveModel(),
		"error", agent.LastError,
	))
}

// copilotGitHubWriteDenyFlags denies EVERY GitHub MCP write tool for the copilot
// CLI. Agents must never author issues/PRs via the GitHub MCP — those calls run
// with the logged-in user's OAuth token, bypass the proxy, and skip the App gate.
// All GitHub writes route through the App-gated `gh` wrapper / hive-open-pr, which
// authors as kubestellar-hive[bot].
//
// PRIMARY defense is dropping --enable-all-github-mcp-tools from the launch
// command: Copilot CLI's built-in GitHub MCP server is READ-ONLY BY DEFAULT
// (v0.0.350+), so the write tools are never registered in the first place. These
// deny flags are belt-and-suspenders on top of that.
//
// The built-in server is named `github-mcp-server` (per `copilot --help`:
// `--disable-builtin-mcps ... (currently: github-mcp-server)`), NOT `github`.
// The prior deny names used the bare `github` server prefix, which was a SILENT
// NO-OP: `github` is only the name of a separately-added server, so a deny on it
// matched nothing and the writes stayed live. These use the CORRECT server name so the
// belt-and-suspenders denies actually match. Read tools (get_issue/list/search)
// stay available (read-only default), so no enable-all flag is needed.
const copilotGitHubWriteDenyFlags = " --deny-tool='github-mcp-server(create_pull_request)'" +
	" --deny-tool='github-mcp-server(create_pull_request_with_copilot)'" +
	" --deny-tool='github-mcp-server(merge_pull_request)'" +
	" --deny-tool='github-mcp-server(create_issue)'" +
	" --deny-tool='github-mcp-server(update_issue)'" +
	" --deny-tool='github-mcp-server(add_issue_comment)'"

// claudeGitHubWriteDenyFlags denies EVERY GitHub MCP write tool for the claude
// CLI — the same logical set as copilotGitHubWriteDenyFlags, in claude's
// --disallowed-tools 'mcp__github__<tool>' syntax. Same rationale: agents author
// as the App via the gh wrapper, never as the user via the MCP. Read tools stay
// enabled. Applied in EVERY agent mode.
const claudeGitHubWriteDenyFlags = " --disallowed-tools 'mcp__github__create_pull_request'" +
	" --disallowed-tools 'mcp__github__create_pull_request_with_copilot'" +
	" --disallowed-tools 'mcp__github__merge_pull_request'" +
	" --disallowed-tools 'mcp__github__create_issue'" +
	" --disallowed-tools 'mcp__github__update_issue'" +
	" --disallowed-tools 'mcp__github__add_issue_comment'"

// claudeHostStateDenyTools names the commands an agent editing a repo workspace
// never needs and that reach the operator's own machine. It mirrors
// CLAUDE_HOST_DENY_TOOLS in config/backends.conf so a pod agent and a relay
// agent are confined identically — the same relay/pod parity bobLaunchCmd below
// is written for.
//
// #4918: an agent doing correct work on an assigned third-party repo ran that
// repo's test suite, a hook escaped its stubs, and `rpm-ostree kargs
// --append-if-missing=...` was issued against the operator's real deployment.
// It failed only because the process lacked privilege.
//
// rpm-ostree never invoked sudo — it asked polkit directly — so denying
// escalation alone would not have stopped it. The host-state tools that reach
// polkit on their own have to be named too.
//
// Comma-separated in ONE argv word: the shell path word-splits its flag string
// (bin/agent-launch.sh), and keeping both paths on the identical spelling is
// what makes them auditable as one policy.
var claudeHostStateDenyTools = strings.Join([]string{
	// Privilege escalation.
	"Bash(sudo:*)", "Bash(pkexec:*)", "Bash(doas:*)", "Bash(su:*)",
	// Host boot/deployment state — these need no escalation of their own.
	"Bash(rpm-ostree:*)", "Bash(bootc:*)", "Bash(ostree:*)",
	"Bash(grubby:*)", "Bash(bootctl:*)", "Bash(efibootmgr:*)",
}, ",")

// hostStateBypassEnv is the opt-out, named to match Codex's
// HIVE_CODEX_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX so the two read alike. An
// operator who genuinely wants an agent to manage host state sets it and gets
// the pre-#4918 posture back.
const hostStateBypassEnv = "HIVE_CLAUDE_DANGEROUSLY_ALLOW_HOST_STATE"

// claudeHostStateDenyFlags returns the --disallowed-tools fragment, or "" when
// the operator has explicitly opted out.
//
// These are DENIALS, not a permission mode: a match is refused and the agent
// carries on. Switching the mode instead would make the CLI prompt, and nobody
// is attached to an unattended pane to answer it. Denials still apply under
// --dangerously-skip-permissions, which is the same property
// claudeGitHubWriteDenyFlags above already depends on.
func claudeHostStateDenyFlags() string {
	if hostStateBypassRequested(os.Getenv(hostStateBypassEnv)) {
		return ""
	}
	return " --disallowed-tools '" + claudeHostStateDenyTools + "'"
}

// hostStateBypassRequested mirrors is_truthy() in config/backends.conf exactly.
// The two launch paths must agree on what "on" means, or an operator who sets
// the opt-out to "yes" gets a confined relay agent and an unconfined pod agent
// from the same configuration — the worst outcome, because it looks like it
// worked.
func hostStateBypassRequested(value string) bool {
	switch value {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	default:
		return false
	}
}

// bobLaunchCmd builds bob's interactive launch command.
//
// The launch stays INTERACTIVE — no -p/--prompt — so the agent drives bob in a
// tmux pane exactly like every other CLI backend and a human can attach to it.
//
// Two flags are passed:
//
//   - --auth-method api-key: this flag DOES exist in bobshell 1.0.6. It was
//     previously removed after `bob --help | grep auth-method` returned
//     nothing, but that test is misleading: the bundle registers the option and
//     then explicitly HIDES it from help output —
//     `t.option("auth-method",{choices:[fr.W3ID_SSO,fr.USE_BOBSHELL]})` followed
//     by `["debug",...,"auth-method"].forEach(c=>t.hide(c))`. Verified by
//     running the real 1.0.6 bundle: `--help` is 67 lines with 0 matches for
//     auth-method, yet the parser distinguishes it from a typo —
//     $ bob --definitely-not-a-flag x -p hi
//     Unknown arguments: definitely-not-a-flag, definitelyNotAFlag
//     $ bob --auth-method bogus-value -p hi
//     Invalid values: Argument: auth-method, Given: "bogus-value",
//     Choices: "sso", "api-key"
//     while `--auth-method api-key` is accepted silently. An unknown flag under
//     yargs .strict() would have errored, so the option is live.
//
//     It matters because it is the ONLY input that outranks the persisted,
//     fleet-shared settings file: bob stores it as globalThis.authMethodByCliArg
//     and resolves
//     `authMethodByCliArg || merged.security.auth.selectedType || <default>`,
//     and it also suppresses the setValue() write-back that would otherwise
//     persist a competing value. BOBSHELL_DEFAULT_AUTH_TYPE
//     (config.BobAuthTypeEnvVar) is only the FALLBACK default and loses to a
//     persisted selectedType. Neither is the primary bug — an unreadable key
//     file was (see verifyBobKeyReadable) — but both are real, and this flag is
//     the cheapest guarantee that a stale settings file cannot re-break bob.
//     Note IBM's public docs also document `--auth-method api-key`.
//
//   - --accept-license: bob hard-errors ("A license agreement is required.
//     Please accept the license terms before proceeding.") before doing any
//     work unless licenseConsent is already persisted in its settings. That
//     consent is normally collected from a human at an interactive prompt that
//     nobody can answer in an unattended pod, and it is stored under
//     $HOME/.bob, so it is lost whenever the PVC is reset. Passing it on every
//     launch is idempotent (it only sets licenseConsent=true) and is the
//     vendor-documented non-interactive path. An operator configuring an API
//     key for unattended use is the act of acceptance; the text stays
//     reviewable via `bob --show-license`.
//
//   - --approval-mode yolo (config.BobApprovalModeFlag/BobApprovalModeYolo):
//     without it bob runs in its "default" approval mode, the TUI reports
//     `Auto-approve: Off`, and the agent blocks forever on its FIRST tool call
//     waiting for a human who is not attached. Verified live on a spoke: with
//     the flag the TUI reports `Auto-approve: Full` and bob executed a shell
//     tool unattended.
//
//     This is deliberately FLAT — not gated on m.agentMode(agent) — because it
//     matches the existing fleet posture rather than inventing a new one for
//     bob. Every other backend already auto-approves tools at EVERY ACMM level:
//     claude gets --dangerously-skip-permissions and copilot gets --allow-all
//     in all three mode branches of launchInTmux. Hive does not restrain agents
//     by making them ask permission for local tool calls; it restrains them by
//     (a) denying specific GitHub write tools per mode and (b) unsetting
//     GH_TOKEN/GITHUB_TOKEN for agents whose mode fails CanPush(). Both of
//     those controls apply to bob unchanged and are where an advisory bob is
//     actually contained. Giving bob a per-mode approval policy would make it
//     the only backend that stalls at low ACMM levels — less capable than its
//     peers, and stalled rather than safely limited.
//
//   - --trust (config.BobTrustFlag): bob otherwise treats the agent workdir as
//     untrusted ("This folder is not trusted. Some features may be disabled.")
//     and gates tool availability on it. See BobTrustFlag for why the flag is
//     preferred over seeding the shared $HOME/.bob/trustedFolders.json.
//
// No --model is passed, and that is load-bearing. bob auto-selects its own
// model, and hive's normalizeModelName rewrites a trailing -<digits> to
// .<digits> for every backend except claude/copilot/inference (copilot uses
// alias-based canonicalization instead, see CanonicalizeCopilotModel), so a
// configured `claude-sonnet-4-6` reached bob as `claude-sonnet-4.6` — an id
// bob's backend
// does not know. Its model config came back undefined and every prompt died
// with "🛑 Cannot read properties of undefined (reading 'maxTokens')". Verified
// live: the same bob with no --model runs inference successfully.
//
// The API key itself is NOT a flag — it is delivered out-of-band via
// tmux set-environment (see agentEnvPairs) so it never lands in the command
// line, `ps` output, or pane scrollback.
//
// The model parameter is intentionally absent from the signature so no future
// caller can reintroduce the crash by passing one.
func bobLaunchCmd(binary string) string {
	return fmt.Sprintf("%s --accept-license %s %s %s %s %s",
		binary,
		config.BobAuthMethodFlag, config.BobAuthTypeAPIKey,
		config.BobApprovalModeFlag, config.BobApprovalModeYolo,
		config.BobTrustFlag)
}

// toolRulesToLaunchCmd builds a backend-specific CLI command from ToolsConfig.
func toolRulesToLaunchCmd(binary, model, backend string, tools *config.ToolsConfig, isInference bool) string {
	denies := tools.DenyPatterns()

	switch backend {
	case bobBackend:
		// bob has no deny-tool flag, so ToolsConfig cannot be expressed here.
		// It must still go through bobLaunchCmd rather than falling to the
		// default branch below, which would append `--model` and crash bob
		// with "Cannot read properties of undefined (reading 'maxTokens')".
		// A bob agent with tools configured therefore launches identically to
		// one without; the deny patterns are silently inapplicable, exactly as
		// they already were before this branch existed.
		return bobLaunchCmd(binary)
	case "claude":
		bareFlag := ""
		if isInference {
			bareFlag = fmt.Sprintf(" --bare --settings %s", claudeInferenceSettingsPath)
		}
		cmd := fmt.Sprintf("%s --model %s --dangerously-skip-permissions%s", binary, model, bareFlag)
		for _, p := range denies {
			cmd += fmt.Sprintf(" --disallowed-tools '%s'", p)
		}
		return cmd
	case "copilot":
		// Never pass --enable-all-github-mcp-tools: Copilot CLI's built-in GitHub
		// MCP server is READ-ONLY BY DEFAULT (v0.0.350+), so the write tools are
		// never registered and read tools stay available. Enabling it would turn
		// writes on and let agents author as the login USER via the MCP.
		cmd := fmt.Sprintf("%s --model %s --no-auto-update --allow-all", binary, model)
		for _, p := range denies {
			// Translate claude-style `mcp__github__<tool>` deny patterns to
			// copilot's built-in server syntax `github-mcp-server(<tool>)`. The
			// server is named `github-mcp-server` (per `copilot --help`), NOT
			// `github` — the old `github(` name matched nothing (silent no-op).
			copilotPattern := strings.ReplaceAll(p, "mcp__github__", "github-mcp-server(")
			if strings.HasPrefix(copilotPattern, "github-mcp-server(") {
				copilotPattern += ")"
			}
			cmd += fmt.Sprintf(" --deny-tool='%s'", copilotPattern)
		}
		return cmd
	default:
		cmd := binary
		if model != "" {
			cmd = fmt.Sprintf("%s --model %s", binary, model)
		}
		return cmd
	}
}

// backendLaunchCmd builds the per-backend CLI command used when an agent has no
// explicit ToolsConfig. It is the default-path counterpart to
// toolRulesToLaunchCmd and is deliberately pure — no Manager, no tmux, no
// process — so the flag contract each backend depends on can be asserted
// directly in tests instead of by polling a live pane for typed output.
func backendLaunchCmd(binary, model, backend string, isInference bool) string {
	var launchCmd string
	switch backend {
	case "claude":
		bareFlag := ""
		if isInference {
			bareFlag = fmt.Sprintf(" --bare --settings %s", claudeInferenceSettingsPath)
		}
		base := fmt.Sprintf("%s --model %s --dangerously-skip-permissions%s", binary, model, bareFlag)
		// Deny ALL GitHub MCP write tools in EVERY mode: agents author via the
		// App-gated gh wrapper, never as the user via the MCP. Mode governs the
		// gh-wrapper/proxy layer only, not what the MCP may write.
		launchCmd = base + claudeGitHubWriteDenyFlags + claudeHostStateDenyFlags()
	case "copilot":
		// model arrives here already canonicalized by normalizeModelName
		// (CanonicalizeCopilotModel: separator drift like claude-fable.5 is
		// normalized to the CLI-accepted claude-fable-5, #4262) and is then
		// passed as-is to `copilot --model %s`. It may be a
		// concrete id OR the auto-selection sentinel "auto" (copilotAutoModel
		// in cli_models.go), which lets the Copilot CLI pick/adjust the model
		// per task. Nothing here assumes a concrete id, so the sentinel flows
		// through unchanged.
		// PRIMARY defense against authoring as the login USER via the MCP:
		// we do NOT pass --enable-all-github-mcp-tools. Copilot CLI's built-in
		// GitHub MCP server is READ-ONLY BY DEFAULT (v0.0.350+), so the write
		// tools (create_issue/create_pull_request/…) are never registered.
		// READ tools (get_issue/list/search) stay available in that read-only
		// default, so nothing here disables useful lookups. All GitHub writes
		// must go through the App-gated gh wrapper / hive-open-pr.
		// copilotGitHubWriteDenyFlags is applied as belt-and-suspenders (with
		// the CORRECT `github-mcp-server(` server name) on top of the read-only
		// default. This is identical across ModeIssuesAndPRs / ModeIssuesOnly /
		// advisory — the mode never changes what the MCP can write (it never
		// legitimately should), it only governs the separate, unchanged
		// gh-wrapper/proxy layer that still reads Mode for the App-gated writes.
		launchCmd = fmt.Sprintf("%s --model %s --no-auto-update --allow-all%s",
			binary, model, copilotGitHubWriteDenyFlags)
	case "gemini":
		launchCmd = fmt.Sprintf("%s --model %s", binary, model)
	case "agy":
		// Antigravity CLI (Google's Gemini CLI replacement). Needs
		// --dangerously-skip-permissions or it blocks on a per-tool
		// approval prompt that no one is attached to answer — the same
		// contract as claude's bypass flag, and the value already used for
		// agy in config/backends.conf.
		//
		// An unrecognised --model is NOT fatal here: agy warns
		// ("model X is not recognized ... Using \"Gemini 3.6 Flash\"
		// instead") and continues on its default, so a stale model carried
		// over from another provider degrades to a warning rather than a
		// dead agent.
		//
		// --effort is REQUIRED whenever --model is given. Without it agy
		// warns "--model <m> requires --effort (available: low, medium,
		// high)" and silently ignores the model, so the configured model
		// would never actually take effect. "low" matches the effort agy
		// itself falls back to, keeping behaviour unchanged while making
		// the model selection real.
		launchCmd = fmt.Sprintf("%s --dangerously-skip-permissions", binary)
		if model != "" {
			launchCmd = fmt.Sprintf("%s --model %s --effort %s", launchCmd, model, agyDefaultEffort)
		}
	case "pi":
		// pi takes the model as a CLI flag, not a subcommand. Without
		// this case the launch command never receives the configured
		// model (previously it also hit the goose binary via the alias).
		launchCmd = fmt.Sprintf("%s --model %s", binary, model)
	case "goose":
		launchCmd = fmt.Sprintf("%s run -s", binary)
		if model != "" {
			launchCmd = fmt.Sprintf("%s --model %s", launchCmd, model)
		}
	case bobBackend:
		launchCmd = bobLaunchCmd(binary)
	default:
		launchCmd = binary
	}
	return launchCmd
}

// connectionMCPFlags builds MCP-related launch flags from connection configs.
func connectionMCPFlags(conns []config.ConnectionConfig, backend string) string {
	var flags string
	for _, conn := range conns {
		if conn.Type != "mcp" || conn.URI == "" {
			continue
		}
		switch backend {
		case "claude":
			flags += fmt.Sprintf(" --mcp-server '%s'", conn.URI)
		}
	}
	return flags
}
