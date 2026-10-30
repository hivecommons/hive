package agent

import (
	"strings"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/watchdog"
)

// cliPaneMarkers are strings that appear in a tmux pane when a CLI (claude,
// copilot, gemini, goose, aider) is running. A bare bash prompt has none of
// these. Checking pane content is more reliable than inspecting /proc/comm
// because CLIs may run as node, python, or other interpreters whose process
// name doesn't match the CLI binary.
var cliPaneMarkers = []string{
	"❯",
	"esc cancel",
	"/ commands",
	"? help",
	"Claude",
	"Copilot",
	"Gemini",
	"goose",
	// pi's marker. pi renders a TUI status bar showing model context usage
	// (e.g. "↑37k ↓20k R756k CH99.6% $0.013 5.9%/1.0M (auto)") instead of a
	// "❯"/"goose is ready" prompt, so none of the entries above match a
	// running pi: waitForCLIReadyForAgent would never see it as ready and the
	// startup kick would be dropped after cliReadyTimeout even though pi is
	// healthy. "%/" matches the fixed "%%/1.0M" context-meter suffix pi
	// renders at every context size.
	piContextMarker,
	// bob's markers. NONE of the entries above match a running bob: verified
	// against the installed bundle (bobshell 1.0.6 bundle/bob.js), which
	// contains zero "❯" characters and no "esc cancel" / "/ commands" /
	// "? help" / "goose" strings. ("Claude"/"Gemini" occur only as model-name
	// data, never as UI chrome.) Without these two entries
	// waitForCLIReadyForAgent can never see a booted bob, so its startup kick
	// would be dropped after cliReadyTimeout even though bob is healthy.
	bobInputPlaceholder,
	bobInputPlaceholderDefault,
	bobProductMarker,
	// codex's markers. Codex 0.144.1's TUI renders NONE of the entries above:
	// verified live (daviddiaz "Visual Hive", the hub-reachable cluster) — an idle codex pane
	// contained no "❯", "goose", "Claude"/"Gemini" chrome, or bob strings, only
	// the "›" (U+203A) input caret and the "OpenAI Codex" banner. Without these
	// two entries waitForCLIReadyForAgent can never see a booted codex, so its
	// kick is dropped after cliReadyTimeout even though codex is healthy.
	codexInputPromptMarker,
	codexProductMarker,
	// agy defaults to Hive's headless shim rather than the upstream TUI. The
	// shim prints a stable ready marker at the shell prompt so the normal
	// launch, reuse, crash-detection and startup-kick gates keep working
	// without treating a bare shell as a live backend.
	agyHeadlessReadyMarker,
}

const (
	// bobInputPlaceholder is the placeholder bob renders inside its input box
	// when it is idle and accepting input. This is bob's equivalent of the "❯"
	// prompt for the other TUIs and is the PRIMARY readiness signal: the
	// bundle renders it on the same component whose presence is gated by
	// `isInputActive`, so seeing it means the input is live, not merely
	// painted. Copied verbatim from bobshell 1.0.6 — see TestBobPaneMarkers.
	bobInputPlaceholder = "Type your message or @path/to/file"
	// bobInputPlaceholderDefault is the OTHER placeholder bob renders in that
	// same input box. The two are alternatives chosen by editor mode, not
	// versions: the bundle picks bobInputPlaceholder only when vim-style
	// modal editing is on, and this string in every other case — which is the
	// default, so it is what a stock bob actually shows when idle and ready.
	//
	// Verified live on bobshell 1.0.6: a healthy authenticated bob pane
	// contained this string and ZERO occurrences of bobInputPlaceholder, so
	// waitForCLIReadyForAgent never saw it as ready and every governor kick
	// was dropped with "CLI did not become ready after restart" while bob sat
	// perfectly healthy at its prompt. Both strings are present in the 1.0.6
	// bundle, so match either rather than replacing one with the other.
	bobInputPlaceholderDefault = "Enter your prompt, / for commands"
	// bobProductMarker is bob's product name, which appears in its banner and
	// dialogs. It is a weaker, secondary signal than bobInputPlaceholder — it
	// also shows on trust/auth/license dialogs, which are NOT ready states —
	// so it is used only for coarse CLI-presence detection (is anything other
	// than bash in this pane?), never as the input-ready gate.
	bobProductMarker = "Bob-Shell"
	// piContextMarker is pi's context-meter suffix ("5.9%/1.0M (auto)" in
	// its TUI status bar). It is the PRIMARY readiness signal for a running
	// pi: the status bar renders only when the agent TUI is live and has a
	// model configured, and it is the only marker pi renders in common with
	// no other CLI (pi never shows "❯"/"goose is ready"). Matching on "%/"
	// rather than a context size keeps it valid at any model/context
	// configuration.
	piContextMarker = "%/"
)

