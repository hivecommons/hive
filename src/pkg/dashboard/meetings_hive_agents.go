package dashboard

import (
	"fmt"
	"sort"
	"strings"
)

// Agents in the meeting attendee's hive status brief ("Betty, which agents
// are running?"). The counts are the dashboard's own, read from the same
// status.Agents list GET /api/status serves and classified by the same
// predicate the sidebar agent dots use (static-src/js/sidebar.js), so what
// she says is what the Agents section shows.
//
// How replicas are counted: exactly as the dashboard counts them. Every
// entry of status.Agents is one agent card, and each replica (reviewer-2,
// reviewer-3) is its own card, so each replica counts as one agent and is
// named by its own name. Agents the dashboard hides (status.HiddenAgents)
// are not counted. The total is len(status.Agents), the number in the
// Agents section summary ("17 agents").
//
// In the scored live test of 2026-11-17 she said "two are paused" against
// one paused agent, and named nobody: the brief counted the raw paused
// flag, which is also set on an on-demand agent the dashboard shows as "on
// demand", and it listed a busy agent as working even when it was paused.

// hiveAgentClass is the one dashboard dot class an agent falls in.
type hiveAgentClass string

const (
	hiveAgentWorking  hiveAgentClass = "working"
	hiveAgentIdle     hiveAgentClass = "idle"
	hiveAgentPaused   hiveAgentClass = "paused"
	hiveAgentOff      hiveAgentClass = "off"
	hiveAgentDown     hiveAgentClass = "down"
	hiveAgentStarting hiveAgentClass = "starting"
	hiveAgentOnDemand hiveAgentClass = "on demand"

	// hiveAgentBusyWorking is FrontendAgent.Busy while a turn is running.
	hiveAgentBusyWorking = "working"
	// hiveAgentStateRunning is FrontendAgent.State for a live process.
	hiveAgentStateRunning = "running"
	// hiveAgentFirstReplica is the ReplicaIndex of a lane's base agent;
	// higher indexes are its replicas (the card's replica badge).
	hiveAgentFirstReplica = 1
)

// hiveAgentClassOf mirrors the sidebar dot (sidebar.js): starting, down,
// on-demand idle, paused, off by the governor's schedule, working, idle.
// A paused agent is never working, whatever its busy flag says.
func hiveAgentClassOf(a FrontendAgent) hiveAgentClass {
	paused := a.Paused && !a.OnDemand
	off := (a.OffByCadence || a.UnscheduledInMode) && !paused && !a.OnDemand
	starting := a.Starting && a.State != hiveAgentStateRunning && !paused && !off
	sessionDown := a.State != hiveAgentStateRunning
	if a.Sandboxed && a.SandboxRun != nil && a.SandboxRun.Health != "" {
		sessionDown = a.SandboxRun.Health == SandboxHealthFailed || a.SandboxRun.Health == SandboxHealthNodeUnhealthy || a.SandboxRun.Health == SandboxHealthOverdue
	}
	down := sessionDown && !paused && !off && !a.OnDemand && !starting
	working := a.Busy == hiveAgentBusyWorking && !paused && !off && !down
	switch {
	case starting:
		return hiveAgentStarting
	case down:
		return hiveAgentDown
	case a.OnDemand && !working:
		return hiveAgentOnDemand
	case paused:
		return hiveAgentPaused
	case off:
		return hiveAgentOff
	case working:
		return hiveAgentWorking
	}
	return hiveAgentIdle
}

// hiveStatusAgents appends the agent counts and names: the total, one
// count per dashboard class, the agents working right now by name (capped
// at hiveStatusMaxListed, "and N more"), then paused, blocked and recently
// finished agents.
func hiveStatusAgents(b *strings.Builder, agents []FrontendAgent) {
	list := append([]FrontendAgent(nil), agents...)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	counts := map[hiveAgentClass]int{}
	replicas := 0
	var working, paused, blocked, done []string
	for _, a := range list {
		class := hiveAgentClassOf(a)
		counts[class]++
		if a.ReplicaBase != "" && a.ReplicaIndex > hiveAgentFirstReplica {
			replicas++
		}
		c := chatSnapshotAgentFrom(a)
		switch class {
		case hiveAgentWorking:
			w := a.Name
			if c.Doing != "" {
				w += " (" + chatTruncate(c.Doing, hiveStatusTextCap) + ")"
			}
			working = append(working, w)
		case hiveAgentPaused:
			paused = append(paused, a.Name)
		}
		if c.BlockedReason != "" {
			blocked = append(blocked, a.Name)
		}
		if c.LastCompleted != "" {
			done = append(done, a.Name+": "+chatTruncate(c.LastCompleted, hiveStatusTextCap))
		}
	}
	fmt.Fprintf(b, " Agents: %d", len(list))
	if replicas > 0 {
		fmt.Fprintf(b, " (each replica counts as one agent, %d are replicas)", replicas)
	}
	fmt.Fprintf(b, ", %d working, %d paused, %d blocked", counts[hiveAgentWorking], counts[hiveAgentPaused], len(blocked))
	for _, class := range []hiveAgentClass{hiveAgentIdle, hiveAgentOff, hiveAgentDown, hiveAgentStarting, hiveAgentOnDemand} {
		if n := counts[class]; n > 0 {
			fmt.Fprintf(b, ", %d %s", n, class)
		}
	}
	b.WriteString(".")
	if len(working) == 0 {
		b.WriteString(" No agent is working right now.")
	}
	hiveStatusList(b, " Working right now", working)
	hiveStatusList(b, " Paused, not running", paused)
	hiveStatusList(b, " Blocked", blocked)
	hiveStatusList(b, " Recently finished", done)
}
