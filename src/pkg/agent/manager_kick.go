// Kick delivery: SendKick and the locked delivery path, startup kicks,
// explain-mode kick suffixes, inference kick stall/nudge handling, and
// kick history seeding.
package agent

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/tracing"
	"go.opentelemetry.io/otel/attribute"
)

// SetRecordPromptCallback wires a function that persists a delivered kick
// prompt (agent, trigger, full text). Safe to call at any time; a nil fn
// clears it. Never takes m.mu — see recordPromptCallback.
func (m *Manager) SetRecordPromptCallback(fn func(agent, trigger, prompt string)) {
	if fn == nil {
		m.recordPromptCallback.Store(nil)
		return
	}
	m.recordPromptCallback.Store(&fn)
}

// recordPrompt fires the record-prompt callback if one is wired. Safe to call
// with m.mu held.
func (m *Manager) recordPrompt(agent, trigger, prompt string) {
	if fn := m.recordPromptCallback.Load(); fn != nil && *fn != nil {
		(*fn)(agent, trigger, prompt)
	}
}

// SetExplainModeDefaultResolver injects the resolver for the hive-wide default
// explain mode. Called from main.go with a closure over the live config, so a
// default changed from the dashboard applies to the next kick without a
// restart. A nil fn clears it, restoring the env-only fallback.
func (m *Manager) SetExplainModeDefaultResolver(fn func() string) {
	// Atomic store — no m.mu — so explainModeDefault can be read lock-free from
	// the kick and launch paths (see explainModeDefaultResolver docs).
	if fn == nil {
		m.explainModeDefaultResolver.Store(nil)
		return
	}
	m.explainModeDefaultResolver.Store(&fn)
}

// explainModeDefault returns the hive-wide default explain mode, or "" when no
// resolver was injected. Safe to call while holding m.mu.
func (m *Manager) explainModeDefault() string {
	fnp := m.explainModeDefaultResolver.Load()
	if fnp == nil || *fnp == nil {
		return ""
	}
	return (*fnp)()
}

// notRunningReason explains WHY an agent is not in StateRunning, in terms an
// operator can act on.
//
// The old wording — "agent <name> not running" — came from this same
// State != StateRunning check and conflated three unrelated situations: an
// agent the operator deliberately paused, one that failed to launch, and one
// that was stopped or never started. During a live incident it read as a
// launch failure on agents that were paused exactly as intended, and sent the
// investigation after a tmux problem that did not exist. Naming the state, and
// the pause trigger where there is one, is the whole fix.
//
// Caller holds m.mu.
func notRunningReason(agent *AgentProcess) string {
	switch {
	case agent.Paused || agent.State == StatePaused:
		reason := "it is paused"
		if t := strings.TrimSpace(agent.PausedTrigger); t != "" {
			reason += " (by " + t + ")"
		}
		if r := strings.TrimSpace(agent.PausedReason); r != "" {
			reason += ": " + r
		}
		return reason
	case agent.State == StateFailed:
		if desc := startBlockedDescription(agent); desc != "" {
			return "it is blocked after repeated failed starts: " + desc
		}
		reason := "it failed to start"
		if e := strings.TrimSpace(agent.LastError); e != "" {
			reason += ": " + e
		}
		return reason
	case agent.State == StateStopped:
		return "it is stopped"
	case agent.State == StateIdle:
		return "it has not been started yet"
	default:
		return "it is in state " + string(agent.State)
	}
}

func (m *Manager) SendKick(name string, message string) error {
	// Agent-kick span. No-op with zero export cost when tracing is disabled
	// (the default). SendKick has no context parameter, so this span roots at
	// Background; it still captures the kick leg of the governor→agent
	// lifecycle. Ended before delivery bookkeeping via defer.
	_, span := tracing.StartSpan(context.Background(), "agent.send_kick",
		attribute.String("agent.name", name))
	defer span.End()

	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	if m.agentSandboxEnabledLocked(agent) {
		return m.startSandboxKickLocked(agent, message)
	}

	if agent.State != StateRunning {
		return fmt.Errorf("agent %s cannot be kicked: %s", name, notRunningReason(agent))
	}
	if remaining := m.providerErrorBackoffRemainingLocked(agent, time.Now()); remaining > 0 {
		return fmt.Errorf("agent %s blocked: inference (%s): %s; next provider probe in %v",
			name, agent.ProviderErrorClass, agent.ProviderErrorLine, remaining.Round(time.Second))
	}

	if !m.tmuxSessionExistsForAgent(agent) {
		return fmt.Errorf("tmux session %s not found", agent.tmuxSession)
	}

	// Detect a crashed CLI (bare shell) or a CLI stuck on a consent screen
	// and restart before sending the kick. A consent pane contains "❯" so it
	// passes the marker check, but a kick typed into it is consumed by the
	// menu — or by bash once the default "No, exit" selection exits the CLI
	// (observed live: "-bash: NEVER: command not found").
	pane := m.captureVisiblePaneForAgent(agent)
	if !paneHasCLIMarker(pane) || paneShowsConsentScreen(pane) {
		m.logger.Warn("agent CLI crashed or stuck on consent screen, restarting before kick",
			"name", name, "consent_screen", paneShowsConsentScreen(pane))
		m.mu.Unlock()
		if err := m.Restart(context.Background(), name); err != nil {
			m.mu.Lock()
			return fmt.Errorf("failed to restart crashed agent %s: %w", name, err)
		}
		if !m.waitForCLIReadyForAgent(agent) {
			m.mu.Lock()
			return fmt.Errorf("agent %s CLI did not become ready after restart", name)
		}
		m.mu.Lock()
		agent, ok = m.agents[name]
		if !ok {
			return fmt.Errorf("agent %s disappeared after restart", name)
		}
	}

	// Wait for the input prompt (❯) before sending — the CLI may be
	// showing a trust prompt or still initializing even though
	// tmuxPaneHasCLI matched a broad marker like "Copilot".
	m.mu.Unlock()
	if !m.waitForInputPromptForAgent(agent) {
		m.mu.Lock()
		return fmt.Errorf("agent %s CLI did not reach input prompt", name)
	}
	m.mu.Lock()
	agent, ok = m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s disappeared while waiting for input prompt", name)
	}

	m.deliverKickLocked(agent, message, "send-kick")

	return nil
}

