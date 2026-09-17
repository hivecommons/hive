// Output polling and in-pane watch loops: the per-agent tmux output
// poller, trust-prompt watcher, output signal logging, blocked-action
// thrash detection, kick-refusal detection, and pane-diff helpers.
//
// Extracted from manager.go as part of the #7303 god-file split.
package agent

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// pollTmuxOutputForAgent is pollTmuxOutput using the agent's tmux socket.
func (m *Manager) pollTmuxOutputForAgent(agent *AgentProcess, ctx context.Context) {
	const pollInterval = 3 * time.Second
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var prevLines []string
	loginStreak := 0
	outcomeTick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Scrollback capture for dashboard display + ring buffer diff
			output := m.captureTmuxPaneForAgent(agent)
			if output == "" {
				continue
			}
			var filtered []string
			for _, line := range strings.Split(output, "\n") {
				trimmed := strings.TrimRight(line, " \t")
				if trimmed != "" {
					filtered = append(filtered, trimmed)
				}
			}
			if len(filtered) == 0 {
				continue
			}

			// Match login prompts against the pane TAIL only: a prompt the CLI
			// is genuinely stuck at sits at the bottom of the pane, while the
			// two false-positive classes that plagued this check — login-ish
			// phrases inside ECHOED KICK TEXT (agent policies discuss auth) and
			// the CLI's transient startup "/login" flash during its auth
			// handshake — live in scrollback or vanish within a poll or two.
			tailStart := len(filtered) - loginPromptTailLines
			if tailStart < 0 {
				tailStart = 0
			}
			tail := filtered[tailStart:]
			showsLogin := paneShowsLoginPrompt(tail)
			bobKeyRejected := effectiveBackend(agent) == bobBackend && paneShowsBobAPIKeyRejected(tail)
			quotaExhausted := paneShowsQuotaExhausted(tail)
			if showsLogin || bobKeyRejected {
				showsLogin = true
				loginStreak++
			} else {
				// The streak resets unconditionally: its job is only to
				// require the login line to PERSIST across consecutive polls,
				// so any poll without one legitimately restarts the count.
				loginStreak = 0
				// The CAP does not. "No login prompt" is not the same claim as
				// "the login cleared" — a booting pane shows neither, and a
				// boot is exactly what a token restart just caused. Resetting
				// here unconditionally meant the give-up latch was wiped by the
				// boot pane of its own remedy and the cap could never hold
				// (#6578). shouldResetTokenRestartCap demands positive evidence
				// instead; see it for the two conditions and why both.
				m.mu.RLock()
				startedAt := agent.StartedAt
				m.mu.RUnlock()
				if agent.shouldResetTokenRestartCap(paneHasCLIMarker(strings.Join(tail, "\n")), startedAt, time.Now()) {
					// A future "token appeared, nudge it" restart is now a
					// fresh theory rather than a repeat of one that already
					// failed. Reset both halves of the cap together.
					agent.tokenRestartAttempts = 0
					agent.tokenRestartGaveUp = false
				}
			}

			agent.paneMu.Lock()
			// Advance the activity clock only when the pane actually changed.
			// Comparing against the PREVIOUS capture (not prevLines, which the
			// ring-buffer diff below consumes and rewrites) is what separates
			// "the CLI is producing output" from "the CLI is sitting at an idle
			// prompt": a static pane re-captured every 3s is byte-identical, so
			// an agent that renders nothing new never moves this timestamp.
			if !samePaneCapture(agent.lastPaneCapture, filtered) {
				agent.LastPaneChange = time.Now()
			}
			agent.lastPaneCapture = filtered
			agent.NeedsLogin = showsLogin
			agent.QuotaExhausted = quotaExhausted
			agent.paneMu.Unlock()

			// Auto-restart agents stuck on the login prompt when a valid
			// token exists in the shared config.json. This handles the case
			// where a user authenticates via one agent's terminal and other
			// agents don't pick up the new token automatically.
			//
			// THREE guards, each traced to a live failure (hivecommons/hive,
			// 2026-08-22, scanner restart_count=28 with every kick destroyed):
			//   1. loginStreak: the login line must persist across consecutive
			//      polls (~9s). The CLI flashes "Please use /login" during its
			//      startup auth handshake even when the seeded token is about
			//      to be accepted; one-poll sightings restarted HEALTHY agents.
			//   2. Kick grace: never restart within tokenRestartKickGrace of a
			//      delivered kick — the restart at 02:28:00 landed the same
			//      second as a 14KB scan prompt and destroyed it. A stuck-at-
			//      login agent that was kicked long ago restarts after the
			//      grace expires; delivered work is never killed mid-scan.
			//   3. The existing cooldown.
			//   4. THE CREDENTIAL MUST BE THIS AGENT'S. configHasTokens() is
			//      backend-blind: it answers true when EITHER the shared claude
			//      or copilot credential is usable, whatever backend the agent
			//      in front of it runs. So an agy agent parked at its Google
			//      OAuth prompt was restarted on the strength of an unrelated
			//      claude login. A restart cannot mint a Google session, so it
			//      looped — and each relaunch minted a fresh PKCE challenge,
			//      invalidating the code the operator was in the middle of
			//      pasting. Observed live 2026-09-01: 15 restarts in an hour
			//      and 8 distinct challenges, which made signing in impossible
			//      rather than merely slow. AgentHasValidCredential resolves
			//      the agent's OWN backend and is positive-evidence-only, so a
			//      backend whose credential this process cannot verify (agy,
			//      gemini) answers false and is left alone — which is correct:
			//      do not restart when you cannot show a restart would help.
			if showsLogin && loginStreak >= loginStreakRestartMin && m.AgentHasValidCredential(agent.Name) {
				m.mu.RLock()
				lastKick := agent.LastKick
				m.mu.RUnlock()
				if lastKick != nil && time.Since(*lastKick) < tokenRestartKickGrace {
					continue
				}
				// GUARD 4 (#4596): the restart theory must not be retried
				// forever. decideTokenRestart owns the attempt accounting so
				// the rule is unit-testable without a tmux pane.
				switch agent.decideTokenRestart(time.Now()) {
				case tokenRestartGiveUp:
					if !agent.tokenRestartGaveUp {
						agent.tokenRestartGaveUp = true
						diag := m.diagnoseStuckLogin(agent)
						agent.LastError = diag
						m.logger.Warn("giving up on token-triggered restart: the agent is still at a login prompt",
							"agent", agent.Name,
							"attempts", agent.tokenRestartAttempts,
							"diagnosis", diag,
						)
					}
					// Deliberately NOT `continue`: this disables only the
					// token-triggered restart. The TLS-error and hung-CLI
					// detectors further down stay live, so an agent that is
					// both stuck at a login prompt and hitting a transient
					// network failure is still recovered by the detector that
					// can actually help.
				case tokenRestartWait:
					// Cooldown has not elapsed; fall through to the other
					// pane detectors below, exactly as before.
				case tokenRestartFire:
					m.logger.Info("auto-restarting agent after token detected in shared config",
						"agent", agent.Name,
						"attempt", agent.tokenRestartAttempts,
						"max_attempts", tokenRestartMaxAttempts,
					)
					go func() {
						if err := m.RestartWithReason(ctx, agent.Name, "login token refreshed"); err != nil {
							m.logger.Warn("token-triggered restart failed",
								"agent", agent.Name,
								"error", err,
							)
						}
					}()
					return // stop polling; Restart will spawn a new goroutine
				}
			}

			if bobKeyRejected {
				agent.LastError = startFailureReason(bobBackend, StartFailureCredentialRejected, "")
				m.mu.Lock()
				delay, blocked := m.recordStartFailureLocked(agent, bobBackend, StartFailureCredentialRejected, "")
				m.mu.Unlock()
				m.logger.Warn("bob API key rejected; automatic restart suppressed by start-failure backoff",
					"agent", agent.Name,
					"blocked", blocked,
					"retry_in", delay.Round(time.Second).String(),
				)
				return
			}

			// Detect fatal TLS/network errors that leave the agent visually
			// "ready" (the Copilot chrome shows ❯ and / commands) but actually
			// dead. These errors are transient — a restart will succeed once
			// the network recovers.
			// effectiveBackend, not Config.Backend: an agent configured for
			// copilot but overridden to another CLI at runtime still reported
			// "copilot" here, so this copilot-only detector ran against a
			// different TUI's output. Observed in production on a bob agent
			// whose config said copilot — bob printed one of the generic
			// patterns below ("fetch failed"), the agent was restarted as if
			// its TLS had died, and the governor kick delivered seconds
			// earlier died with the session. It looped every ~60s, so no kick
			// ever survived long enough to run.
			if effectiveBackend(agent) == "copilot" && !agent.kickDelivering.Load() && paneShowsFatalNetworkError(filtered) {
				sinceLastRestart := time.Since(agent.lastTokenRestart).Seconds()
				if sinceLastRestart >= float64(tlsErrorRestartCooldownSec) {
					m.logger.Warn("fatal network/TLS error detected, restarting agent",
						"agent", agent.Name,
					)
					agent.lastTokenRestart = time.Now()
					agent.LastError = "transient TLS/network error"
					go func() {
						if err := m.RestartWithReason(ctx, agent.Name, "transient network error"); err != nil {
							m.logger.Warn("tls-error-triggered restart failed",
								"agent", agent.Name,
								"error", err,
							)
						}
					}()
					return
				}
			}

			// #4697: a RETRYABLE API failure leaves the CLI alive at its idle
			// prompt with the response truncated, where it waits for the next
			// scheduled kick. Unlike the restart above, the fix is to type at
			// the session that is already holding the context.
			//
			// `filtered` is scrollback, so it is used only as a cheap gate —
			// no match means no tmux exec. The decision itself is made on the
			// VISIBLE pane, because a matched line in scrollback is usually an
			// error the agent already recovered from.
			// kickDelivering: the nudge TYPES into the pane ("try again" +
			// Enter). Firing it mid-delivery would splice those keystrokes
			// into the middle of a kick and submit the truncated result.
			if !agent.kickDelivering.Load() && paneShowsTransientAPIError(filtered) {
				m.nudgeIfTransientAPIError(agent, m.captureVisiblePaneForAgent(agent))
			}

			// A live CLI marker is the success signal for the start-failure
			// record: whatever failed before, this agent is up now (#5958).
			// Checked before the hang branch below so a recovered agent stops
			// carrying a stale blocked reason on the dashboard.
			if paneShowsCLIReady(filtered) {
				m.mu.Lock()
				m.clearStartFailureLocked(agent)
				m.mu.Unlock()
			}

			// Detect copilot hung: if running long enough with no CLI prompt,
			// launch bare `copilot` to diagnose the error. Only clear the
			// token if the diagnostic shows an auth error.
			// Skip for inference backends — they use Claude -p mode (non-interactive).
			//
			// effectiveBackend, not Config.Backend (#5958): an agent with a
			// backend override was already being judged by the wrong CLI's
			// readiness signature here, which is the same class of bug as #5921's
			// root cause 1 — launched as one thing, health-checked as another.
			//
			// kickDelivering gates this for the same reason it gates the TLS
			// restart above, but this detector is the one that was actually
			// observed destroying kicks: it does not look at pane CONTENT for
			// an error, only for the ABSENCE of idle chrome, and a kick being
			// typed is precisely a pane with no idle chrome. Delivery pushes
			// the prompt out of the capture window for ~100s on a large kick,
			// far longer than expiredTokenHangTimeoutSec has any reason to
			// tolerate.
			if effectiveBackend(agent) == "copilot" && !IsInferenceBackend(agent.BackendOverride) && agent.StartedAt != nil &&
				!agent.kickDelivering.Load() &&
				time.Since(*agent.StartedAt).Seconds() >= expiredTokenHangTimeoutSec &&
				!paneShowsCLIReady(filtered) {
				// #5921 root cause 1: launch_cmd runs a DIFFERENT CLI than the
				// configured backend, so this readiness probe is waiting for a
				// prompt the launched binary never prints. A copilot diagnostic
				// cannot resolve that — it would relaunch bare copilot, succeed,
				// restart the agent back into bob, and hang again, forever (the
				// observed 4,025-line loop). Name the contradiction instead and
				// let the backoff hold it.
				// #5921's other user-side cause: the pane says
				// "Please use /login to sign in to use Copilot" and the hive
				// reported "hung". The CLI is telling us exactly what is wrong;
				// record THAT rather than running a diagnostic to rediscover it.
				if showsLogin {
					m.mu.Lock()
					delay, blocked := m.recordStartFailureLocked(agent, effectiveBackend(agent), StartFailureLoginRequired, "")
					m.mu.Unlock()
					m.logger.Warn("agent CLI is sitting at a login prompt and never became ready",
						"agent", agent.Name,
						"backend", effectiveBackend(agent),
						"blocked", blocked,
						"retry_in", delay.Round(time.Second).String(),
					)
					return
				}
				if declared := backendMismatch(agent.Config.Backend, agent.Config.LaunchCmd); declared != "" {
					m.mu.Lock()
					delay, blocked := m.recordStartFailureLocked(agent, agent.Config.Backend, StartFailureBackendMismatch,
						"configured "+agent.Config.Backend+", launches "+declared)
					m.mu.Unlock()
					m.logger.Warn("agent backend/launch_cmd mismatch; not running a diagnostic that cannot converge",
						"agent", agent.Name,
						"configured_backend", agent.Config.Backend,
						"launch_cmd_backend", declared,
						"blocked", blocked,
						"retry_in", delay.Round(time.Second).String(),
					)
					return
				}
				m.mu.Lock()
				startBackoff := m.startFailureBackoffRemainingLocked(agent, time.Now())
				startReason := agent.StartFailureReason
				m.mu.Unlock()
				if startBackoff > 0 {
					// Already diagnosed, already paced. Re-running the diagnostic
					// here is what recreated a tmux session every ~3 minutes for
					// a condition only a human can clear.
					m.logger.Debug("copilot hang diagnostic suppressed by start-failure backoff",
						"agent", agent.Name,
						"reason", startReason,
						"retry_in", startBackoff.Round(time.Second).String(),
					)
					return
				}
				sinceLastRestart := time.Since(agent.lastTokenRestart).Seconds()
				if sinceLastRestart >= float64(tokenRestartCooldownSec) {
					m.logger.Warn("copilot hung with no CLI prompt, running diagnostic",
						"agent", agent.Name,
						"uptime_sec", int(time.Since(*agent.StartedAt).Seconds()),
					)
					agent.lastTokenRestart = time.Now()
					go m.runCopilotDiagnostic(ctx, agent)
					return
				}
			}

			if prevLines == nil {
				if agent.OutputBuffer.Count() == 0 {
					for _, l := range filtered {
						if !isBufferNoise(l) {
							agent.OutputBuffer.Write(l)
						}
					}
				}
				prevLines = filtered
				continue
			}
			newLines := diffNewLines(prevLines, filtered)
			for _, l := range newLines {
				if !isBufferNoise(l) {
					agent.OutputBuffer.Write(l)
				}
				m.logOutputSignals(agent.Name, l)
				if !agent.KickRefused {
					m.checkKickRefusal(agent, l)
				}
			}
			prevLines = filtered

			// #7421: once the kicked turn is over (the CLI is back at its idle
			// prompt), classify how it ended — a clarifying question, a policy
			// stand-down, a nothing-produced report — so the governor stops
			// counting a delivered-but-fruitless kick as a completed one.
			// Inference backends are classified from the stall watchdog
			// instead (nudgeIfKickStalled), which already tracks their turn.
			// The gate is cheap; the visible-pane capture only happens once the
			// gate passes, and is throttled to one in kickOutcomePollEvery ticks.
			if !IsInferenceBackend(effectiveBackend(agent)) {
				outcomeTick++
				if outcomeTick%kickOutcomePollEvery == 0 {
					m.maybeSettleKickOutcome(agent, func() string { return m.captureVisiblePaneForAgent(agent) })
				}
			}
		}
	}
}