// paneHasCLIMarker reports whether the given pane content contains any known
// CLI UI marker.
func paneHasCLIMarker(output string) bool {
	if output == "" {
		return false
	}
	for _, marker := range cliPaneMarkers {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

// agentWorkingMarkers are fragments that modern TUI backends (Claude Code /
// Claude Fable) render while the agent is actively processing a request. Unlike
// the older "esc to interrupt" footer captured by cliWorkingMarker, these are
// the shapes those backends emit today: "esc interrupt" is the abort hint on
// the streaming footer and "◉ Working" is the live activity spinner — both
// present in the mid-task Claude Fable 5 capture in #7085, alongside a fully
// rendered "❯" input box. paneShowsBusyFooter covers newer escape-action
// footers such as "esc stop agents" while waiting on background agents.
//
// bobBusyMarker is bob 2.x's equivalent: while a response streams it renders
// "⠇ Processing… (Enter to steer, Tab to queue)" above a still-painted "❯"
// input box, and none of the other markers match it. Without it a kicked
// bob turn was classified ended ~30s after delivery and its turn-end kick log
// archive was a mid-run snapshot (#15864).
var agentWorkingMarkers = []string{
	"esc interrupt",
	"◉ Working",
	bobBusyMarker,
}

// bobBusyMarker is the steer hint bob 2.x shows only while it is processing a
// request. Verified live on Bob-Shell 2.0.5 (#15864).
const bobBusyMarker = "Enter to steer"

// paneShowsAgentWorking reports whether the pane is showing an actively-working
// agent rather than a ready CLI input prompt. Modern TUI backends keep the "❯"
// input box rendered for the whole time a response streams, so a busy pane
// satisfies paneShowsInputPrompt's "❯" check and the readiness gate would pass
// — then deliverKickLocked sends Ctrl+C + /clear and destroys the in-flight
// work and every background sub-agent that session dispatched (#7085).
//
// This is the same class of false positive paneShowsConsentScreen guards
// against, and it takes the same precaution: callers must pass the VISIBLE pane
// only (no scrollback). A completed task's working marker lingers in history,
// and treating that as "still working" would wedge the agent — it would never
// be kicked again and the hive would stall silently. Backends that render no
// working marker (goose, codex) never match here and are unaffected.
func paneShowsAgentWorking(pane string) bool {
	if pane == "" {
		return false
	}
	if lastRunning := strings.LastIndex(pane, agyHeadlessRunningMarker); lastRunning >= 0 {
		lastReady := strings.LastIndex(pane, agyHeadlessReadyMarker)
		if lastReady < lastRunning {
			return true
		}
	}
	if paneShowsBusyFooter(pane) {
		return true
	}
	for _, marker := range agentWorkingMarkers {
		if strings.Contains(pane, marker) {
			return true
		}
	}
	return false
}

// paneShowsInputPrompt reports whether the pane content shows a CLI input
// prompt that is ready to accept a kick.
//
// The first four markers are the pre-existing set, preserved verbatim so
// claude/copilot/gemini/goose readiness is bit-for-bit unchanged. The bob
// placeholder is additive: bob's TUI renders none of the other four (verified
// against bobshell 1.0.6 — the bundle contains no "❯" at all), so without it a
// healthy bob never registers as ready and its startup kick is dropped.
//
// The codex caret (U+203A "›") is likewise additive: Codex 0.144.1's TUI
// renders none of the markers above (verified live — its idle pane contains no
// "❯" at all), so without it a healthy codex pane never registers as ready and
// its kick is dropped with "did not reach input prompt".
//
// Callers pass captured pane text; empty input is not a prompt.
func paneShowsInputPrompt(output string) bool {
	if output == "" {
		return false
	}
	return strings.Contains(output, "❯") ||
		strings.Contains(output, "goose is ready") ||
		strings.Contains(output, "> Enter to send") ||
		strings.Contains(output, "\n>\n") ||
		strings.Contains(output, bobInputPlaceholder) ||
		strings.Contains(output, bobInputPlaceholderDefault) ||
		strings.Contains(output, codexInputPromptMarker) ||
		strings.Contains(output, piContextMarker) ||
		strings.Contains(output, agyHeadlessReadyMarker) ||
		strings.Contains(output, "π >") ||
		strings.Contains(output, "╰─")
}

// paneShowsCopilotQuestionForm reports whether Copilot CLI is blocking on its
// interactive ask_user form instead of accepting a kick. Observed live
// 2026-10-01 on hive's telemetry agent:
//
//	○ Asking user Waiting for response
//	Copilot needs information.
//	What would you like me to actually do right now?
//	[Concrete task]  Target repo (if applicable)
//	Concrete task
//	e.g. 'Audit observability in hivecommons/pluk and file a telemetry issue' or 'Just explain the policy above'
//	❯
//	enter accept · tab next · ctrl+d decline · esc cancel
//
// Any one marker is enough because pane captures can be clipped to the visible
// terminal height. Callers should dismiss this with Escape, never Enter: Enter
// accepts the empty form as an answer and can start another turn.
func paneShowsCopilotQuestionForm(output string) bool {
	if output == "" {
		return false
	}
	return strings.Contains(output, "Copilot needs information.") ||
		strings.Contains(output, "Asking user") ||
		strings.Contains(output, "enter accept · tab next · ctrl+d decline · esc cancel")
}

// snapshot copies the agent's observable state into the crux.Process the
// manager hands out from GetStatus, GetStatusFast and AllStatuses.
func (a *AgentProcess) snapshot() crux.Process {
	history := make([]KickRecord, len(a.KickHistory))
	copy(history, a.KickHistory)
	a.paneMu.RLock()
	pane := make([]string, len(a.lastPaneCapture))
	copy(pane, a.lastPaneCapture)
	// NeedsLogin and LastPaneChange are written by the pane poller under paneMu.
	needsLogin := a.NeedsLogin
	loginURL := a.LoginURL
	quotaExhausted := a.QuotaExhausted
	var subAgentModels []SubAgentModel
	if effectiveBackend(a) == "copilot" && a.LastKick != nil && a.subAgentKick.Equal(*a.LastKick) {
		subAgentModels = append(subAgentModels, a.SubAgentModels...)
	}
	lastPaneChange := a.LastPaneChange
	conds := make([]watchdog.Condition, len(a.WatchdogConditions))
	copy(conds, a.WatchdogConditions)
	a.paneMu.RUnlock()
	snap := crux.Process{
		Name:                      a.Name,
		ID:                        a.ID,
		Config:                    a.Config,
		State:                     a.State,
		PID:                       a.PID,
		UID:                       a.UID,
		StartedAt:                 a.StartedAt,
		LastKick:                  a.LastKick,
		KicksUndeliverable:        a.KicksUndeliverable,
		BusySince:                 a.BusySince,
		LastTranscriptActivity:    a.LastTranscriptActivity,
		BusyCondition:             a.BusyCondition,
		BusyConditionMessage:      a.BusyConditionMessage,
		Paused:                    a.Paused,
		PausedAt:                  a.PausedAt,
		PausedReason:              a.PausedReason,
		PausedTrigger:             a.PausedTrigger,
		PausedBy:                  a.PausedBy,
		OperatorStopped:           a.OperatorStopped,
		PinnedCLI:                 a.PinnedCLI,
		PinnedModel:               a.PinnedModel,
		ModelOverride:             a.ModelOverride,
		BackendOverride:           a.BackendOverride,
		RestartCount:              a.RestartCount,
		RestartEvents:             cloneRestartEvents(a.RestartEvents),
		LastRestartReason:         a.LastRestartReason,
		TurnLoss:                  cloneTurnLoss(a.TurnLoss),
		KickHistory:               history,
		LastKickMessage:           a.LastKickMessage,
		KickRefused:               a.KickRefused,
		KickRefusalReason:         a.KickRefusalReason,
		BootstrapOverride:         a.BootstrapOverride,
		LastError:                 a.LastError,
		KickOutcome:               a.KickOutcome,
		SandboxLastRun:            cloneSandboxRun(a.SandboxLastRun),
		NeedsLogin:                needsLogin,
		LoginURL:                  loginURL,
		Starting:                  a.startupLaunchQueued || a.launching,
		QuotaExhausted:            quotaExhausted,
		LastPaneChange:            lastPaneChange,
		WatchdogConditions:        conds,
		StallNudges:               a.StallNudges,
		StallRecoveries:           a.StallRecoveries,
		SessionRSSMB:              a.SessionRSSMB,
		SessionRecycles:           a.SessionRecycles,
		CIPollNudges:              a.CIPollNudges,
		AwaitingCI:                a.AwaitingCI,
		ActionNudges:              a.ActionNudges,
		TransientNudges:           a.TransientNudges,
		ProviderErrorClass:        a.ProviderErrorClass,
		ProviderErrorLine:         a.ProviderErrorLine,
		ProviderErrorBackoffUntil: a.ProviderErrorBackoffUntil,
		BackendAuth:               a.BackendAuth,
		StartFailureClass:         a.StartFailureClass,
		StartFailureReason:        a.StartFailureReason,
		StartFailureCount:         a.StartFailureCount,
		StartFailureLastAt:        a.StartFailureLastAt,
		StartFailureExitCode:      a.StartFailureExitCode,
		StartFailureSignal:        a.StartFailureSignal,
		StartBlocked:              a.StartBlocked,
		StartBackoffUntil:         a.StartBackoffUntil,
		HasLaunched:               a.HasLaunched,
		LaunchedMode:              a.LaunchedMode,
		OutputBuffer:              a.OutputBuffer,
		SubAgentModels:            subAgentModels,
	}
	snap.SetPaneCapture(pane, filterPaneOutput)
	return snap
}

// PaneLines returns the last n lines from the most recent tmux pane capture,
// preferring content from the current CLI session (after the last ❯ prompt).
// Falls back to showing the full tail if the current session has too few lines.
func (a *AgentProcess) PaneLines(n int) []string {
	a.paneMu.RLock()
	defer a.paneMu.RUnlock()
	if len(a.lastPaneCapture) == 0 {
		return nil
	}
	return filterPaneOutput(a.lastPaneCapture, n)
}

func isVisualNoise(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true
	}
	if strings.Trim(t, "─━│┃┌┐└┘┏┓┗┛╭╮╰╯├┤┬┴┼╞╡╪═║╔╗╚╝╟╢╤╧╫╠╣╦╩╬╾╼╿╽╸╺╹╻╴╶╵╷ ") == "" {
		return true
	}
	if strings.HasPrefix(t, "/data/agents/") && !strings.Contains(t, " ") {
		return true
	}
	return false
}

func isCLIChrome(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true
	}
	lower := strings.ToLower(t)
	if strings.HasPrefix(t, "/ commands") ||
		strings.HasPrefix(t, "? help") ||
		strings.HasPrefix(t, "@ files") ||
		strings.HasPrefix(t, "# issues") {
		return true
	}
	if strings.Contains(t, "```") {
		return true
	}
	if strings.Contains(lower, "esc cancel") {
		return true
	}
	keyHints := 0
	for _, hint := range []string{
		"open sidebar", "/ commands", "? help", "tab next tab", "esc to interrupt",
		"esc interrupt", "esc cancel", "ctrl+c", "ctrl-c", "ctrl + c", "enter to send",
		"shift+tab", "shift + tab", "↑/↓ to navigate",
	} {
		if strings.Contains(lower, hint) {
			keyHints++
		}
	}
	if keyHints >= 2 && len([]rune(t)) <= 180 {
		return true
	}
	if keyHints == 1 && len([]rune(t)) <= 180 &&
		(strings.HasPrefix(t, "← ") || strings.Contains(t, " · ") || strings.Contains(t, " • ")) {
		return true
	}
	if strings.HasPrefix(t, "← ") && (strings.Contains(lower, "open sidebar") || strings.Contains(lower, "? help")) {
		return true
	}
	if strings.HasPrefix(t, "↵") || strings.HasPrefix(t, "⏎") {
		return true
	}
	spinnerGlyphs := []string{"◎", "◉", "●", "○", "◐", "◑", "◒", "◓", "⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	hasSpinner := false
	for _, prefix := range spinnerGlyphs {
		if strings.Contains(t, prefix) {
			hasSpinner = true
		}
		if strings.HasPrefix(t, prefix) && len([]rune(t)) <= 160 &&
			(strings.Contains(lower, "working") || strings.Contains(lower, "thinking") ||
				strings.Contains(lower, "processing") || strings.Contains(lower, "tokens") ||
				strings.Contains(lower, "esc") || strings.Contains(lower, "ctrl+c")) {
			return true
		}
	}
	// Model/status bars for Copilot, Claude, Codex, Gemini, bob, goose and
	// similar TUIs. Keep this bounded to short chrome-shaped lines so ordinary
	// prose mentioning a tool name survives.
	if len([]rune(t)) <= 180 &&
		(strings.Contains(lower, "claude ") || strings.Contains(lower, "copilot") ||
			strings.Contains(lower, "gemini") || strings.Contains(lower, "openai codex") ||
			strings.Contains(lower, "codex") || strings.Contains(lower, "bob-shell") ||
			strings.Contains(lower, "goose")) &&
		(strings.Contains(lower, "esc") || strings.Contains(lower, "ctrl+c") ||
			strings.Contains(lower, "/ commands") || strings.Contains(lower, "? help") ||
			strings.Contains(lower, "tokens") || strings.Contains(lower, "context") ||
			strings.Contains(lower, "%/") ||
			(hasSpinner && (strings.Contains(lower, "thinking") || strings.Contains(lower, "working") ||
				strings.Contains(lower, "loading") || strings.Contains(lower, "processing")))) {
		return true
	}
	return false
}

func isBufferNoise(s string) bool {
	if isCLIChrome(s) || isVisualNoise(s) {
		return true
	}
	t := strings.TrimSpace(s)
	if t == "❯" || t == "›" || t == ">" {
		return true
	}
	if t == bobInputPlaceholder || t == bobInputPlaceholderDefault {
		return true
	}
	if strings.HasPrefix(t, "› ") && len([]rune(t)) <= 120 {
		lower := strings.ToLower(t)
		for _, placeholder := range []string{
			"improve documentation", "explain this codebase", "ask anything", "type a message",
			"type your message", "enter your prompt", "message or @path", "what do you want",
		} {
			if strings.Contains(lower, placeholder) {
				return true
			}
		}
	}
	for _, banner := range []string{"╭─╮", "╰─╯", "█ ▘▝ █", "▔▔▔▔", "Copilot v", "Check for mistakes"} {
		if strings.Contains(t, banner) {
			return true
		}
	}
	if strings.HasPrefix(t, "● Tip:") || strings.HasPrefix(t, "└ ") || strings.HasPrefix(t, "↑/↓ to navigate") {
		return true
	}
	if strings.Contains(t, "copilot-instructions.md") && strings.Contains(t, "/init") {
		return true
	}
	if strings.Contains(t, "Do you trust the files in this folder") {
		return true
	}
	if strings.HasPrefix(t, "› ") && (strings.Contains(t, "Yes") || strings.Contains(t, "No (Esc)")) {
		return true
	}
	if strings.HasPrefix(t, "●") && strings.Contains(t, "Folder") && strings.Contains(t, "trusted") {
		return true
	}
	if strings.HasPrefix(t, "✗ Model") && strings.Contains(t, "not available") {
		return true
	}
	return false
}

func filterPaneOutput(lines []string, n int) []string {
	lastPrompt := -1
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "❯" || trimmed == "›" || trimmed == ">" {
			lastPrompt = i
			break
		}
	}
	if lastPrompt >= 0 && lastPrompt < len(lines)-1 {
		afterPrompt := lines[lastPrompt+1:]
		hasContent := false
		for _, l := range afterPrompt {
			if !isCLIChrome(l) && !isVisualNoise(l) {
				hasContent = true
				break
			}
		}
		if hasContent {
			lines = afterPrompt
		} else {
			lines = lines[:lastPrompt]
		}
	}
	var cleaned []string
	for _, l := range lines {
		if !isBufferNoise(l) {
			cleaned = append(cleaned, l)
		}
	}
	lines = cleaned
	lines = DeduplicateBlocks(lines)
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, len(lines))
	copy(out, lines)
	return out
}

