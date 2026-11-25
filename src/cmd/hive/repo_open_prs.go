package main

import (
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// The per-repo open-PRs gate (project.repo_policies[].open_prs), hive side.
//
// Every path that opens a PR for an agent asks this gate first: the GitHub
// pr-request relay and the sandbox create call (through
// github.Client.SetPROpenModeGate), the forge relay, the in-process sandbox
// launch (crux.ProjectContext.RepoPRRefusal) and the sidecar callback
// (cruxhost.Deps.RepoPRRefusal).
//
// For workflow_only the question is whether the PR comes from a
// workflow-dispatched hand-off. It is answered only from what the hive
// itself recorded, never from the PR's title, body or branch:
//
//   - workflowHandoffKicks: every hand-off kick the hive planned, keyed by
//     the agent and the exact kick text the hive wrote, recorded BEFORE the
//     kick is sent (the sandbox launch runs inside the send).
//   - the durable workflow dispatch log, written once the kick landed, with
//     the hand-off's status.
//
// A sandboxed kick may push only when its kick text is a planned hand-off
// kick. A relay request must name (hive-open-pr --issues) the issue the hive
// handed to that agent, and only until the hand-off is settled (started or
// not started).

// workflowOnlyPRWindow is how long after a hand-off was planned its agent
// may open the hand-off's PR in a workflow_only repo. It is the hand-off's
// issue-claim lifetime: past it the hive frees the issue for other work, so
// the hand-off no longer vouches for a PR either.
const workflowOnlyPRWindow = workflowDispatchClaimTTL

// plannedHandoffKick is one hand-off kick the hive planned.
type plannedHandoffKick struct {
	agent string
	ref   string
	at    time.Time
}

// handoffKickRegistry holds the planned hand-off kicks of this process.
type handoffKickRegistry struct {
	mu    sync.Mutex
	kicks map[string]plannedHandoffKick
}

var workflowHandoffKicks = &handoffKickRegistry{kicks: map[string]plannedHandoffKick{}}

func handoffKickKey(agent, message string) string { return agent + "\x00" + message }

// record notes a planned hand-off kick of ref to agent with the exact text
// message, and drops entries past the window.
func (r *handoffKickRegistry) record(agent, ref, message string, now time.Time) {
	if r == nil || strings.TrimSpace(agent) == "" || message == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.kicks {
		if now.Sub(v.at) > workflowOnlyPRWindow {
			delete(r.kicks, k)
		}
	}
	r.kicks[handoffKickKey(agent, message)] = plannedHandoffKick{agent: agent, ref: ref, at: now}
}

// byMessage returns the live planned kick with exactly this agent and text.
func (r *handoffKickRegistry) byMessage(agent, message string, now time.Time) (plannedHandoffKick, bool) {
	if r == nil {
		return plannedHandoffKick{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k, ok := r.kicks[handoffKickKey(agent, message)]
	if !ok || now.Sub(k.at) > workflowOnlyPRWindow || now.Before(k.at) {
		return plannedHandoffKick{}, false
	}
	return k, true
}

// list returns the live planned kicks.
func (r *handoffKickRegistry) list(now time.Time) []plannedHandoffKick {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]plannedHandoffKick, 0, len(r.kicks))
	for _, k := range r.kicks {
		if now.Sub(k.at) <= workflowOnlyPRWindow && !now.Before(k.at) {
			out = append(out, k)
		}
	}
	return out
}

// splitHandoffRef parses "owner/repo#N" into the qualified, lower-cased repo
// and N.
func splitHandoffRef(org, ref string) (string, int, bool) {
	i := strings.LastIndex(ref, "#")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(ref[i+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return strings.ToLower(config.QualifyRepo(org, ref[:i])), n, true
}

func qualifiedLower(org, repo string) string {
	return strings.ToLower(config.QualifyRepo(org, strings.TrimSpace(repo)))
}

// handoffState is what the gate knows about the hand-offs it may consult.
type handoffState struct {
	log     []scheduler.WorkflowDispatchEntry
	planned []plannedHandoffKick
}

// relayPRCovered reports whether a relay PR request from agent in repo that
// names issues comes from a live hand-off: one of the issues is an item the
// hive handed to this agent (planned, or delivered and still pending), and
// no settled record (started or not started) of that item for this agent
// exists. A request that names no issue is never covered.
func relayPRCovered(st handoffState, org, agent, repo string, issues []int, now time.Time) bool {
	agent = strings.TrimSpace(agent)
	if agent == "" || len(issues) == 0 {
		return false
	}
	want := qualifiedLower(org, repo)
	named := make(map[int]bool, len(issues))
	for _, n := range issues {
		named[n] = true
	}
	live := map[int]bool{}
	settled := map[int]bool{}
	for _, e := range st.log {
		if e.Agent != agent {
			continue
		}
		r, n, ok := splitHandoffRef(org, e.Ref)
		if !ok || r != want || !named[n] {
			continue
		}
		if e.Settled() {
			settled[n] = true
			continue
		}
		if !e.At.IsZero() && !now.Before(e.At) && now.Sub(e.At) <= workflowOnlyPRWindow {
			live[n] = true
		}
	}
	for _, k := range st.planned {
		if k.agent != agent {
			continue
		}
		if r, n, ok := splitHandoffRef(org, k.ref); ok && r == want && named[n] {
			live[n] = true
		}
	}
	for n := range live {
		if !settled[n] {
			return true
		}
	}
	return false
}

// sandboxBackstopCovered is the rule for the sandbox create call itself
// (github.Client.CreateAgentPR), which sees no kick text: agent holds a live
// planned hand-off kick in repo. The launch gate already refused push to
// every kick that is not a planned hand-off, so only hand-off kicks reach
// this call.
func sandboxBackstopCovered(st handoffState, org, agent, repo string) bool {
	want := qualifiedLower(org, repo)
	for _, k := range st.planned {
		if k.agent != agent {
			continue
		}
		if r, _, ok := splitHandoffRef(org, k.ref); ok && r == want {
			return true
		}
	}
	return false
}

// openPRsRefusal is the gate for a PR request (relay, forge relay, sandbox
// backstop): the repo's effective mode and, when the PR may not open, why.
// state is read only for a workflow_only repo.
func openPRsRefusal(cfg *config.Config, state func() handoffState, agent, repo string, issues []int, via string, now time.Time) (config.RepoOpenPRsMode, string) {
	mode := cfg.RepoOpenPRsMode(repo)
	workflow := false
	if mode == config.RepoOpenPRsWorkflowOnly && state != nil && cfg != nil {
		st := state()
		if via == github.SandboxPRVia {
			workflow = sandboxBackstopCovered(st, cfg.Project.Org, agent, repo)
		} else {
			workflow = relayPRCovered(st, cfg.Project.Org, agent, repo, issues, now)
		}
	}
	return mode, cfg.RepoOpenPRsRefusal(repo, workflow)
}

// sandboxKickRefusal is the gate for a sandboxed kick's launch: in a
// workflow_only repo, only a kick whose exact text is a hand-off kick the
// hive planned for this agent and repo may push.
func sandboxKickRefusal(cfg *config.Config, reg *handoffKickRegistry, agent, repo, message string, now time.Time) (config.RepoOpenPRsMode, string) {
	mode := cfg.RepoOpenPRsMode(repo)
	workflow := false
	if mode == config.RepoOpenPRsWorkflowOnly && cfg != nil {
		if k, ok := reg.byMessage(agent, message, now); ok {
			if r, _, ok := splitHandoffRef(cfg.Project.Org, k.ref); ok && r == qualifiedLower(cfg.Project.Org, repo) {
				workflow = true
			}
		}
	}
	return mode, cfg.RepoOpenPRsRefusal(repo, workflow)
}

// hiveOpenPRsGate binds openPRsRefusal to the live config, the planned
// hand-off kicks and the process's workflow dispatch log.
func hiveOpenPRsGate(cfg func() *config.Config, logger *slog.Logger) func(agent, repo string, issues []int, via string) (string, string) {
	return func(agent, repo string, issues []int, via string) (string, string) {
		c := cfg()
		now := time.Now()
		state := func() handoffState {
			st := handoffState{planned: workflowHandoffKicks.list(now)}
			if c != nil && via != github.SandboxPRVia {
				// Opening the log without a config would bind it to no file
				// for the life of the process.
				st.log = workflowDispatchRT.dispatchLog(c, logger).Entries()
			}
			return st
		}
		mode, reason := openPRsRefusal(c, state, agent, repo, issues, via, now)
		return string(mode), reason
	}
}

// openPRsRefusalAudit records a refusal on a path that has no GitHub client
// audit sink of its own (the forge relay, the sidecar callback). It is set
// once the dashboard's audit log exists; nil records nothing.
var openPRsRefusalAudit atomic.Pointer[func(agent, repo, mode, via, reason string)]

func auditOpenPRsRefusal(agent, repo, mode, via, reason string) {
	if fn := openPRsRefusalAudit.Load(); fn != nil && *fn != nil {
		(*fn)(agent, repo, mode, via, reason)
	}
}

// openPRsRefusalDetail is the agent_write_refused detail every path uses.
func openPRsRefusalDetail(repo, mode, via, reason string) string {
	return "repo=" + repo + ", op=" + github.WriteOpOpenPR + ", gate=" + github.GateOpenPRs + ", mode=" + mode + ", via=" + via + ", outcome=refused, reason=" + reason
}

// sandboxRepoPRRefusal is crux.ProjectContext.RepoPRRefusal for the
// in-process runtime. It only reads memory: it runs under the agent
// manager's lock.
func (b *boot) sandboxRepoPRRefusal(agent, repo, message string) string {
	_, reason := sandboxKickRefusal(b.cfg, workflowHandoffKicks, agent, repo, message, time.Now())
	return reason
}