// codexUpdatePromptLabel identifies the codex update menu in blockingPrompts.
//
// NOTE: answering "skip until next version" is deliberate, and updating in
// place is NOT a viable alternative here. The prompt's own "1. Update now"
// runs `npm install -g @openai/codex`, which needs write access to
// /usr/local/lib/node_modules (root:root). Neither the agent UID nor the hub
// process has it — attempting the install from the hub fails with the same
// exit status 243. "Skip until next version" persists in each agent's
// CODEX_HOME, so the prompt is answered once per agent and does not recur.
// Updating the CLI belongs to the image build, not to a running agent.
const codexUpdatePromptLabel = "codex update prompt"

// watchForTrustPromptForAgent monitors a tmux session for startup-blocking
// modal prompts using the agent's tmux socket, and answers each with the
// specific option that unblocks it (see blockingPrompts).
//
// Originally this handled only Copilot's folder-trust dialog. It covers codex's
// trust and update menus too, because those block startup exactly the same way
// and the update menu's default answer is actively destructive.
// Trust-prompt watcher pacing. Vars, not consts, so the pkg/agent TestMain can
// shrink them (see the pacing block near deliverStartupKick). Production
// values unchanged; shared by both watchForTrustPrompt variants.
var (
	trustPollInterval = 2 * time.Second
	trustMaxWait      = 120 * time.Second
	trustCooldown     = 3 * time.Second
	// trustReanswerAfter is how long a prompt must have been answered before
	// the same prompt may be answered again. Short enough that a CLI which
	// restarts inside the pane (crash loop, /login relaunch) is unwedged on
	// its next appearance; long enough that the menu still rendering for a
	// beat after the keystroke is never double-typed (the original failure:
	// a second poll matched the fading menu and typed the option again — by
	// then the CLI was at its input prompt, so the digit was submitted as a
	// user message and answered, burning tokens).
	trustReanswerAfter = 60 * time.Second
)