// SanitizePaneText removes terminal/TUI chrome from pane-derived text before it
// is shown in chat, notifications or activity summaries. The input is expected
// to be display text, not a shell transcript for replay.
func SanitizePaneText(text string, maxLines int) string {
	lines := strings.Split(text, "\n")
	lines = filterPaneOutput(lines, maxLines)
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// DeduplicateBlocks removes repeated blocks from pane output.
// It finds the longest suffix that also appears earlier (non-overlapping) and
// removes the earlier copy, then repeats until nothing more can be removed.
//
// Lines are normalized ONCE up front and interned to integer ids; block
// matching then runs on a suffix-match table (Z-algorithm over the reversed
// ids) so each pass is O(n²) integer compares at worst instead of the previous
// O(n³) with two allocating normalizeLine calls per compare. The old shape took
// seconds per 500-line agent buffer on near-repeating spinner output and, run
// for every agent on every status rebuild, wedged the dashboard snapshot
// pipeline: rebuilds outlived the next mutation epoch and were dropped, so
// statusSeq stopped advancing.
func DeduplicateBlocks(lines []string) []string {
	if len(lines) < 4 {
		return lines
	}
	ids := internNormalized(lines)
	cur := lines
	changed := false
	for {
		n := len(cur)
		if n < 4 {
			break
		}
		z := suffixMatchLengths(ids)
		start := -1
		// Largest block first; within a block size, the latest earlier
		// occurrence first — the same search order as the original scan.
		for b := n / 2; b >= 2 && start < 0; b-- {
			for e := n - b - 1; e >= b-1; e-- {
				if z[e] >= b {
					start = e - b + 1
					// Remove the earlier duplicate block.
					next := make([]string, 0, n-b)
					next = append(next, cur[:start]...)
					next = append(next, cur[start+b:]...)
					cur = next
					ids = append(ids[:start:start], ids[start+b:]...)
					break
				}
			}
		}
		if start < 0 {
			break
		}
		changed = true
	}
	if !changed {
		return lines
	}
	return cur
}

// internNormalized maps each line to a small integer id such that two lines
// share an id iff their normalizeLine forms are equal.
func internNormalized(lines []string) []int32 {
	ids := make([]int32, len(lines))
	table := make(map[string]int32, len(lines))
	for i, l := range lines {
		key := normalizeLine(l)
		id, ok := table[key]
		if !ok {
			id = int32(len(table))
			table[key] = id
		}
		ids[i] = id
	}
	return ids
}

// suffixMatchLengths returns z where z[e] is the length of the longest block
// ending at index e that equals the block of the same length ending at the
// last index (i.e. the longest common suffix of ids[:e+1] and ids). It is the
// Z-algorithm run over the reversed sequence.
func suffixMatchLengths(ids []int32) []int {
	n := len(ids)
	rev := make([]int32, n)
	for i := range ids {
		rev[i] = ids[n-1-i]
	}
	zr := make([]int, n)
	zr[0] = n
	l, r := 0, 0
	for i := 1; i < n; i++ {
		if i < r {
			zr[i] = min(r-i, zr[i-l])
		}
		for i+zr[i] < n && rev[zr[i]] == rev[i+zr[i]] {
			zr[i]++
		}
		if i+zr[i] > r {
			l, r = i, i+zr[i]
		}
	}
	z := make([]int, n)
	for e := range z {
		z[e] = zr[n-1-e]
	}
	return z
}

func (a *AgentProcess) FilteredPaneLines(n int) []string {
	a.paneMu.RLock()
	defer a.paneMu.RUnlock()
	if len(a.lastPaneCapture) == 0 {
		return nil
	}
	return filterPaneOutput(a.lastPaneCapture, n)
}

// codexInputPromptMarker is the caret Codex renders on its input line when it
// is idle and awaiting input. It is Codex's equivalent of claude/gemini's "❯"
// and bob's placeholder — the PRIMARY readiness signal for a codex agent.
//
// It is a SINGLE-ANGLE-QUOTATION-MARK (U+203A "›"), deliberately distinct from
// the "❯" (U+276F) used by the other TUIs and by the consent-screen menu, so it
// never collides with paneShowsConsentScreen's "❯"-selected-line check.
//
// Verified live on Codex 0.144.1 (daviddiaz "Visual Hive", the hub-reachable cluster): an idle
// scanner pane sitting at its prompt rendered this caret with placeholder
// ghost-text ("› Improve documentation in @filename", "› Explain this
// codebase") and contained ZERO "❯", "goose is ready", "> Enter to send", or
// bob placeholders — so without this marker a healthy codex pane never
// registers as ready and every kick is dropped with "did not reach input
// prompt", leaving the advisory issue stale.
//
// Matching the caret alone (not any specific placeholder string) is robust to
// the ghost-text varying between Codex tips/versions, and stays tight: Codex
// shows this idle input caret ONLY while awaiting input — a running turn
// renders streaming output and a working indicator instead, not the "›" caret.
const codexInputPromptMarker = "›"

// codexProductMarker is Codex's product name, rendered in its splash banner
// ("OpenAI Codex (v0.144.1)"). Like bobProductMarker it is a coarse
// CLI-presence signal only (it also shows on the splash before the input caret
// is live), never the input-ready gate — that is codexInputPromptMarker.
const codexProductMarker = "OpenAI Codex"

// copilotModelFailureMarker is the Copilot CLI's equivalent of Claude Code's
// "API Error:" chrome. The copilot backend never emits "API Error:" at all, so
// gating solely on that string made this whole watchdog blind to every copilot
// agent: the CLI exhausted its own retries, printed the line below, dropped
// back to its idle prompt, and then sat there until the next cadence kick —
// which on an hourly cadence wastes most of an hour of a live session.
//
// Observed verbatim on the hosted console spoke's scanner agent:
//
//	Execution failed: Error: Failed to get response from the AI model;
//	retried 5 times (total retry wait time: 6.00 seconds) Last error:
//	Failed native model HTTP request: error decoding response body:
//	request or response body error: error reading a body from connection:
//	cannot decrypt peer's message
const copilotModelFailureMarker = "failed to get response from the ai model"

// copilotTransportErrorPatterns are the transport-layer failures the Copilot
// CLI reports underneath copilotModelFailureMarker. Each one means the request
// did not complete, so repeating it can succeed — the same admission test the
// Claude-side list above applies.
//
// The marker alone is deliberately NOT sufficient. Copilot wraps every model
// failure in that sentence, including ones a retry cannot fix, so a bare match
// would nudge an agent in a loop against an auth or quota wall. Requiring a
// transport cause keeps membership as narrow as the Claude-side list, and the
// call site still re-checks authorization and quota independently.
var copilotTransportErrorPatterns = []string{
	"failed native model http request",
	"error decoding response body",
	"error reading a body from connection",
	// TLS record failure seen when the connection is torn down mid-body;
	// transport-level, never a property of the request content.
	"cannot decrypt peer's message",
}