// deliverKickLocked types a message into the agent's CLI and records the
// kick bookkeeping (LastKick, history, stall watchdog, audit log). Callers
// must hold m.mu and must already have verified the CLI is ready for input
// (crash detect + waitForCLIReadyForAgent + waitForInputPromptForAgent) —
// this function does no readiness checking of its own.
func (m *Manager) deliverKickLocked(agent *AgentProcess, message, trigger string) {
	// Archive the PREVIOUS kick's scrollback and clear the history before any
	// input touches the pane, so each archived kick log is cleanly delimited
	// (#4296). Must be the first thing this function does.
	m.rotateKickLogOnKickLocked(agent)

	// Clear stale input before kick (Ctrl+C then Ctrl+U).
	// Goose 1.37 exits on ^C — skip clear for goose backend.
	if agent.Config.Backend != "goose" && agent.BackendOverride != "goose" {
		m.tmuxSendKeysForAgent(agent, "C-c")
		time.Sleep(staleCheckDelay)
		m.tmuxSendKeysForAgent(agent, "C-u")
		time.Sleep(staleCheckDelay)
	}

	if agent.Config.ClearOnKick {
		m.tmuxSendLiteralForAgent(agent, "/clear")
		time.Sleep(textToEnterDelay)
		m.tmuxSendEntersForAgent(agent)
		time.Sleep(clearBeforeKickDelay)
	}

	// Weak OSS models served over inference backends often answer a kick
	// with a prose plan addressed to a reader and execute zero tool calls
	// (observed live: litellm/vllm + deepseek-r1-14b produced a coherent
	// PLAN and returned to the idle prompt without running anything).
	// Append an action-forcing block here — where the effective backend is
	// knowable — instead of editing the kick templates, which are shared
	// with commercial CLI backends that do not need it.
	message = kickMessageWithSuffixes(message,
		IsInferenceBackend(effectiveBackend(agent)),
		resolveExplainMode(agent.Config, m.explainModeDefault()))

	// Send message in chunks (400 rune max per chunk, rune-safe)
	runes := []rune(message)
	if len(runes) <= chunkSize {
		m.tmuxSendLiteralForAgent(agent, message)
	} else {
		for offset := 0; offset < len(runes); offset += chunkSize {
			end := offset + chunkSize
			if end > len(runes) {
				end = len(runes)
			}
			m.tmuxSendLiteralForAgent(agent, string(runes[offset:end]))
			time.Sleep(chunkDelay)
		}
	}

	// Text and Enter must always be separate calls with a delay between
	time.Sleep(textToEnterDelay)
	m.tmuxSendEntersForAgent(agent)

	now := time.Now()
	agent.LastKick = &now
	agent.LastKickMessage = message
	agent.KickRefused = false
	agent.KickRefusalReason = ""
	// The session now holds this kick's output; the next rotation point
	// (kick, restart, shutdown) must archive it before it is destroyed.
	agent.kickLogPending = true

	// Arm the post-kick watchdog for inference agents: the watcher loop
	// sends a "continue" nudge if the pane freezes at an idle prompt, and
	// an action nudge if the model responds with prose but runs no tools.
	if IsInferenceBackend(effectiveBackend(agent)) {
		m.recordInferenceKick(agent, now)
	}
	// #4697: a new kick is a new incident, so the transient-API-error budget
	// starts over. Reset for EVERY backend, not just inference ones — this is
	// the CLI-backend watchdog's only per-kick anchor, and leaving a spent
	// budget in place would make one bad kick window silence the nudge for the
	// life of the process. Clearing the cooldown too lets a fresh kick that
	// fails immediately be recovered without waiting it out.
	agent.transientNudgesThisKick = 0
	agent.lastTransientNudge = time.Time{}

	snippet := message
	const maxSnippetLen = 120
	snippet = truncateStr(snippet, maxSnippetLen)
	record := KickRecord{Timestamp: now, Agent: agent.Name, Snippet: snippet}
	if len(agent.KickHistory) >= kickHistoryCapacity {
		agent.KickHistory = agent.KickHistory[1:]
	}
	agent.KickHistory = append(agent.KickHistory, record)

	kickPreview := message
	const maxKickPreviewLen = 200
	if len(kickPreview) > maxKickPreviewLen {
		kickPreview = truncateStr(kickPreview, maxKickPreviewLen)
	}
	m.logger.Info("audit: agent kicked",
		"name", agent.Name,
		"message_len", len(message),
		"preview", kickPreview,
		"trigger", trigger,
	)

	// Persist the FULL prompt text. The log line above and KickRecord.Snippet
	// only carry a truncated preview, which is not enough to answer "what was
	// my agent asked to do?".
	m.recordPrompt(agent.Name, trigger, message)

	m.notifyKickObserver(agent.Name, KickObserverEventDelivered, trigger)
}