func (m *Manager) watchForTrustPromptForAgent(agent *AgentProcess, ctx context.Context) {
	// No deadline: the watcher runs for the agent's whole lifetime (ctx is the
	// per-launch context). The old 120s window assumed the trust prompt only
	// appears at startup, but Copilot ≥1.0.78 can render it later than that on
	// a slow first start — and an unanswered prompt wedges the agent, which the
	// login-detector then misreads as "needs login" (live on hivecommons/hive,
	// 2026-08-22). A 2s poll of an in-memory pane capture is too cheap to need
	// a deadline.
	ticker := time.NewTicker(trustPollInterval)
	defer ticker.Stop()

	answeredAt := make(map[string]time.Time, len(blockingPrompts))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A tick that was already queued when ctx was cancelled can win
			// the select above (Go picks uniformly among ready cases), so
			// re-check here: the launch is over and nothing may be typed
			// into its pane. Without this a cancelled watcher could answer
			// the same prompt a second time after it had already been
			// answered (#6282, #6283).
			if ctx.Err() != nil {
				return
			}
			output := m.captureTmuxPaneForAgent(agent)
			if key, label, ok := blockingPromptKey(agent.effectiveBackend(), output); ok && time.Since(answeredAt[label]) > trustReanswerAfter {
				answeredAt[label] = time.Now()
				time.Sleep(paneCaptureSleep)
				// An empty key means the affirmative option is already selected
				// and Enter alone answers it (agy). Typing a digit there would
				// be stray input, not a selection.
				if key != "" {
					m.tmuxSendKeysForAgent(agent, key)
					time.Sleep(enterDelay)
				}
				m.tmuxSendKeysForAgent(agent, "Enter")
				m.logger.Info("auto-answered blocking prompt",
					"agent", agent.Name, "prompt", label, "option", key)

				time.Sleep(trustCooldown)
			}
		}
	}
}

