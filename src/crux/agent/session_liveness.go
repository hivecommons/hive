package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"time"
)

const (
	// kickSessionStartGrace is how long after a kick a Copilot CLI session may
	// go without writing a start event before it is declared never started.
	kickSessionStartGrace = 300 * time.Second
	// agentTurnSilence is how long an open turn may go without any new event
	// (and no in-flight tool call) before the session is declared hung.
	agentTurnSilence = 600 * time.Second
	// sessionStallRestartCooldown bounds session-liveness restarts per agent.
	sessionStallRestartCooldown = 30 * time.Minute
)

var (
	eventUserMessage = []byte(`"type":"user.message"`)
	eventTurnStart   = []byte(`"type":"assistant.turn_start"`)
)

// copilotSessionStall inspects the newest session dir under home's
// .copilot/session-state that was created at or after kickAt and reports why
// it looks hung, or "" when it is healthy or not yet judgeable. A spinner in
// the pane counts as activity, so the only reliable signal is events.jsonl.
func copilotSessionStall(home string, kickAt, now time.Time) string {
	root := filepath.Join(home, ".copilot", "session-state")
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	var newest string
	var newestMod time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(kickAt) {
			continue
		}
		if newest == "" || info.ModTime().After(newestMod) {
			newest, newestMod = e.Name(), info.ModTime()
		}
	}
	if newest == "" {
		return ""
	}
	events := filepath.Join(root, newest, "events.jsonl")
	info, err := os.Stat(events)
	if err != nil {
		if now.Sub(kickAt) > kickSessionStartGrace {
			return "session never started"
		}
		return ""
	}
	data, err := os.ReadFile(events)
	if err != nil {
		return ""
	}
	if !bytes.Contains(data, eventUserMessage) && !bytes.Contains(data, eventTurnStart) {
		if now.Sub(kickAt) > kickSessionStartGrace {
			return "session never started"
		}
		return ""
	}
	if now.Sub(info.ModTime()) > agentTurnSilence && lastEventIsTurnStart(data) {
		return "turn open with no events"
	}
	return ""
}

// lastEventIsTurnStart reports whether the final JSONL line is an
// assistant.turn_start, i.e. no message or tool call followed it.
func lastEventIsTurnStart(data []byte) bool {
	data = bytes.TrimRight(data, "\r\n")
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		data = data[i+1:]
	}
	return bytes.Contains(data, eventTurnStart)
}

// sessionStalled reports whether a running Copilot CLI agent, shown busy by
// its pane, has a hung session per copilotSessionStall.
func (m *Manager) sessionStalled(agent *AgentProcess, pane string, now time.Time) string {
	if effectiveBackend(agent) != "copilot" || agent.LastKick == nil {
		return ""
	}
	if !paneShowsActiveWork(pane) {
		return ""
	}
	if now.Sub(agent.lastSessionStallRestart) < sessionStallRestartCooldown {
		return ""
	}
	home := AgentHome(agent.Name, agent.UID, "copilot")
	return copilotSessionStall(home, *agent.LastKick, now)
}
