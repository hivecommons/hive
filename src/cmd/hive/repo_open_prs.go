package main

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// The per-repo open-PRs gate (project.repo_policies[].open_prs), hive side.
//
// Every path that opens a PR for an agent asks openPRsRefusal first: the
// GitHub pr-request relay and the sandbox create call (through
// github.Client.SetPROpenModeGate), the forge relay, the in-process sandbox
// launch (crux.ProjectContext.RepoPRRefusal) and the sidecar callback
// (cruxhost.Deps.PROpenGate).
//
// For workflow_only the question is whether the PR comes from a
// workflow-dispatched hand-off. That is answered from the hive's own record:
// the durable workflow dispatch log, which the hive writes when it delivers
// a hand-off kick to an agent. The PR's title, body and branch name are
// agent text and are never read for this.

// workflowOnlyPRWindow is how long after a hand-off was delivered its agent
// may open the hand-off's PR in a workflow_only repo. It is the hand-off's
// issue-claim lifetime: past it the hive frees the issue for other work, so
// the hand-off no longer vouches for a PR either.
const workflowOnlyPRWindow = workflowDispatchClaimTTL

// workflowHandoffCoversPR reports whether entries hold a live workflow
// hand-off to agent in repo that covers a PR naming issues: delivered within
// workflowOnlyPRWindow of now, not marked not-started (that hand-off went to
// another agent), and, when the PR names issues, handed off for one of them.
// A PR that names no issue (the sandbox path, or a pull-request item's
// hand-off) is covered by any live hand-off to the agent in that repo.
func workflowHandoffCoversPR(entries []scheduler.WorkflowDispatchEntry, org, agent, repo string, issues []int, now time.Time) bool {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return false
	}
	want := strings.ToLower(config.QualifyRepo(org, strings.TrimSpace(repo)))
	for _, e := range entries {
		if e.Agent != agent || e.Status == scheduler.WorkflowHandoffNotStarted || e.At.IsZero() {
			continue
		}
		if age := now.Sub(e.At); age < 0 || age > workflowOnlyPRWindow {
			continue
		}
		i := strings.LastIndex(e.Ref, "#")
		if i <= 0 {
			continue
		}
		if strings.ToLower(config.QualifyRepo(org, e.Ref[:i])) != want {
			continue
		}
		if len(issues) == 0 {
			return true
		}
		n, err := strconv.Atoi(e.Ref[i+1:])
		if err != nil {
			continue
		}
		for _, named := range issues {
			if named == n {
				return true
			}
		}
	}
	return false
}

// openPRsRefusal is the gate itself: the repo's effective mode and, when the
// PR may not open, why. entries reads the hand-off record; it is only called
// for a workflow_only repo.
func openPRsRefusal(cfg *config.Config, entries func() []scheduler.WorkflowDispatchEntry, agent, repo string, issues []int, now time.Time) (config.RepoOpenPRsMode, string) {
	mode := cfg.RepoOpenPRsMode(repo)
	workflow := false
	if mode == config.RepoOpenPRsWorkflowOnly && entries != nil && cfg != nil {
		workflow = workflowHandoffCoversPR(entries(), cfg.Project.Org, agent, repo, issues, now)
	}
	return mode, cfg.RepoOpenPRsRefusal(repo, workflow)
}

// hiveOpenPRsGate binds openPRsRefusal to the live config and the process's
// workflow dispatch log.
func hiveOpenPRsGate(cfg func() *config.Config, logger *slog.Logger) func(agent, repo string, issues []int) (string, string) {
	return func(agent, repo string, issues []int) (string, string) {
		c := cfg()
		entries := func() []scheduler.WorkflowDispatchEntry {
			if c == nil {
				// Opening the log without a config would bind it to no
				// file for the life of the process.
				return nil
			}
			return workflowDispatchRT.dispatchLog(c, logger).Entries()
		}
		mode, reason := openPRsRefusal(c, entries, agent, repo, issues, time.Now())
		return string(mode), reason
	}
}

// sandboxRepoPRRefusal is crux.ProjectContext.RepoPRRefusal for the
// in-process runtime: the same gate, for a sandboxed kick that names no
// issue.
func (b *boot) sandboxRepoPRRefusal(agent, repo string) string {
	_, reason := hiveOpenPRsGate(func() *config.Config { return b.cfg }, b.logger)(agent, repo, nil)
	return reason
}
