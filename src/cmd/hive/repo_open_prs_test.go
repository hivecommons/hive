package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
)

func openPRsTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"docs", "api", "web"}
	cfg.Project.RepoPolicies = []config.RepoPolicy{
		{Repo: "docs", OpenPRs: string(config.RepoOpenPRsWorkflowOnly)},
		{Repo: "api", OpenPRs: string(config.RepoOpenPRsOff)},
	}
	return cfg
}

// The relay gate: workflow_only allows only the PR for the issue the hive
// handed to this agent, while the hand-off is not settled; off refuses
// everything; other repos are unaffected.
func TestOpenPRsRefusal(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	cfg := openPRsTestConfig()
	// The hive delivered a hand-off of acme/docs#7 to drafter an hour ago.
	st := handoffState{log: []scheduler.WorkflowDispatchEntry{
		{Key: "k1", Ref: "acme/docs#7", Profile: "analysis", Agent: "drafter", At: now.Add(-time.Hour), Status: scheduler.WorkflowHandoffPending},
	}}
	state := func() handoffState { return st }

	cases := []struct {
		name    string
		agent   string
		repo    string
		issues  []int
		refused bool
		mode    config.RepoOpenPRsMode
	}{
		{"workflow_only refuses the cadence agent's PR", "quality", "docs", []int{12}, true, config.RepoOpenPRsWorkflowOnly},
		{"workflow_only refuses a cadence PR naming no issue", "quality", "acme/docs", nil, true, config.RepoOpenPRsWorkflowOnly},
		{"workflow_only allows the hand-off's PR", "drafter", "docs", []int{7}, false, config.RepoOpenPRsWorkflowOnly},
		{"workflow_only refuses the hand-off agent's PR naming no issue", "drafter", "docs", nil, true, config.RepoOpenPRsWorkflowOnly},
		{"workflow_only refuses the hand-off agent's PR for another issue", "drafter", "docs", []int{8}, true, config.RepoOpenPRsWorkflowOnly},
		{"off refuses everything, even the hand-off agent", "drafter", "api", []int{7}, true, config.RepoOpenPRsOff},
		{"off refuses a cadence PR", "quality", "api", nil, true, config.RepoOpenPRsOff},
		{"another repo is unaffected", "quality", "web", nil, false, config.RepoOpenPRsOn},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mode, reason := openPRsRefusal(cfg, state, c.agent, c.repo, c.issues, "relay", now)
			if mode != c.mode {
				t.Errorf("mode = %q, want %q", mode, c.mode)
			}
			if (reason != "") != c.refused {
				t.Errorf("reason = %q, refused want %v", reason, c.refused)
			}
		})
	}
}

// Provenance is the hive's record only: a settled, stale, foreign-repo or
// other-agent record does not vouch for a PR; a planned kick does, even
// before the delivery is logged.
func TestRelayPRCovered(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	base := scheduler.WorkflowDispatchEntry{Ref: "acme/docs#7", Agent: "drafter", At: now.Add(-time.Hour), Status: scheduler.WorkflowHandoffPending}
	mut := func(f func(*scheduler.WorkflowDispatchEntry)) handoffState {
		e := base
		f(&e)
		return handoffState{log: []scheduler.WorkflowDispatchEntry{e}}
	}
	planned := []plannedHandoffKick{{agent: "drafter", ref: "acme/docs#7", at: now.Add(-time.Minute)}}
	cases := map[string]struct {
		st   handoffState
		want bool
	}{
		"live pending":            {mut(func(*scheduler.WorkflowDispatchEntry) {}), true},
		"legacy no status":        {mut(func(e *scheduler.WorkflowDispatchEntry) { e.Status = "" }), true},
		"bare ref":                {mut(func(e *scheduler.WorkflowDispatchEntry) { e.Ref = "docs#7" }), true},
		"started (settled)":       {mut(func(e *scheduler.WorkflowDispatchEntry) { e.Status = scheduler.WorkflowHandoffStarted }), false},
		"not started":             {mut(func(e *scheduler.WorkflowDispatchEntry) { e.Status = scheduler.WorkflowHandoffNotStarted }), false},
		"past the window":         {mut(func(e *scheduler.WorkflowDispatchEntry) { e.At = now.Add(-workflowOnlyPRWindow - time.Minute) }), false},
		"no delivery time":        {mut(func(e *scheduler.WorkflowDispatchEntry) { e.At = time.Time{} }), false},
		"other agent":             {mut(func(e *scheduler.WorkflowDispatchEntry) { e.Agent = "quality" }), false},
		"other repo":              {mut(func(e *scheduler.WorkflowDispatchEntry) { e.Ref = "acme/web#7" }), false},
		"malformed ref":           {mut(func(e *scheduler.WorkflowDispatchEntry) { e.Ref = "acme/docs" }), false},
		"empty":                   {handoffState{}, false},
		"planned, not yet logged": {handoffState{planned: planned}, true},
		"planned but settled": {handoffState{planned: planned, log: []scheduler.WorkflowDispatchEntry{
			{Ref: "acme/docs#7", Agent: "drafter", At: now.Add(-time.Minute), Status: scheduler.WorkflowHandoffStarted},
		}}, false},
	}
	for name, c := range cases {
		if got := relayPRCovered(c.st, "acme", "drafter", "docs", []int{7}, now); got != c.want {
			t.Errorf("%s: covers = %v, want %v", name, got, c.want)
		}
	}
	if relayPRCovered(handoffState{planned: planned}, "acme", "drafter", "docs", nil, now) {
		t.Error("a request naming no issue must never be covered")
	}
	if relayPRCovered(handoffState{planned: planned}, "acme", "", "docs", []int{7}, now) {
		t.Error("an unnamed agent must never be covered")
	}
}

