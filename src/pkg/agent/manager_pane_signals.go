package agent

import (
	"strings"

	"github.com/hivecommons/hive/pkg/watchdog"
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
// rendered "❯" input box.
var agentWorkingMarkers = []string{
	"esc interrupt",
	"◉ Working",
}

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
	for _, marker := range agentWorkingMarkers {
		if strings.Contains(pane, marker) {
			return true
		}
	}
	return false
}

// blockingPrompt is a startup-blocking modal that must be answered with a
// SPECIFIC numbered option rather than a bare Enter or a generic
// navigate-away-from-"No" heuristic.
//
// The generic heuristic in dismissInferencePrompts steers away from options
// containing "no"/"exit" and otherwise confirms whatever is selected. That is
// wrong for menus whose DEFAULT selection is affirmative but harmful — most
// notably codex's update prompt, where the pre-selected option shells out to
// `npm install -g`. Answering those needs the exact key, so each entry names
// the one prompt it answers and nothing else is ever blind-fired at.
//
// This mirrors blockingPromptKey() in bin/contributor-relay.js, which solved
// the same problem contributor-side. The hub had no equivalent.
type blockingPrompt struct {
	// backend this prompt belongs to. Prompts are matched only against the
	// backend that actually renders them, so a codex pattern can never fire at
	// a claude pane that happens to contain similar words.
	backend string
	// match reports whether this prompt is the one on screen. All conditions
	// must hold, so a prompt is only answered when positively identified.
	match func(pane string) bool
	key   string // the option to type before Enter
	label string // for the audit log
}

var blockingPrompts = []blockingPrompt{
	{
		backend: "claude",
		// Claude Code: "Quick safety check: Is this a project you created or
		// one you trust?" defaults to "No, exit". If it is not answered before
		// the kick path runs, Claude exits and the prompt text lands in bash.
		match: func(p string) bool {
			return strings.Contains(p, "Is this a project you created or one you trust") &&
				strings.Contains(p, "Yes, I trust this folder")
		},
		key:   "Down",
		label: "claude workspace trust",
	},
	{
		backend: "copilot",
		// Copilot: "Confirm folder trust" → 1. Yes (THIS SESSION ONLY).
		//
		// Deliberately NOT "2. Yes, and remember": remembering makes the CLI
		// rewrite the SHARED ~/.copilot/config.json from its own in-memory
		// snapshot, which stomps every other agent's state in that file — traced
		// live on hivecommons/hive (2026-08-22): each agent's "remember" wiped
		// the others' trustedFolders entries, and one stale rewrite resurrected
		// a dead token over the operator's fresh login, which is why re-logins
		// never stuck. Session-only trust writes NOTHING; the watcher now runs
		// for the agent's whole lifetime, so every (re)launch gets re-answered
		// and persistence is unnecessary.
		match: func(p string) bool {
			return strings.Contains(p, "Confirm folder trust") || strings.Contains(p, "Do you trust the files")
		},
		key:   "1",
		label: "copilot folder trust",
	},
	{
		backend: "agy",
		// agy (Antigravity CLI): "Do you trust the contents of this project?"
		// An arrow-key menu whose affirmative option is ALREADY selected, so a
		// bare Enter is correct and there is no numbered option to type. It
		// blocks startup exactly like the codex/copilot trust dialogs.
		match: func(p string) bool {
			return strings.Contains(p, "Do you trust the contents of this project")
		},
		key:   "",
		label: "agy project trust",
	},
	{
		backend: "omp",
		match: func(p string) bool {
			lower := strings.ToLower(p)
			return strings.Contains(lower, "welcome to omp") && strings.Contains(lower, "press enter to skip")
		},
		key:   "Enter",
		label: "omp onboarding",
	},
	{
		backend: "codex",
		// codex: "Do you trust the contents of this directory?" → 1. Yes, continue.
		match: func(p string) bool {
			return strings.Contains(p, "Do you trust the contents of this directory")
		},
		key:   "1",
		label: "codex directory trust",
	},
	{
		backend: "codex",
		// codex: "✨ Update available! x -> y" → 3. Skip until next version.
		//
		// Deliberately NOT "1. Update now", which is the PRE-SELECTED option:
		// it runs `npm install -g @openai/codex` as the unprivileged agent UID,
		// which fails with EACCES and takes the CLI down with it — on every
		// launch, indefinitely, until a human intervenes. Even where it could
		// succeed it is slow, needs network, can fail half-way, and drifts the
		// CLI version out from under the image.
		//
		// "Skip until next version" is chosen over a plain "Skip" because it
		// persists: a plain Skip re-prompts on the very next launch.
		match: func(p string) bool {
			return strings.Contains(p, "Update available!") && strings.Contains(p, "Skip until next version")
		},
		key:   "3",
		label: codexUpdatePromptLabel,
	},
}

// blockingPromptTailLines bounds how much of the pane a prompt may be matched
// in. captureTmuxPaneForAgent returns SCROLLBACK, not just the visible screen,
// so matching the whole capture answers prompts that have long since scrolled
// away: after a codex CLI died, its update menu stayed in history and the
// watcher typed "3" into the bash shell that replaced it, once every poll,
// forever. A live modal is always at the bottom of the pane, so only the tail
// is eligible.
const blockingPromptTailLines = 25

