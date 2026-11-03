package agent

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
)

// ciPollCommands returns each CI-status command in pane as its normalized
// command line (from the command name to end of line), skipping mentions
// that are prohibitions quoted from the kick prompt or policy: those landed
// in the pane right after the kick and read as poll commands issued 0s in.
func ciPollCommands(pane string) []string {
	pane = stripExplainLines(pane)
	var cmds []string
	for _, loc := range ciPollCommandRe.FindAllStringIndex(pane, -1) {
		lineStart := strings.LastIndex(pane[:loc[0]], "\n") + 1
		if ciPollProhibitionRe.MatchString(pane[lineStart:loc[0]]) {
			continue
		}
		line, _, _ := strings.Cut(pane[loc[0]:], "\n")
		// Failure diagnosis can revisit the same log while comparing jobs or
		// extracting evidence. It does not wait for a check to finish (#14382).
		if ciLogReadRe.MatchString(line) {
			continue
		}
		cmds = append(cmds, strings.Join(strings.Fields(strings.TrimRight(line, " \t┃│")), " "))
	}
	return cmds
}

// countCIPollCommands counts CI-status commands in pane.
func countCIPollCommands(pane string) int {
	return len(ciPollCommands(pane))
}

// ciPollCommandsSinceBaseline returns the commands in current that were run
// after baseline was taken. The pane capture holds only the newest
// tmuxCaptureLines lines and evicts the oldest first, so baseline is a prefix
// of the kick's full command sequence and current is a suffix of it. The
// longest baseline tail that opens current is what is still visible; the rest
// of current is new. Subtracting per-command counts instead let reads that had
// scrolled out cancel genuine new ones (#14065).
func ciPollCommandsSinceBaseline(current, baseline []string) []string {
	for k := min(len(current), len(baseline)); k > 0; k-- {
		if slices.Equal(baseline[len(baseline)-k:], current[:k]) {
			return current[k:]
		}
	}
	return current
}

// countRepeatedCIPollCommands counts CI-status commands whose command line
// appears more than once since baseline. Waiting on one PR re-reads the same
// target; a merge sweep or CI-repair pass reads each of many PRs once, and
// that is the lane's work, not polling (#14016). Reads already in baseline are
// dropped BEFORE deciding whether a command repeats: one read in each of two
// kicks is not repeated polling in either kick (#14061).
func countRepeatedCIPollCommands(pane string, baseline []string) int {
	counts := map[string]int{}
	for _, cmd := range ciPollCommandsSinceBaseline(ciPollCommands(pane), baseline) {
		counts[cmd]++
	}
	n := 0
	for _, c := range counts {
		if c > 1 {
			n += c
		}
	}
	return n
}

// repeatedCIPollCommandLines lists each command line run more than once since
// baseline as "<count>× <command>", most-repeated first then by command, so a
// ci_poll_nudge report shows which reads repeated (#14289).
func repeatedCIPollCommandLines(pane string, baseline []string) []string {
	counts := map[string]int{}
	for _, cmd := range ciPollCommandsSinceBaseline(ciPollCommands(pane), baseline) {
		counts[cmd]++
	}
	var cmds []string
	for cmd, c := range counts {
		if c > 1 {
			cmds = append(cmds, cmd)
		}
	}
	slices.SortFunc(cmds, func(a, b string) int {
		if counts[a] != counts[b] {
			return counts[b] - counts[a]
		}
		return strings.Compare(a, b)
	})
	lines := make([]string, 0, len(cmds))
	for _, cmd := range cmds {
		lines = append(lines, fmt.Sprintf("%d× %s", counts[cmd], RedactPaneTokens(cmd)))
	}
	return lines
}

// reviewLaneAgent mirrors the review swarm's own admission rule
// (review.hasReviewCapability): a name or role carrying "review" is a
// reviewer. pkg/review is not imported here (cycle risk); the token is tiny.
func reviewLaneAgent(agent *AgentProcess) bool {
	for _, f := range []string{agent.Name, agent.Config.Role} {
		if strings.Contains(strings.ToLower(f), "review") {
			return true
		}
	}
	return false
}

// ciPollNudgeExemptAgent identifies lanes whose work inherently reads CI
// status across repositories. The guard cannot distinguish those operational
// reads from an agent polling its own PR, so nudging these lanes interrupts the
// work they were kicked to do.
func ciPollNudgeExemptAgent(agent *AgentProcess) bool {
	if reviewLaneAgent(agent) {
		return true
	}
	for _, f := range []string{agent.Name, agent.Config.Role} {
		if exemptCIPollLane(f) {
			return true
		}
	}
	return false
}

// exemptCIPollLane matches a lane name or role exactly, or as a dash/underscore-
// suffixed variant ("ci-maintainer-2", "quality_full"), so replicas and policy
// variants of an exempt lane are exempt too.
func exemptCIPollLane(f string) bool {
	lane := strings.ToLower(strings.TrimSpace(f))
	for _, base := range []string{"ci-maintainer", "quality"} {
		if lane == base || strings.HasPrefix(lane, base+"-") || strings.HasPrefix(lane, base+"_") {
			return true
		}
	}
	return false
}