// deliverStartupKick delivers a bootstrap prompt to a freshly launched agent
// once its CLI is actually ready for input, mirroring SendKick's readiness
// chain (CLI marker visible, input prompt shown, not parked on a consent
// screen — the consent check lives inside waitForInputPromptForAgent). It
// runs fire-and-forget in a goroutine, bounded by cliReadyTimeout +
// inputPromptTimeout. If the CLI never becomes ready the prompt is dropped
// with a warning rather than typed into a bare bash pane — the crash
// detector restarts the agent and the next launch builds a fresh bootstrap.
// gen is the agent's launch generation at spawn time; a mismatch at delivery
// time means the agent was relaunched while we waited (the new launch owns
// its own startup kick, so this one is stale and dropped).
func (m *Manager) deliverStartupKick(agent *AgentProcess, prompt string, gen int) {
	if !m.waitForCLIReadyForAgent(agent) {
		m.logger.Warn("startup kick dropped: CLI never became ready",
			"name", agent.Name, "session", agent.tmuxSession, "trigger", "startup")
		return
	}
	if !m.waitForInputPromptForAgent(agent) {
		m.logger.Warn("startup kick dropped: CLI never reached input prompt",
			"name", agent.Name, "session", agent.tmuxSession, "trigger", "startup")
		return
	}

	// bob needs an extra settle window that the other TUIs do not. Its UI is a
	// React/Ink app (its crashes surface as React reconciler stack traces), and
	// Ink paints the input box on an early render pass — so the placeholder
	// that waitForInputPromptForAgent matches can be visible before the
	// reconciler has finished mounting the input component and attached its
	// stdin handler. Text typed in that window is painted into the pane but
	// never reaches component state, so the kick is silently swallowed and bob
	// stays idle — the exact failure this change exists to fix. claude/copilot/
	// gemini do not need this: their input handlers are live as soon as the
	// prompt renders, which is why the delay is bob-only rather than a new
	// universal pause in the shared path.
	//
	// Read outside m.mu: effectiveBackend is a pure field read and this path
	// must not hold the lock while sleeping.
	if effectiveBackend(agent) == bobBackend {
		time.Sleep(bobInputHandlerSettleDelay)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.agents[agent.Name]
	if !ok || current != agent || agent.State != StateRunning || agent.launchGen != gen {
		m.logger.Warn("startup kick dropped: agent restarted or stopped while waiting",
			"name", agent.Name, "trigger", "startup")
		return
	}
	m.deliverKickLocked(agent, prompt, "startup")
}

// inferenceKickActionSuffix is appended to every kick sent to an agent whose
// effective backend is a self-hosted inference backend (vllm/llm-d/litellm).
// Weak OSS models tend to answer a kick conversationally — describing steps
// for someone else to follow — instead of acting; this block demands
// immediate tool execution. Commercial CLI backends don't receive it.
const inferenceKickActionSuffix = "IMPORTANT — EXECUTE, DO NOT NARRATE: " +
	"You have real tools (Bash, file edit, gh). Perform the work NOW in " +
	"this session. Do not describe steps for someone else, do not summarize " +
	"a plan and stop. Begin immediately by running your first command. " +
	"Every response that contains no tool execution is a failure."

// explainKickSuffixBrief and explainKickSuffixFull are appended to a kick when
// the agent's resolved explain mode is brief/full (#3887).
//
// The terseness instruction they sit next to has a real rationale — agents told
// to explain themselves narrate INSTEAD of acting, which is why every policy
// carries "Output Rules — Terse Mode" and why inferenceKickActionSuffix exists
// at all. Relaxing that outright would trade one debugging problem for a worse
// one, so these blocks are written to preserve it:
//
//   - Acting first is restated as the hard requirement, and an explanation with
//     no tool call is named as a failure, so the block cannot be read as
//     permission to reply with prose. This matters most on the inference
//     backends, where both suffixes are present.
//   - Explanation is confined to lines carrying config.ExplainLinePrefix, so it
//     is separable from the agent's real output at read time instead of being
//     interleaved into it (see handleAgentFullLog's explain filter).
//   - Terse mode is suspended ONLY on those prefixed lines. Compressing the
//     explanation to caveman-speak would defeat the point of asking for it,
//     while leaving terse mode in force everywhere else keeps the agent's
//     actual output — logs, bead titles, PR bodies — exactly as it was.
//
// Appended per-kick rather than baked into the policy files because the option
// is per-agent and toggleable at runtime; editing prompts would make it a
// fleet-wide, redeploy-gated behavior change, which the issue explicitly did
// not want.
const explainKickSuffixBrief = "EXPLAIN MODE (brief) — DEBUGGING AID, NOT A LICENCE TO NARRATE: " +
	"Do the work exactly as you otherwise would; tool execution is still the " +
	"requirement and a response that only explains is a failure. In addition, " +
	"before each tool call emit ONE line starting with " + config.ExplainLinePrefix +
	" giving your reason for that specific call (what you expect it to show or " +
	"change). Keep every other line unchanged. Terse-mode output rules are " +
	"suspended on " + config.ExplainLinePrefix + " lines only — write those in " +
	"plain, complete sentences so a human debugging you can read them."

const explainKickSuffixFull = explainKickSuffixBrief +
	" Additionally, when the work for this kick is finished, emit a closing " +
	"block of " + config.ExplainLinePrefix + " lines covering: the goal as you " +
	"understood it, the approach you chose, the alternatives you considered and " +
	"why you rejected them, and what evidence would have changed your decision. " +
	"This block comes AFTER the work, never instead of it."

// kickMessageWithSuffixes composes the message actually typed into an agent's
// CLI from the caller's kick text plus the backend/mode-dependent suffixes.
//
// Split out of deliverKickLocked so the composition — in particular the ORDER
// of the two suffixes — is testable without a tmux session. On an inference
// backend both apply, and the action-forcing block must be read first: the
// explain block then reads as a qualification of "execute, do not narrate"
// rather than as a later instruction overriding it.
func kickMessageWithSuffixes(message string, isInference bool, explainMode string) string {
	if isInference {
		message += "\n\n" + inferenceKickActionSuffix
	}
	if suffix := explainKickSuffix(explainMode); suffix != "" {
		message += "\n\n" + suffix
	}
	return message
}

// explainKickSuffix returns the kick suffix for a resolved explain mode, or ""
// when explanation is off. Modes are resolved by resolveExplainMode, so an
// unknown value here is treated as off rather than defaulted to a mode.
func explainKickSuffix(mode string) string {
	switch mode {
	case config.ExplainModeBrief:
		return explainKickSuffixBrief
	case config.ExplainModeFull:
		return explainKickSuffixFull
	default:
		return ""
	}
}

// resolveExplainMode returns the explain mode in force for an agent, given the
// hive-wide default already resolved by the caller (see Manager.explainModeDefault).
//
// Precedence, and the tri-state is the point: an explicit per-agent value —
// INCLUDING "off" — always wins, so an operator who turned explanation on
// fleet-wide does not force it onto an agent that opted out. Only an unset
// agent inherits the hive default. An invalid value in either place resolves to
// off, so a typo degrades to today's behavior rather than to an unexpected mode.
//
// hiveDefault is "" only when no resolver was injected (tests, bare setups);
// the env var is read here so that path keeps behaving as it did before the
// governor.explain_mode setting existed (#4712). When a resolver IS wired,
// config.GovernorConfig.ResolveExplainModeDefault has already applied the
// config-over-env precedence and never returns "".
func resolveExplainMode(cfg config.AgentConfig, hiveDefault string) string {
	mode := cfg.ExplainMode
	if mode == "" {
		mode = strings.TrimSpace(hiveDefault)
	}
	if mode == "" {
		mode = strings.TrimSpace(os.Getenv(config.ExplainModeEnvVar))
	}
	switch mode {
	case config.ExplainModeBrief, config.ExplainModeFull:
		return mode
	default:
		return config.ExplainModeOff
	}
}

const (
	// inferenceKickStallTimeout is how long after a kick an unchanged, idle
	// pane counts as a stalled kick (message swallowed without a response).
	inferenceKickStallTimeout = 5 * time.Minute
	// inferenceStallNudgeMessage is the literal message typed into the CLI to
	// unstick a stalled kick.
	inferenceStallNudgeMessage = "continue"
	// transientAPIErrorNudgeMessage is typed into a CLI agent that dropped back
	// to its idle prompt after a retryable API failure (#4697).
	//
	// "try again", not the "continue" above, because the two situations differ:
	// a stalled kick was never consumed, so "continue" means "get on with it",
	// while here a request FAILED mid-flight and the work needs re-attempting.
	// "try again" is also the exact wording an operator used on the live pane
	// in #4697, which recovered the agent every time — no reason to ship an
	// untested synonym in place of the phrase with evidence behind it.
	transientAPIErrorNudgeMessage = "try again"
	// transientAPIErrorNudgeCooldown is the minimum gap between two nudges for
	// the same agent. The error line remains on screen after the nudge is
	// typed, so the next poll still matches; this is what stops a single
	// incident from firing every 3s until the pane scrolls.
	transientAPIErrorNudgeCooldown = 90 * time.Second
	// transientAPIErrorMaxNudgesPerKick caps how many times one kick window may
	// be nudged. A connection that fails three times in a row is not a blip the
	// hive can type its way out of; past the cap the agent surfaces LastError
	// and waits for an operator instead of nudging forever.
	transientAPIErrorMaxNudgesPerKick = 3
	// transientAPIErrorTailLines is how much of the VISIBLE pane is examined.
	// Scrollback is deliberately excluded: an error the agent already recovered
	// from stays in history forever, and matching it there would re-nudge a
	// working agent. Sized to cover the error block plus the idle prompt under
	// it without reaching back into the previous response.
	transientAPIErrorTailLines = 12
	// cliInputPromptMarker is the CLI's idle input prompt indicator.
	cliInputPromptMarker = "❯"
	// inferenceActionNudgeGrace is the minimum time after a kick before the
	// no-action check may fire, so the watcher never misreads the brief
	// post-Enter window (kick echoed, spinner not yet rendered) as a
	// completed prose-only response.
	inferenceActionNudgeGrace = 2 * time.Minute
	// inferenceActionNudgeMessage is typed into the CLI when the model
	// answered a kick with prose only — a plan addressed to a reader with
	// zero tool executions (observed live with weak OSS models on
	// inference backends, e.g. deepseek-r1-14b via litellm/vllm).
	inferenceActionNudgeMessage = "You produced a plan but executed nothing. Execute it yourself NOW using your tools, starting with step 1. Do not reply with prose only."
	// inferenceMaxOutputTokensDefault caps CLAUDE_CODE_MAX_OUTPUT_TOKENS for
	// inference-backend agents. 16384 is a safe universal floor across the
	// commercial models operators point litellm at: Azure GPT-4o allows at
	// most 16384 completion tokens and 400s ("max_tokens is too large:
	// 128000. This model supports at most 16384 completion tokens") on
	// anything higher; GPT-4.1/GPT-5 and most vLLM/Claude backends meet or
	// exceed 16384. A previous 128000 value (chosen so verbose OSS models
	// would not truncate) made every request to a capped commercial model
	// fail. 16384 output tokens is still generous for agent work, so we
	// trade "never truncate huge OSS outputs" for "works on capped
	// commercial models" — the correct default.
	inferenceMaxOutputTokensDefault = 16384
	// cliActiveCounterMarker appears inside Claude Code's live activity
	// spinner, e.g. "✶ Infusing… (18s · ↓ 94 tokens)" (verified against
	// Claude Code v2.1.204). The completed form ("✻ Worked for 26s") has no
	// counter, so this distinguishes an in-flight response from a finished
	// one on versions whose footer no longer shows cliWorkingMarker.
	cliActiveCounterMarker = "s · ↓"
)

// toolSummaryRe matches Claude Code's collapsed tool-activity summary lines,
// rendered only when tools actually executed (verified against Claude Code
// v2.1.204): "Running 1 shell command…" while a Bash call is in flight, and
// "Ran 1 shell command" / "Read 1 file, ran 2 shell commands" once done.
// Edit/write variants are included for completeness across versions.
var toolSummaryRe = regexp.MustCompile(`(?i)\b(?:ran|running) \d+ shell command|\bread \d+ file|\bedited \d+ file|\bwrote \d+ file|\bupdated \d+ file`)

// expandedToolCallMarkers are literal fragments of Claude Code's expanded
// per-tool rendering. "⎿" is the result elbow drawn under a tool call
// (verified live on v2.1.204: "⎿  $ sleep 15 && echo probe3"); the
// "⏺ Name(" forms are the expanded tool-call headers older CLI versions
// render. A bare "⏺" is NOT a tool marker — v2.1.204 uses it as the bullet
// for every assistant response block, including pure prose.
var expandedToolCallMarkers = []string{
	"⎿",
	"⏺ Bash(",
	"⏺ Read(",
	"⏺ Write(",
	"⏺ Edit(",
	"⏺ Update(",
	"⏺ Search(",
	"⏺ Fetch(",
	"⏺ Task(",
}

// countToolMarkers counts tool-execution markers in captured pane content.
// The no-action watchdog compares the count after a kick against the count
// recorded at kick delivery: scrollback keeps markers from work done before
// the kick, so only an increase proves the model executed tools since.
//
// EXPLAIN lines are stripped first (#3887). toolSummaryRe matches ENGLISH
// PHRASES — "read 3 files", "running 2 shell commands" — because that is how
// the CLI renders its own collapsed tool summaries. An agent in explain mode is
// asked to state, in plain English, what it is about to do, so it writes
// exactly those phrases as narration:
//
//	EXPLAIN: reading 3 files under pkg/agent to find the kick handler.
//
// That counts as a tool marker, the watchdog concludes "real tool activity
// since the kick", and the prose-only action nudge never fires — on precisely
// the agents an operator turned explanation ON to debug, which is the worst
// possible time to silently lose the check. Narration about a tool is not tool
// execution, so an explanation line contributes nothing to the count.
//
// The whole line is dropped rather than just the prefix: a "⎿" or "⏺ Bash("
// quoted INSIDE an explanation is the agent describing a tool call, not making
// one.
func countToolMarkers(pane string) int {
	pane = stripExplainLines(pane)
	n := len(toolSummaryRe.FindAllStringIndex(pane, -1))
	for _, marker := range expandedToolCallMarkers {
		n += strings.Count(pane, marker)
	}
	return n
}

// stripExplainLines removes the agent's explanation lines from captured pane
// content, so pane analysis sees only what the agent actually did.
//
// Matching is on the TRIMMED line, mirroring the read-time filter in
// pkg/dashboard (filterExplainLines): the CLI indents assistant output, so
// anchoring at column 0 would miss every real line. Returns the input unchanged
// when nothing matches, which is every pane on a hive with explain mode off.
func stripExplainLines(pane string) string {
	if !strings.Contains(pane, config.ExplainLinePrefix) {
		return pane
	}
	lines := strings.Split(pane, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), config.ExplainLinePrefix) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// paneShowsActiveWork reports whether the CLI is mid-response: either the
// legacy "esc to interrupt" footer hint or the live spinner counter is
// visible. The idle input prompt "❯" alone proves nothing on v2.1.204 —
// the input box stays rendered while a response streams.
func paneShowsActiveWork(pane string) bool {
	return strings.Contains(pane, cliWorkingMarker) || strings.Contains(pane, cliActiveCounterMarker)
}

func paneShowsEmptyInputPrompt(pane string) bool {
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		return line == cliInputPromptMarker
	}
	return false
}