// The sandbox launch gate matches the exact kick text the hive planned,
// recorded before the send, so the hand-off kick may push and the agent's
// cadence kicks in the same repo may not.
func TestSandboxKickRefusal(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	cfg := openPRsTestConfig()
	reg := &handoffKickRegistry{kicks: map[string]plannedHandoffKick{}}
	handoff := "## Workflow hand-off: analysis for acme/docs#7\n..."
	reg.record("drafter", "acme/docs#7", handoff, now.Add(-time.Minute))

	if _, reason := sandboxKickRefusal(cfg, reg, "drafter", "acme/docs", handoff, now); reason != "" {
		t.Errorf("the planned hand-off kick must push, got %q", reason)
	}
	if _, reason := sandboxKickRefusal(cfg, reg, "drafter", "acme/docs", "harden the tests", now); reason == "" {
		t.Error("the hand-off agent's cadence kick must not push in a workflow_only repo")
	}
	if _, reason := sandboxKickRefusal(cfg, reg, "quality", "acme/docs", handoff, now); reason == "" {
		t.Error("another agent sending the same text is not the hand-off")
	}
	if _, reason := sandboxKickRefusal(cfg, reg, "drafter", "acme/web", "anything", now); reason != "" {
		t.Errorf("an on repo is unaffected, got %q", reason)
	}
	if _, reason := sandboxKickRefusal(cfg, reg, "drafter", "acme/api", handoff, now); reason == "" {
		t.Error("off refuses even the hand-off kick")
	}
	if _, reason := sandboxKickRefusal(cfg, reg, "drafter", "acme/docs", handoff, now.Add(workflowOnlyPRWindow+time.Minute)); reason == "" {
		t.Error("a planned kick past the window no longer vouches")
	}
	// The create-call backstop: covered only by a planned hand-off kick in
	// that repo.
	st := handoffState{planned: reg.list(now)}
	if !sandboxBackstopCovered(st, "acme", "drafter", "docs") || sandboxBackstopCovered(st, "acme", "quality", "docs") || sandboxBackstopCovered(st, "acme", "drafter", "web") {
		t.Error("sandbox backstop coverage wrong")
	}
	if _, reason := openPRsRefusal(cfg, func() handoffState { return st }, "drafter", "docs", nil, github.SandboxPRVia, now); reason != "" {
		t.Errorf("backstop must allow the hand-off agent's sandbox PR, got %q", reason)
	}
}

// project.open_prs: false wins over a workflow hand-off.
func TestOpenPRsRefusalHiveWideOff(t *testing.T) {
	now := time.Now()
	cfg := openPRsTestConfig()
	off := false
	cfg.Project.OpenPRs = &off
	st := handoffState{log: []scheduler.WorkflowDispatchEntry{{Ref: "acme/docs#7", Agent: "drafter", At: now.Add(-time.Minute)}}}
	mode, reason := openPRsRefusal(cfg, func() handoffState { return st }, "drafter", "docs", []int{7}, "relay", now)
	if mode != config.RepoOpenPRsOff || !strings.Contains(reason, "project.open_prs") {
		t.Fatalf("hive-wide off: mode=%q reason=%q", mode, reason)
	}
}

// The forge relay asks the same gate: an off repo opens no change request,
// the request is rejected with the reason, and the refusal is audited.
func TestForgeRelayOpenPRsOff(t *testing.T) {
	h := newRelayHarness(t)
	var audited []string
	fn := func(agent, repo, mode, via, reason string) {
		audited = append(audited, openPRsRefusalDetail(repo, mode, via, reason))
	}
	openPRsRefusalAudit.Store(&fn)
	t.Cleanup(func() { openPRsRefusalAudit.Store(nil) })
	h.cfg.Project.RepoPolicies = append(h.cfg.Project.RepoPolicies, config.RepoPolicy{Repo: "widgets", OpenPRs: string(config.RepoOpenPRsOff)})
	path := writeRelayRequest(t, h.relay.prDir, "a.json", goodPR())
	h.relay.scanOnce(context.Background())
	if len(h.writer.specs) != 0 {
		t.Fatalf("change request opened in an off repo: %v", h.writer.specs)
	}
	assertExists(t, path+".rejected", true)
	var res github.PRResponse
	readRelayResult(t, path, &res)
	if res.OK || !strings.Contains(res.Error, github.GateOpenPRs) || !strings.Contains(res.Error, "off") {
		t.Fatalf("result = %+v", res)
	}
	if len(audited) != 1 || !strings.Contains(audited[0], "gate=open-prs") || !strings.Contains(audited[0], "via=forge-relay") {
		t.Fatalf("refusal audit = %v", audited)
	}
}