// paneTail returns the last n non-blank lines of a captured pane.
func paneTail(pane string, n int) string {
	lines := strings.Split(pane, "\n")
	kept := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		kept = append(kept, lines[i])
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return strings.Join(kept, "\n")
}

// blockingPromptKey returns the keystroke that dismisses whatever known
// startup-blocking modal this backend currently has on screen, and whether one
// was recognised. Only the tail of the pane is considered — see
// blockingPromptTailLines.
func blockingPromptKey(backend, pane string) (key, label string, ok bool) {
	tail := paneTail(pane, blockingPromptTailLines)
	for _, p := range blockingPrompts {
		if p.backend == backend && p.match(tail) {
			return p.key, p.label, true
		}
	}
	return "", "", false
}

// PaneShowsBlockingPrompt reports whether the pane is currently sitting on a
// known startup-blocking modal (folder trust, codex update, …) for the given
// backend. The login-detector uses it to stand down: a trust-wedged pane is
// NOT a login problem, and pausing the agent for it kills the very watcher
// that would answer the prompt — the deadlock that kept hivecommons/hive's
// copilot agents "sitting at login prompt" through every re-login (2026-08-22).
func PaneShowsBlockingPrompt(backend, pane string) bool {
	_, _, ok := blockingPromptKey(backend, pane)
	return ok
}

// paneHasBlockingPrompt reports whether ANY known blocking prompt is on the
// pane, regardless of backend. Used by the readiness gate, which does not know
// the backend; the patterns are specific enough that a false positive only
// delays readiness rather than mis-firing a keystroke.
func paneHasBlockingPrompt(pane string) bool {
	tail := paneTail(pane, blockingPromptTailLines)
	for _, p := range blockingPrompts {
		if p.match(tail) {
			return true
		}
	}
	return false
}

// backendHasBlockingPrompts reports whether any known startup-blocking prompt
// belongs to this backend, and so whether the watcher is worth running for it.
//
// Derived from the table rather than hardcoded: the watcher used to be gated on
// `backend == "copilot"`, which is why codex agents were never rescued from
// their update menu even after the menu itself was understood. Adding a prompt
// for a new backend now enables the watcher for it automatically.
func backendHasBlockingPrompts(backend string) bool {
	for _, p := range blockingPrompts {
		if p.backend == backend {
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
		strings.Contains(output, "π >") ||
		strings.Contains(output, "╰─")
}

func (a *AgentProcess) snapshot() AgentProcess {
	history := make([]KickRecord, len(a.KickHistory))
	copy(history, a.KickHistory)
	a.paneMu.RLock()
	pane := make([]string, len(a.lastPaneCapture))
	copy(pane, a.lastPaneCapture)
	// NeedsLogin and LastPaneChange are written by the pane poller under paneMu.
	needsLogin := a.NeedsLogin
	quotaExhausted := a.QuotaExhausted
	lastPaneChange := a.LastPaneChange
	conds := make([]watchdog.Condition, len(a.WatchdogConditions))
	copy(conds, a.WatchdogConditions)
	a.paneMu.RUnlock()
	return AgentProcess{
		Name:                      a.Name,
		ID:                        a.ID,
		Config:                    a.Config,
		State:                     a.State,
		PID:                       a.PID,
		UID:                       a.UID,
		StartedAt:                 a.StartedAt,
		LastKick:                  a.LastKick,
		Paused:                    a.Paused,
		PausedAt:                  a.PausedAt,
		PausedReason:              a.PausedReason,
		PausedTrigger:             a.PausedTrigger,
		PausedBy:                  a.PausedBy,
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
		KickOutcome:               a.KickOutcome,
		NeedsLogin:                needsLogin,
		QuotaExhausted:            quotaExhausted,
		LastPaneChange:            lastPaneChange,
		WatchdogConditions:        conds,
		StallNudges:               a.StallNudges,
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
		tmuxSession:               a.tmuxSession,
		tmuxSocket:                a.tmuxSocket,
		OutputBuffer:              a.OutputBuffer,
		lastPaneCapture:           pane,
	}
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
	if strings.Trim(t, "─━") == "" {
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
	if strings.HasPrefix(t, "/ commands") ||
		strings.HasPrefix(t, "? help") ||
		strings.HasPrefix(t, "@ files") ||
		strings.HasPrefix(t, "# issues") {
		return true
	}
	// Copilot/Claude/Gemini status bar: contains "esc cancel" or model name
	if strings.Contains(t, "esc cancel") {
		return true
	}
	// Model name in status bar (short line with model identifier)
	if (strings.Contains(t, "Claude ") && !strings.Contains(t, "Claude Code")) ||
		strings.Contains(t, "Copilot v") ||
		strings.Contains(t, "Gemini ") {
		// Only match if it looks like a status bar (has spinner or command hints)
		for _, prefix := range []string{"◎", "◉", "●", "○", "◐", "◑", "◒", "◓"} {
			if strings.Contains(t, prefix) {
				return true
			}
		}
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
		if !isVisualNoise(l) {
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
