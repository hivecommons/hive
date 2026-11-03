package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// Session recycle (#14842). A CLI session's V8 heap grows with every turn
// and never shrinks on idle: on the hosted hive, active sessions gained
// 35–65 MiB per six minutes until the pod OOM'd. The health tick samples
// each agent's CLI process-tree RSS and, once a session is over its RSS or
// age budget AND idle at the input prompt, ends it through the ordinary
// restart path so claims survive exactly as they do on any other restart.

const (
	sessionRecycleReason = "session recycle"
	// sessionRecycleStartupGrace keeps a freshly launched session out of
	// the policy while the CLI is still booting, regardless of config.
	sessionRecycleStartupGrace = 5 * time.Minute
)

// procTreeRSSMB sums VmRSS (MiB) over rootPid and every descendant found
// under root (a /proc mount). It scans the tree once, so the cost is one
// readdir plus one small read per process, not one per agent per process.
// Missing or unreadable entries (a child that exited mid-scan) are skipped.
func procTreeRSSMB(root string, rootPid int) int {
	if rootPid <= 0 {
		return 0
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	children := map[int][]int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		ppid, ok := procPPID(filepath.Join(root, e.Name(), "stat"))
		if !ok {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}
	totalKB := 0
	seen := map[int]bool{}
	stack := []int{rootPid}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		totalKB += procVmRSSKB(filepath.Join(root, strconv.Itoa(pid), "status"))
		stack = append(stack, children[pid]...)
	}
	return totalKB / 1024
}

// procPPID parses the parent pid (field 4) out of /proc/<pid>/stat. The
// comm field is parenthesised and may itself contain spaces or parens, so
// the fields are taken from after the LAST ')'.
func procPPID(statPath string) (int, bool) {
	data, err := os.ReadFile(statPath)
	if err != nil {
		return 0, false
	}
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(string(data[i+1:]))
	// fields[0] is state, fields[1] is ppid.
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// procVmRSSKB reads the VmRSS line of /proc/<pid>/status; 0 when absent
// (kernel threads, or a process that exited between readdir and read).
func procVmRSSKB(statusPath string) int {
	data, err := os.ReadFile(statusPath)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "VmRSS:"))
		if len(fields) == 0 {
			return 0
		}
		kb, err := strconv.Atoi(fields[0])
		if err != nil {
			return 0
		}
		return kb
	}
	return 0
}

// tmuxPanePIDForAgent asks tmux for the shell pid of the agent's pane; the
// CLI and its helpers are that shell's descendants.
func (m *Manager) tmuxPanePIDForAgent(agent *AgentProcess) int {
	if agent == nil || agent.tmuxSession == "" {
		return 0
	}
	out, err := m.tmuxCmd(agent, "display-message", "-p", "-t", agent.tmuxSession, "#{pane_pid}").Output()
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return pid
}

// sessionRecycleInput is everything the pure policy needs about one agent,
// gathered by the health tick so the decision itself has no clock, lock or
// tmux dependency and is table-testable.
type sessionRecycleInput struct {
	Policy config.SessionRecycleConfig
	RSSMB  int
	// StartedAt is when the current CLI session launched; zero means
	// unknown, which never recycles on age.
	StartedAt time.Time
	// LastActivity is the latest of transcript activity, pane change and
	// kick delivery; zero means "no evidence", which counts as idle only if
	// the session is older than MinIdle.
	LastActivity time.Time
	LastRecycle  time.Time
	// Pane is the visible pane capture; the idle test is made against it.
	Pane string
	// KickInFlight is true while a kick is being typed, dispatched
	// asynchronously, or queued for startup — the turn has not started
	// in the pane yet, so the pane alone would look idle.
	KickInFlight bool
}

// sessionRecycleDecision returns the reason to recycle, or "" to leave the
// session alone. Over-budget is necessary but never sufficient: the session
// must also be idle at the CLI's input prompt with no turn pending, past
// the per-agent backoff, and silent for at least MinIdle.
func sessionRecycleDecision(in sessionRecycleInput, now time.Time) string {
	p := in.Policy.Effective()
	if p.Enabled == nil || !*p.Enabled {
		return ""
	}
	if in.StartedAt.IsZero() || now.Sub(in.StartedAt) < sessionRecycleStartupGrace {
		return ""
	}
	if !in.LastRecycle.IsZero() && now.Sub(in.LastRecycle) < config.SessionRecycleBackoff {
		return ""
	}
	age := now.Sub(in.StartedAt)
	var reason string
	switch {
	case in.RSSMB > p.MaxRSSMB:
		reason = fmt.Sprintf("rss %d MiB over %d MiB budget", in.RSSMB, p.MaxRSSMB)
	case age >= p.MaxAge:
		reason = fmt.Sprintf("session age %s over %s budget", roundDurationForHumans(age), p.MaxAge)
	default:
		return ""
	}
	if in.KickInFlight {
		return ""
	}
	if !paneIdleAtInputPrompt(in.Pane) {
		return ""
	}
	last := in.LastActivity
	if last.IsZero() {
		last = in.StartedAt
	}
	if now.Sub(last) < p.MinIdle {
		return ""
	}
	return reason
}