// paneContentHash returns a short stable hash of pane content, used to detect
// whether a pane has changed since a kick was delivered.
func paneContentHash(pane string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(pane))
	return fmt.Sprintf("%016x", h.Sum64())
}

func paneAfterKickBaseline(pane, baseline string) string {
	if baseline == "" {
		return pane
	}
	if idx := strings.LastIndex(pane, baseline); idx >= 0 {
		return pane[idx+len(baseline):]
	}
	paneLines := strings.Split(pane, "\n")
	baseLines := strings.Split(baseline, "\n")
	common := 0
	for common < len(paneLines) && common < len(baseLines) && paneLines[common] == baseLines[common] {
		common++
	}
	if common > 0 {
		return strings.Join(paneLines[common:], "\n")
	}
	return pane
}

// recordInferenceKick arms the post-kick stall watchdog for an inference
// agent: remembers when the kick was delivered and what the pane looked like
// right after delivery. Caller must hold m.mu.
func (m *Manager) recordInferenceKick(agent *AgentProcess, at time.Time) {
	visible := m.captureVisiblePaneForAgent(agent)
	agent.lastInferKickAt = at
	agent.lastInferKickPane = paneContentHash(visible)
	agent.lastInferKickVisible = visible
	agent.stallNudgeSent = false
	// Baseline for the no-action check: markers already in scrollback from
	// work done before this kick must not count as post-kick tool activity.
	agent.lastInferKickMarks = countToolMarkers(m.captureTmuxPaneForAgent(agent))
	agent.actionNudgeSent = false
}

