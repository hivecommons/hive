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

func TestOpenPRsRefusal(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	cfg := openPRsTestConfig()
	// The hive delivered a hand-off of acme/docs#7 to drafter an hour ago.
	log := []scheduler.WorkflowDispatchEntry{
		{Key: "k1", Ref: "acme/docs#7", Profile: "analysis", Agent: "drafter", At: now.Add(-time.Hour), Status: scheduler.WorkflowHandoffPending},
	}
	entries := func() []scheduler.WorkflowDispatchEntry { return log }

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
		{"workflow_only allows the hand-off's sandbox PR (no issue)", "drafter", "acme/docs", nil, false, config.RepoOpenPRsWorkflowOnly},
		{"workflow_only refuses the hand-off agent's PR for another issue", "drafter", "docs", []int{8}, true, config.RepoOpenPRsWorkflowOnly},
		{"off refuses everything, even the hand-off agent", "drafter", "api", []int{7}, true, config.RepoOpenPRsOff},
		{"off refuses a cadence PR", "quality", "api", nil, true, config.RepoOpenPRsOff},
		{"another repo is unaffected", "quality", "web", nil, false, config.RepoOpenPRsOn},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mode, reason := openPRsRefusal(cfg, entries, c.agent, c.repo, c.issues, now)
			if mode != c.mode {
				t.Errorf("mode = %q, want %q", mode, c.mode)
			}
			if (reason != "") != c.refused {
				t.Errorf("reason = %q, refused want %v", reason, c.refused)
			}
		})
	}
}

// Provenance is the hive's hand-off record only: a stale, not-started,
// foreign-repo or other-agent record does not vouch for a PR.
func TestWorkflowHandoffCoversPR(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	base := scheduler.WorkflowDispatchEntry{Ref: "acme/docs#7", Agent: "drafter", At: now.Add(-time.Hour), Status: scheduler.WorkflowHandoffPending}
	mut := func(f func(*scheduler.WorkflowDispatchEntry)) []scheduler.WorkflowDispatchEntry {
		e := base
		f(&e)
		return []scheduler.WorkflowDispatchEntry{e}
	}
	cases := map[string]struct {
		entries []scheduler.WorkflowDispatchEntry
		want    bool
	}{
		"live pending":     {mut(func(*scheduler.WorkflowDispatchEntry) {}), true},
		"started":          {mut(func(e *scheduler.WorkflowDispatchEntry) { e.Status = scheduler.WorkflowHandoffStarted }), true},
		"legacy no status": {mut(func(e *scheduler.WorkflowDispatchEntry) { e.Status = "" }), true},
		"bare ref":         {mut(func(e *scheduler.WorkflowDispatchEntry) { e.Ref = "docs#7" }), true},
		"not started":      {mut(func(e *scheduler.WorkflowDispatchEntry) { e.Status = scheduler.WorkflowHandoffNotStarted }), false},
		"past the window":  {mut(func(e *scheduler.WorkflowDispatchEntry) { e.At = now.Add(-workflowOnlyPRWindow - time.Minute) }), false},
		"no delivery time": {mut(func(e *scheduler.WorkflowDispatchEntry) { e.At = time.Time{} }), false},
		"other agent":      {mut(func(e *scheduler.WorkflowDispatchEntry) { e.Agent = "quality" }), false},
		"other repo":       {mut(func(e *scheduler.WorkflowDispatchEntry) { e.Ref = "acme/web#7" }), false},
		"malformed ref":    {mut(func(e *scheduler.WorkflowDispatchEntry) { e.Ref = "acme/docs" }), false},
		"empty log":        {nil, false},
	}
	for name, c := range cases {
		if got := workflowHandoffCoversPR(c.entries, "acme", "drafter", "docs", []int{7}, now); got != c.want {
			t.Errorf("%s: covers = %v, want %v", name, got, c.want)
		}
	}
	if workflowHandoffCoversPR([]scheduler.WorkflowDispatchEntry{base}, "acme", "", "docs", nil, now) {
		t.Error("an unnamed agent must never be covered")
	}
}

// project.open_prs: false wins over a workflow hand-off.
func TestOpenPRsRefusalHiveWideOff(t *testing.T) {
	now := time.Now()
	cfg := openPRsTestConfig()
	off := false
	cfg.Project.OpenPRs = &off
	log := []scheduler.WorkflowDispatchEntry{{Ref: "acme/docs#7", Agent: "drafter", At: now.Add(-time.Minute)}}
	mode, reason := openPRsRefusal(cfg, func() []scheduler.WorkflowDispatchEntry { return log }, "drafter", "docs", []int{7}, now)
	if mode != config.RepoOpenPRsOff || !strings.Contains(reason, "project.open_prs") {
		t.Fatalf("hive-wide off: mode=%q reason=%q", mode, reason)
	}
}

// The forge relay asks the same gate: an off repo opens no change request
// and the request is rejected with the reason; clearing the mode opens it.
func TestForgeRelayOpenPRsOff(t *testing.T) {
	h := newRelayHarness(t)
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
}
