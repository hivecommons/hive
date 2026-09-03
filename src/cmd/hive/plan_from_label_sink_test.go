package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/planning"
)

// sinkTestLogger returns a logger writing to buf so QueuedPlan's two log-only
// branches (paused vs unavailable) can be told apart by their messages.
func sinkTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

func sinkTestEpic() *beads.Bead {
	return &beads.Bead{ID: "hive-epic-1", ExternalRef: "kubestellar/hive#42"}
}

// KickedPlan must record the kick against the ARCHITECT in the governor (that
// timestamp is what stops the eval loop from re-kicking every cycle) and leave
// an audit trail on the dashboard naming the epic and its source issue.
func TestLabelPlanSinkKickedPlanRecordsKickAndAudit(t *testing.T) {
	var buf bytes.Buffer
	logger := sinkTestLogger(&buf)
	gov := governor.New(config.GovernorConfig{}, map[string]config.AgentConfig{}, logger)
	srv := dashboard.NewServer(0, logger)

	sink := labelPlanSink{gov: gov, dashSrv: srv, logger: logger}
	sink.KickedPlan(sinkTestEpic())

	hist := gov.KickHistory()
	if len(hist) != 1 {
		t.Fatalf("KickHistory has %d records, want 1", len(hist))
	}
	if hist[0].Agent != planning.ArchitectAgentName {
		t.Errorf("kick recorded for %q, want %q", hist[0].Agent, planning.ArchitectAgentName)
	}

	found := false
	for _, e := range srv.GetAudit().Recent(0) {
		if e.Action == "plan_from_label" &&
			strings.Contains(e.Detail, "hive-epic-1") &&
			strings.Contains(e.Detail, "kubestellar/hive#42") {
			found = true
		}
	}
	if !found {
		t.Errorf("no plan_from_label audit entry naming epic and ref; audit = %+v", srv.GetAudit().Recent(0))
	}
}

// A nil dashboard is a legitimate wiring state (tests, headless boots): the
// kick must still be recorded and the sink must not panic on the nil guard.
func TestLabelPlanSinkKickedPlanNilDashboard(t *testing.T) {
	var buf bytes.Buffer
	logger := sinkTestLogger(&buf)
	gov := governor.New(config.GovernorConfig{}, map[string]config.AgentConfig{}, logger)

	sink := labelPlanSink{gov: gov, dashSrv: nil, logger: logger}
	sink.KickedPlan(sinkTestEpic())

	if got := len(gov.KickHistory()); got != 1 {
		t.Fatalf("KickHistory has %d records, want 1", got)
	}
}

// QueuedPlan's paused branch is the deliberate-operator-pause path: it must
// log at the calm "architect paused" level and, critically, never touch the
// governor (a kick here would defeat the pause) — enforced by the nil gov.
func TestLabelPlanSinkQueuedPlanPaused(t *testing.T) {
	var buf bytes.Buffer
	sink := labelPlanSink{gov: nil, dashSrv: nil, logger: sinkTestLogger(&buf)}

	sink.QueuedPlan(sinkTestEpic(), true)

	out := buf.String()
	if !strings.Contains(out, "architect paused") {
		t.Errorf("paused branch log = %q, want mention of architect paused", out)
	}
	if strings.Contains(out, "architect unavailable") {
		t.Errorf("paused branch logged the unavailable message: %q", out)
	}
}

// The not-paused branch means the architect is simply absent — that is a
// warning-worthy state and must say "unavailable", not "paused".
func TestLabelPlanSinkQueuedPlanUnavailable(t *testing.T) {
	var buf bytes.Buffer
	sink := labelPlanSink{gov: nil, dashSrv: nil, logger: sinkTestLogger(&buf)}

	sink.QueuedPlan(sinkTestEpic(), false)

	out := buf.String()
	if !strings.Contains(out, "architect unavailable") {
		t.Errorf("unavailable branch log = %q, want mention of architect unavailable", out)
	}
	if strings.Contains(out, "architect paused") {
		t.Errorf("unavailable branch logged the paused message: %q", out)
	}
}

// With no architect store configured, planFromLabeledIssues falls back to any
// available store rather than dropping the plan on the floor — the epic must
// land in the fallback store.
func TestPlanFromLabeledIssuesFallbackStore(t *testing.T) {
	fallback := newPlanTestStore(t)
	stores := map[string]*beads.Store{"scanner": fallback}
	mgr := agent.NewManager(map[string]config.AgentConfig{}, restoreTestLogger(), agent.ProjectContext{})
	actionable := &github.ActionableResult{Issues: github.IssueResult{Items: []github.Issue{
		planLabeledIssue(7, "plan"),
	}}}

	planFromLabeledIssues(actionable, stores, mgr, nil, nil, restoreTestLogger(),
		planning.PlanningMinACMMLevel)

	if got := len(fallback.List(beads.ListFilter{})); got != 1 {
		t.Errorf("fallback store has %d beads, want 1 minted epic", got)
	}
}

// A stores map whose only entry is a nil *beads.Store must be treated as "no
// store": planFromLabeledIssues returns without panicking.
func TestPlanFromLabeledIssuesNilStoreValue(t *testing.T) {
	stores := map[string]*beads.Store{"scanner": nil}
	mgr := agent.NewManager(map[string]config.AgentConfig{}, restoreTestLogger(), agent.ProjectContext{})
	actionable := &github.ActionableResult{Issues: github.IssueResult{Items: []github.Issue{
		planLabeledIssue(9, "plan"),
	}}}

	planFromLabeledIssues(actionable, stores, mgr, nil, nil, restoreTestLogger(),
		planning.PlanningMinACMMLevel)
}