// paneIdleAtInputPrompt is the "CLI reached its input prompt and is doing
// nothing" test: the CLI is up, shows its prompt, and shows none of the
// working, active-work, consent or blocking-prompt signatures.
func paneIdleAtInputPrompt(pane string) bool {
	if !paneHasCLIMarker(pane) {
		return false
	}
	if paneShowsAgentWorking(pane) || paneShowsActiveWork(pane) {
		return false
	}
	if paneShowsConsentScreen(pane) || paneHasBlockingPrompt(pane) {
		return false
	}
	return paneShowsInputPrompt(pane)
}

// Seams for tests, like procRoot: the pane-pid lookup shells out to tmux and
// the restart tears down a real session.
var (
	sessionRecyclePanePID = (*Manager).tmuxPanePIDForAgent
	sessionRecycleRestart = (*Manager).restartWithStormBreaker
)

// sessionRecycleCandidate is one running agent the health tick saw with a
// live pane, carried across the RLock release so RSS sampling (a tmux exec
// and a /proc scan) happens unlocked.
type sessionRecycleCandidate struct {
	name string
	pane string
}

// sampleAndRecycleSessions is the health-tick half of the policy: sample
// RSS for every candidate, publish it on the AgentProcess for the
// dashboard, and restart the ones the policy says to. Restarts go through
// the storm breaker like every other automatic restart.
func (m *Manager) sampleAndRecycleSessions(ctx context.Context, candidates []sessionRecycleCandidate) {
	now := time.Now()
	type recycle struct {
		name, reason string
		rssMB        int
		age          time.Duration
	}
	var toRecycle []recycle
	for _, c := range candidates {
		m.mu.RLock()
		agent := m.agents[c.name]
		m.mu.RUnlock()
		if agent == nil {
			continue
		}
		rssMB := procTreeRSSMB(procRoot, sessionRecyclePanePID(m, agent))

		m.mu.Lock()
		agent, ok := m.agents[c.name]
		if !ok || agent == nil || agent.State != StateRunning || agent.Paused {
			m.mu.Unlock()
			continue
		}
		if rssMB > 0 {
			agent.SessionRSSMB = rssMB
		}
		in := sessionRecycleInput{
			Policy:       agent.Config.SessionRecycle.Effective(),
			RSSMB:        agent.SessionRSSMB,
			LastRecycle:  agent.lastSessionRecycle,
			Pane:         c.pane,
			KickInFlight: m.kickInFlightLocked(agent),
		}
		if agent.StartedAt != nil {
			in.StartedAt = *agent.StartedAt
		}
		agent.paneMu.RLock()
		paneChange := agent.LastPaneChange
		agent.paneMu.RUnlock()
		var lastKick time.Time
		if agent.LastKick != nil {
			lastKick = *agent.LastKick
		}
		in.LastActivity = latestTime(agent.LastTranscriptActivity, paneChange, lastKick)
		reason := sessionRecycleDecision(in, now)
		if reason != "" {
			agent.lastSessionRecycle = now
			agent.SessionRecycles++
			agent.LastError = ""
			toRecycle = append(toRecycle, recycle{name: c.name, reason: reason, rssMB: in.RSSMB, age: now.Sub(in.StartedAt)})
		}
		m.mu.Unlock()
	}
	for _, r := range toRecycle {
		didRestart, err := sessionRecycleRestart(m, ctx, r.name, sessionRecycleReason, r.reason)
		switch {
		case err != nil:
			m.logger.Error("agent session recycle failed", "name", r.name, "reason", r.reason, "error", err)
		case didRestart:
			m.logger.Info("agent session recycled",
				"name", r.name,
				"rss_mb", r.rssMB,
				"age", roundDurationForHumans(r.age),
				"reason", r.reason,
			)
		}
	}
}

// kickInFlightLocked reports whether a kick is anywhere between "decided"
// and "visible in the pane": being typed, dispatched asynchronously, or
// queued for startup. Caller holds m.mu.
func (m *Manager) kickInFlightLocked(agent *AgentProcess) bool {
	if agent.kickDelivering.Load() || agent.startupKickInFlight || agent.pendingStartupKick != "" {
		return true
	}
	if d, ok := m.kickDispatches.get(agent.Name); ok && d.Pending() {
		return true
	}
	return false
}

func latestTime(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}