// nudgeIfKickStalled watches an inference agent after a kick and corrects
// two distinct failure modes, sending at most one nudge each (so at most two
// combined nudges per kick):
//
//   - Frozen pane: the pane has not changed since the kick was delivered and
//     the stall timeout elapsed — the CLI swallowed the message. Sends the
//     "continue" nudge (counted in StallNudges).
//   - Prose-only response: the pane changed (the CLI consumed the kick), the
//     response completed back at the idle input prompt, but the tool-marker
//     count has not risen above the baseline recorded at kick delivery — the
//     model narrated a plan instead of acting. Sends the action nudge
//     (counted in ActionNudges).
//
// A CLI that is mid-response (paneShowsActiveWork) is always left alone, and
// post-kick tool activity disarms the watchdog entirely.
func (m *Manager) nudgeIfKickStalled(name, pane string) {
	now := time.Now()
	m.mu.Lock()
	agent, ok := m.agents[name]
	if !ok || agent.lastInferKickAt.IsZero() || agent.lastInferKickPane == "" {
		m.mu.Unlock()
		return
	}
	if paneShowsActiveWork(pane) || !strings.Contains(pane, cliInputPromptMarker) {
		m.mu.Unlock()
		return
	}
	sinceKick := now.Sub(agent.lastInferKickAt)
	if match, ok := classifyProviderError(paneAfterKickBaseline(pane, agent.lastInferKickVisible)); ok {
		backoff := m.markProviderErrorLocked(agent, match, now)
		attempt := agent.providerErrorBackoffAttempt
		m.mu.Unlock()

		m.logger.Warn("inference provider error detected, backing off kicks instead of sending action nudge",
			"name", name,
			"class", match.Class,
			"attempt", attempt,
			"backoff", backoff.Round(time.Second),
			"error", match.Line)
		return
	}
	m.clearProviderErrorLocked(agent)

	if paneContentHash(pane) == agent.lastInferKickPane {
		// Frozen pane: the CLI never consumed the kick.
		if agent.stallNudgeSent || sinceKick < inferenceKickStallTimeout {
			m.mu.Unlock()
			return
		}
		agent.stallNudgeSent = true
		agent.StallNudges++
		totalNudges := agent.StallNudges
		m.mu.Unlock()

		m.logger.Warn("inference agent stalled after kick, sending continue nudge",
			"name", name,
			"minutes_since_kick", int(sinceKick.Minutes()),
			"total_nudges", totalNudges)
		m.tmuxSendLiteralForAgent(agent, inferenceStallNudgeMessage)
		time.Sleep(textToEnterDelay)
		m.tmuxSendEntersForAgent(agent)
		return
	}

	// The pane moved since the kick — the CLI consumed it and the response
	// completed (idle prompt, no active-work indicator). Check whether any
	// tools ran since the kick before declaring the response prose-only.
	if agent.actionNudgeSent || sinceKick < inferenceActionNudgeGrace {
		m.mu.Unlock()
		return
	}
	if countToolMarkers(m.captureTmuxPaneForAgent(agent)) > agent.lastInferKickMarks {
		// Real tool activity since the kick — the agent is acting. Disarm.
		agent.lastInferKickPane = ""
		m.mu.Unlock()
		return
	}
	agent.actionNudgeSent = true
	agent.ActionNudges++
	totalActionNudges := agent.ActionNudges
	m.mu.Unlock()

	m.logger.Warn("inference agent answered kick with prose only, sending action nudge",
		"name", name,
		"minutes_since_kick", int(sinceKick.Minutes()),
		"total_action_nudges", totalActionNudges)
	m.tmuxSendLiteralForAgent(agent, inferenceActionNudgeMessage)
	time.Sleep(textToEnterDelay)
	m.tmuxSendEntersForAgent(agent)
}