// outputSignalPatterns are substrings in agent output that indicate meaningful
// events worth logging. Each pattern maps to a short event label.
var outputSignalPatterns = map[string]string{
	"[HEARTBEAT]":  "heartbeat",
	"[STATUS]":     "status",
	"[FINDING]":    "finding",
	"[COMPLETE]":   "task_complete",
	"[ERROR]":      "agent_error",
	"PASS":         "pass_marker",
	"git commit":   "git_commit",
	"git checkout": "git_branch",
	"git push":     "git_push",
	"created file": "file_created",
	"Wrote":        "file_written",
	"test:":        "test_activity",
	"FAIL":         "test_failure",
	"coverage":     "coverage_report",
}

// logOutputSignals checks a line of agent output for meaningful patterns
// and emits a structured slog entry for each match.
func (m *Manager) logOutputSignals(agent, line string) {
	for pattern, event := range outputSignalPatterns {
		if strings.Contains(line, pattern) {
			preview := line
			const maxPreviewLen = 200
			preview = truncateStr(preview, maxPreviewLen)
			m.logger.Info("agent output signal",
				"agent", agent,
				"event", event,
				"content", preview,
			)
			return
		}
	}
}

var kickRefusalPatterns = []string{
	"I'm declining to execute",
	"I'm declining this",
	"prompt injection",
	"I won't act on bulk automated",
	"credential handling concern",
	"autonomous orchestration prompt",
	"I shouldn't follow autonomously",
	"characteristic of a prompt injection attack",
}

