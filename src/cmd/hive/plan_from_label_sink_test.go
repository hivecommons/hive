package main

import (
	"bytes"
	"log/slog"
	"strconv"
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
	return &beads.Bead{ID: "hive-epic-1", ExternalRef: "hivecommons/hive#42"}
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
			strings.Contains(e.Detail, "hivecommons/hive#42") {
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
		planLabeledIssue(7, "hive-plan"),
	}}}

	planFromLabeledIssues(actionable, stores, mgr, nil, nil, restoreTestLogger(), planLabelTestConfig(),
		planning.PlanningMinACMMLevel)

	if got := len(fallback.List(beads.ListFilter{})); got != 1 {
		t.Errorf("fallback store has %d beads, want 1 minted epic", got)
	}
}

// FailedPlan is the decompose-gave-up audit event: it must record a
// plan_decompose_failed entry naming the epic, its source issue, and the
// attempt cap, and must never touch the governor (enforced by the nil gov —
// a kick after giving up would restart the loop the cap just stopped).
func TestLabelPlanSinkFailedPlanAuditsWithAttempts(t *testing.T) {
	var buf bytes.Buffer
	logger := sinkTestLogger(&buf)
	srv := dashboard.NewServer(0, logger)

	sink := labelPlanSink{gov: nil, dashSrv: srv, logger: logger}
	sink.FailedPlan(sinkTestEpic())

	found := false
	for _, e := range srv.GetAudit().Recent(0) {
		if e.Action == "plan_decompose_failed" &&
			strings.Contains(e.Detail, "hive-epic-1") &&
			strings.Contains(e.Detail, "hivecommons/hive#42") &&
			strings.Contains(e.Detail, "attempts="+strconv.Itoa(planning.DecomposeMaxAttempts)) {
			found = true
		}
	}
	if !found {
		t.Errorf("no plan_decompose_failed audit entry naming epic, ref, and attempt cap; audit = %+v", srv.GetAudit().Recent(0))
	}
	if !strings.Contains(buf.String(), "no plan after max attempts") {
		t.Errorf("FailedPlan log = %q, want the epic-marked-stuck warning", buf.String())
	}
}

// KickedDesign mirrors KickedPlan: the governor must see the architect kick
// (or the eval loop re-kicks the design every cycle) and the audit trail must
// carry the revision number so operators can see which round this is.
func TestLabelPlanSinkKickedDesignRecordsKickAndRevision(t *testing.T) {
	var buf bytes.Buffer
	logger := sinkTestLogger(&buf)
	gov := governor.New(config.GovernorConfig{}, map[string]config.AgentConfig{}, logger)
	srv := dashboard.NewServer(0, logger)

	sink := labelPlanSink{gov: gov, dashSrv: srv, logger: logger}
	sink.KickedDesign(sinkTestEpic(), 3)

	hist := gov.KickHistory()
	if len(hist) != 1 || hist[0].Agent != planning.ArchitectAgentName {
		t.Fatalf("KickHistory = %+v, want one kick for %q", hist, planning.ArchitectAgentName)
	}

	found := false
	for _, e := range srv.GetAudit().Recent(0) {
		if e.Action == "design_kicked" &&
			strings.Contains(e.Detail, "hive-epic-1") &&
			strings.Contains(e.Detail, "revision=3") {
			found = true
		}
	}
	if !found {
		t.Errorf("no design_kicked audit entry naming epic and revision; audit = %+v", srv.GetAudit().Recent(0))
	}
}

// KickedDesign with a nil dashboard (headless boot) must still record the
// governor kick and not panic on the nil guard.
func TestLabelPlanSinkKickedDesignNilDashboard(t *testing.T) {
	var buf bytes.Buffer
	logger := sinkTestLogger(&buf)
	gov := governor.New(config.GovernorConfig{}, map[string]config.AgentConfig{}, logger)

	sink := labelPlanSink{gov: gov, dashSrv: nil, logger: logger}
	sink.KickedDesign(sinkTestEpic(), 1)

	if got := len(gov.KickHistory()); got != 1 {
		t.Fatalf("KickHistory has %d records, want 1", got)
	}
}

// ApprovedDesign is audit-and-log only: design_approved must name the epic and
// ref, and the nil gov enforces that approval never records a kick.
func TestLabelPlanSinkApprovedDesignAudits(t *testing.T) {
	var buf bytes.Buffer
	logger := sinkTestLogger(&buf)
	srv := dashboard.NewServer(0, logger)

	sink := labelPlanSink{gov: nil, dashSrv: srv, logger: logger}
	sink.ApprovedDesign(sinkTestEpic())

	found := false
	for _, e := range srv.GetAudit().Recent(0) {
		if e.Action == "design_approved" &&
			strings.Contains(e.Detail, "hive-epic-1") &&
			strings.Contains(e.Detail, "hivecommons/hive#42") {
			found = true
		}
	}
	if !found {
		t.Errorf("no design_approved audit entry naming epic and ref; audit = %+v", srv.GetAudit().Recent(0))
	}
}

// DesignNeedsHuman is the revision-cap escalation: the audit entry must carry
// the revision count and the log must say a human is needed — this is the only
// signal an operator gets that the design loop has stopped.
func TestLabelPlanSinkDesignNeedsHumanAuditsRevisions(t *testing.T) {
	var buf bytes.Buffer
	logger := sinkTestLogger(&buf)
	srv := dashboard.NewServer(0, logger)

	sink := labelPlanSink{gov: nil, dashSrv: srv, logger: logger}
	sink.DesignNeedsHuman(sinkTestEpic(), 5)

	found := false
	for _, e := range srv.GetAudit().Recent(0) {
		if e.Action == "design_needs_human" &&
			strings.Contains(e.Detail, "hive-epic-1") &&
			strings.Contains(e.Detail, "revisions=5") {
			found = true
		}
	}
	if !found {
		t.Errorf("no design_needs_human audit entry naming epic and revisions; audit = %+v", srv.GetAudit().Recent(0))
	}
	if !strings.Contains(buf.String(), "needs a human") {
		t.Errorf("DesignNeedsHuman log = %q, want the needs-a-human warning", buf.String())
	}
}

// The audit-only methods must all tolerate a nil dashboard without panicking —
// the same wiring state KickedPlan already guards against.
func TestLabelPlanSinkAuditMethodsNilDashboard(t *testing.T) {
	var buf bytes.Buffer
	sink := labelPlanSink{gov: nil, dashSrv: nil, logger: sinkTestLogger(&buf)}

	sink.FailedPlan(sinkTestEpic())
	sink.ApprovedDesign(sinkTestEpic())
	sink.DesignNeedsHuman(sinkTestEpic(), 2)
}

// A stores map whose only entry is a nil *beads.Store must be treated as "no
// store": planFromLabeledIssues returns without panicking.
func TestPlanFromLabeledIssuesNilStoreValue(t *testing.T) {
	stores := map[string]*beads.Store{"scanner": nil}
	mgr := agent.NewManager(map[string]config.AgentConfig{}, restoreTestLogger(), agent.ProjectContext{})
	actionable := &github.ActionableResult{Issues: github.IssueResult{Items: []github.Issue{
		planLabeledIssue(9, "plan"),
	}}}

	planFromLabeledIssues(actionable, stores, mgr, nil, nil, restoreTestLogger(), planLabelTestConfig(),
		planning.PlanningMinACMMLevel)
}