// nudgeIfTransientAPIError recovers a CLI agent that a retryable API failure
// left parked at its idle prompt (#4697).
//
// THE GAP THIS FILLS. Every neighbouring recovery path deliberately excludes
// this case: the crash watcher wants a dead process and the CLI is alive; the
// fatal-network restart (paneShowsFatalNetworkError) is copilot-only and would
// be the wrong medicine anyway, since a restart throws away the very context
// that makes recovery cheap; the login detector wants a login prompt; #4400 and
// #4583 handle errors retrying CANNOT fix. And nudgeIfKickStalled — the one
// piece that already types "continue" at a frozen pane — is armed only from
// recordInferenceKick, which sits behind an IsInferenceBackend guard, so CLI
// backends never arm it. The result was an agent that is healthy,
// authenticated, under quota, mid-task and one Enter from resuming, sitting
// idle until the next scheduled kick hours later.
//
// pane must be the VISIBLE pane, not scrollback — see transientAPIErrorTailLines.
//
// Every precondition is a refusal to nudge something that is not stuck:
//
//   - inference backends are skipped: nudgeIfKickStalled already covers them,
//     and two watchdogs typing at one pane would race;
//   - paneShowsActiveWork or a missing idle prompt means the CLI is streaming
//     or mid-retry (Claude Code retries some failures itself, rendering a
//     countdown) — interrupting that would CAUSE the stall it is meant to fix;
//   - an authorization or quota failure on screen is re-checked here even
//     though the pattern list excludes both, because Claude Code prefixes every
//     API failure with the same "API Error:" chrome and a nudge into a wall
//     burns tokens to no effect.
func (m *Manager) nudgeIfTransientAPIError(agent *AgentProcess, pane string) {
	// Inference backends have their own watchdog; see the doc comment.
	if IsInferenceBackend(effectiveBackend(agent)) {
		return
	}
	tail := paneTail(pane, transientAPIErrorTailLines)
	if tail == "" {
		return
	}
	// Mid-response or mid-retry: leave it alone.
	if paneShowsActiveWork(tail) || !paneShowsEmptyInputPrompt(tail) {
		return
	}
	lines := strings.Split(tail, "\n")
	if !paneShowsTransientAPIError(lines) {
		return
	}
	// Guardrails: never nudge an error a retry cannot clear.
	if paneShowsQuotaExhausted(lines) {
		return
	}
	for _, line := range lines {
		if lineShowsUpstreamAuthorizationError(line) {
			return
		}
	}
	if m.tmuxSessionHasAttachedClientForAgent(agent) {
		return
	}

	now := time.Now()
	m.mu.Lock()
	if now.Sub(agent.lastTransientNudge) < transientAPIErrorNudgeCooldown {
		m.mu.Unlock()
		return
	}
	if agent.transientNudgesThisKick >= transientAPIErrorMaxNudgesPerKick {
		// Past the cap the failure is an operator problem. Surface it once —
		// re-stamping LastError every poll would hide whatever else the agent
		// reports — and stop typing.
		if agent.LastError == "" {
			agent.LastError = "repeated transient API errors; nudges exhausted"
		}
		m.mu.Unlock()
		return
	}
	agent.lastTransientNudge = now
	agent.transientNudgesThisKick++
	agent.TransientNudges++
	attempt, total := agent.transientNudgesThisKick, agent.TransientNudges
	m.mu.Unlock()

	m.logger.Warn("CLI agent idle after a transient API error, sending retry nudge",
		"name", agent.Name,
		"attempt", attempt,
		"max_per_kick", transientAPIErrorMaxNudgesPerKick,
		"total_nudges", total)
	m.tmuxSendLiteralForAgent(agent, transientAPIErrorNudgeMessage)
	time.Sleep(textToEnterDelay)
	m.tmuxSendEntersForAgent(agent)
}