func (m *Manager) checkKickRefusal(agent *AgentProcess, line string) {
	lower := strings.ToLower(line)
	for _, pattern := range kickRefusalPatterns {
		if strings.Contains(lower, strings.ToLower(pattern)) {
			agent.KickRefused = true
			const maxReasonRunes = 200
			reason := line
			if runes := []rune(reason); len(runes) > maxReasonRunes {
				reason = string(runes[:maxReasonRunes])
			}
			agent.KickRefusalReason = reason
			m.logger.Warn("agent refused kick",
				"agent", agent.Name,
				"pattern", pattern,
				"line", reason,
			)
			return
		}
	}
}

func diffNewLines(prev, curr []string) []string {
	if len(prev) == 0 {
		return curr
	}
	overlap := findOverlap(prev, curr)
	if overlap >= 0 {
		return curr[overlap:]
	}
	return curr
}

var spinnerReplacer = strings.NewReplacer(
	"◐", "○", "◑", "○", "◒", "○", "◓", "○",
	"◎", "○", "◉", "○", "●", "○",
)

var creditsRe = regexp.MustCompile(`AI Credits: [\d.]+`)

func normalizeLine(s string) string {
	s = strings.TrimRight(s, " \t")
	s = spinnerReplacer.Replace(s)
	s = creditsRe.ReplaceAllString(s, "AI Credits: _")
	return s
}

func findOverlap(prev, curr []string) int {
	maxTail := len(prev)
	if maxTail > len(curr) {
		maxTail = len(curr)
	}
	for tail := maxTail; tail > 0; tail-- {
		match := true
		for i := range tail {
			if normalizeLine(prev[len(prev)-tail+i]) != normalizeLine(curr[i]) {
				match = false
				break
			}
		}
		if match {
			return tail
		}
	}
	return -1
}