// nudgeIfPollingCI tells an agent that keeps polling CI within one kick to
// move on. Policy templates already forbid it; this is the harness backstop.
// Sends at most one nudge per kick, and only at an idle prompt so it never
// splices into a running response.
//
// Two independent triggers count as "polling CI" (#9673's ask, item 2): more
// than ciPollNudgeThreshold repeated poll commands this kick (the same
// command line run more than once, #14016), OR at least one poll
// command with the kick already running longer than ciPollNudgeWallClock — a
// single `gh run watch` blocks for the whole wait and would otherwise never
// cross the count threshold. The wall-clock trigger skips a sweep: a scanner
// reading each of several PRs once late in a long kick is doing its work, not
// waiting on one PR (#14066).

func (m *Manager) nudgeIfPollingCI(agent *AgentProcess, scrollback, visible string) {
	if agent.kickDelivering.Load() || agent.LastKick == nil {
		return
	}
	// Reviewers, CI maintainers and quality agents read check status as evidence
	// for the PRs and repositories they inspect. Those reads are not an own-PR
	// wait loop, which this guard cannot identify from pane text alone
	// (#13860, #13898, #14188).
	if ciPollNudgeExemptAgent(agent) {
		return
	}
	cmds := ciPollCommands(scrollback)
	m.mu.Lock()
	if agent.ciPollBaseline < 0 {
		agent.ciPollBaseline = len(cmds)
		agent.ciPollCommandBaseline = cmds
		m.mu.Unlock()
		return
	}
	newPolls := len(ciPollCommandsSinceBaseline(cmds, agent.ciPollCommandBaseline))
	newRepeats := countRepeatedCIPollCommands(scrollback, agent.ciPollCommandBaseline)
	overThreshold := newRepeats > ciPollNudgeThreshold ||
		(newPolls > 0 && !ciPollSweep(newPolls, newRepeats) && time.Since(*agent.LastKick) > ciPollNudgeWallClock)
	alreadyNudged := agent.ciPollNudgeSent
	m.mu.Unlock()

	// idlePrompt is the same idle-at-prompt gate the nudge below requires: the
	// agent stopped producing new output after its polling ran. AwaitingCI
	// (#9673 item 3) uses it too, so the dashboard's "Waiting on CI" state
	// self-clears the instant the agent starts doing something else, rather
	// than sticking until the next kick.
	idlePrompt := !paneShowsActiveWork(visible) && paneShowsEmptyInputPrompt(visible)
	m.mu.Lock()
	agent.AwaitingCI = overThreshold && idlePrompt
	m.mu.Unlock()

	if alreadyNudged || !overThreshold || !idlePrompt {
		return
	}
	if m.tmuxSessionHasAttachedClientForAgent(agent) {
		return
	}
	m.mu.Lock()
	agent.ciPollNudgeSent = true
	agent.CIPollNudges++
	totalCIPollNudges := agent.CIPollNudges
	lane := agent.Config.Role
	var sinceKick time.Duration
	if agent.LastKick != nil {
		sinceKick = time.Since(*agent.LastKick).Round(time.Minute)
	}
	m.mu.Unlock()

	// Self-report (#13470, was the hive-code-defect "CI-poll nudge" counter):
	// the harness caps the nudge at one per kick, so every observation is a
	// distinct kick in which the agent repeated CI-status commands; whether
	// the target was its own PR cannot be inferred from pane text (#14384).
	// A lone poll past the wall clock is a normal post-push check, and one
	// read each of several PRs is a sweep, so only repeated polling of the
	// same target reaches the fleet; the in-session nudge is unchanged.
	if ciPollReportWorthy(newRepeats) {
		m.mu.Lock()
		repeated := repeatedCIPollCommandLines(scrollback, agent.ciPollCommandBaseline)
		m.mu.Unlock()
		m.observeCondition(crux.ClassCIPollNudge, agent.Name,
			fmt.Sprintf("repeated CI-status commands observed after the kick: stop-polling nudge sent (%d repeated of %d poll command(s) since the kick, %s after it; %d nudge(s) since boot)", newRepeats, newPolls, roundDurationForHumans(sinceKick), totalCIPollNudges),
			fmt.Sprintf("agent=%s lane=%s polls_total=%d polls_since_kick=%d repeated_since_kick=%d threshold=%d wall_clock=%s report_min_polls=%d\n--- pane tail (%d lines) ---\n%s\n--- repeated poll commands ---\n%s",
				agent.Name, lane, len(cmds), newPolls, newRepeats, ciPollNudgeThreshold, ciPollNudgeWallClock, ciPollReportMinPolls, stallNudgeTailLines, paneTail(visible, stallNudgeTailLines), strings.Join(repeated, "\n")))
	}

	m.logger.Warn("agent is polling CI, sending stop-polling nudge", "name", agent.Name, "polls", len(cmds))
	m.tmuxSendLiteralForAgent(agent, ciPollNudgeMessage)
	time.Sleep(textToEnterDelay)
	m.tmuxSendEntersForAgent(agent)
}

// ciPollSweep reports whether the poll commands since the kick read several
// targets once each: more than one command and none of them repeated. A lone
// poll is not a sweep; it may be the blocking `gh run watch` the wall-clock
// trigger exists for.
func ciPollSweep(newPolls, newRepeats int) bool {
	return newPolls > 1 && newRepeats == 0
}

// ciPollReportWorthy reports whether a CI-poll nudge is repeated polling
// (at least ciPollReportMinPolls repeated commands since the kick) rather
// than a single check or a sweep.
func ciPollReportWorthy(newRepeats int) bool {
	return newRepeats >= ciPollReportMinPolls
}