const (
	enterCount = 3
	chunkSize  = 400
	// cliBootGraceSeconds is how long after StartedAt a bare pane (no CLI
	// marker) is tolerated before CheckAndRestartCrashedAgents treats it as a
	// crash. It matches the production cliReadyTimeout (60s) so a still-booting
	// CLI is never restarted underneath itself, which would spawn a second
	// concurrent CLI.
	cliBootGraceSeconds = 60
)

// Pacing seams (#4717/#4693/#4688). These are package VARS, not consts, so the
// pkg/agent TestMain can shrink them for the whole suite: they accumulate to
// well over the default 10m `go test` timeout when 440 tests pay production
// pacing (1-3s sleeps, 60-120s poll deadlines) against stub CLIs that render
// instantly. Production values are unchanged — nothing outside TestMain may
// mutate them. Relationships the values encode (bobInputHandlerSettleDelay
// distinct from every tmux-pacing delay, and far below inputPromptTimeout) are
// pinned by TestBobSettleDelay_* and must be preserved by any test override.
var (
	clearBeforeKickDelay    = 2 * time.Second
	enterDelay              = 300 * time.Millisecond
	textToEnterDelay        = 1 * time.Second
	chunkDelay              = 1 * time.Second
	staleCheckDelay         = 1 * time.Second
	cliReadyPollInterval    = 2 * time.Second
	cliReadyTimeout         = 60 * time.Second
	inputPromptPollInterval = 2 * time.Second
	inputPromptTimeout      = 120 * time.Second
	// preLaunchShellClearDelay gives bash time to process the C-c that
	// clears stale PS2 quote-continuation state before the launch command
	// is typed into the pane.
	preLaunchShellClearDelay = 500 * time.Millisecond
	// bobInputHandlerSettleDelay is an extra pause applied ONLY to the bob
	// backend after its input prompt becomes visible, before its startup kick
	// is typed. bob's TUI is React/Ink: Ink paints the input box on an early
	// render pass, so the placeholder can be on screen before the reconciler
	// finishes mounting the input component and attaching its stdin handler.
	// Typing in that gap paints characters that never reach component state —
	// the kick is swallowed and bob sits idle. It is deliberately NOT reused
	// from textToEnterDelay/staleCheckDelay (1s each): those cover tmux
	// keystroke pacing, a different concern, and a value that merely happens
	// to work is what this constant exists to avoid. 3s is a conservative
	// multiple of the observed 1s pacing unit, chosen because over-waiting
	// costs one agent a few seconds once per launch while under-waiting
	// silently loses the bootstrap prompt entirely.
	bobInputHandlerSettleDelay = 3 * time.Second
	// sessionReadyDelay is how long a freshly created tmux session's shell is
	// given to initialize before the launch command is typed into it. Without
	// it, $(cat /tmp/.hive-bootstrap-*.txt) can fail because the shell isn't
	// ready to process command substitution yet.
	sessionReadyDelay = 2 * time.Second
)

func (m *Manager) SeedLastKick(name string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agent, ok := m.agents[name]; ok {
		agent.LastKick = &t
	}
}

func (m *Manager) SeedKickHistory(name string, records []KickRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agent, ok := m.agents[name]; ok {
		if len(records) > kickHistoryCapacity {
			records = records[len(records)-kickHistoryCapacity:]
		}
		agent.KickHistory = make([]KickRecord, len(records))
		copy(agent.KickHistory, records)
	}
}
