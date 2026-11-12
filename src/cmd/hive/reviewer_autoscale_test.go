package main

import (
	"bytes"
	"context"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/review"
)

var radAutoscaleT0 = time.Date(2026, 11, 12, 17, 0, 0, 0, time.UTC)

// radAutoscaleConfig has two agents the review swarm dispatches to
// (adjudicator and reviewer, both listed) and one it does not (scanner), with
// autoscale on for adjudicator. scale_down_cycles is 2 so two idle cycles
// are enough to start draining a replica.
func radAutoscaleConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{
		Agents: map[string]config.AgentConfig{
			"adjudicator": {ID: "adjudicator", Enabled: true, Role: "reviewer"},
			"reviewer":    {ID: "reviewer", Enabled: true, Role: "reviewer"},
			"scanner":     {ID: "scanner", Enabled: true},
		},
	}}
	cfg.Review.ReviewerAgents = []string{"adjudicator", "reviewer"}
	cfg.Review.Reviewers.Autoscale = config.ReviewerAutoscaleConfig{Enabled: true, Agent: "adjudicator", Min: 1, Max: 3, ScaleDownCycles: 2}
	if err := cfg.ExpandAgentReplicas(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func radEnabledAgentNames(cfg *config.Config) string {
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.EnabledAgents() {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

// radLogLines returns the log lines at level that contain substr.
func radLogLines(logs *bytes.Buffer, level, substr string) []string {
	var out []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level="+level) && strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
}

// radLastReconcile is the agent set of the last ReconcileAgents call.
func radLastReconcile(t *testing.T, rec *cruxtest.Recorder) string {
	t.Helper()
	call, ok := rec.Last("ReconcileAgents")
	if !ok || len(call.Args) != 1 {
		t.Fatalf("no ReconcileAgents call recorded: %v", rec.Methods())
	}
	names, ok := call.Args[0].([]string)
	if !ok {
		t.Fatalf("ReconcileAgents argument = %T", call.Args[0])
	}
	return strings.Join(names, " ")
}

// Moving the lane to another agent retires the old lane's extra replica at
// once, starts the new lane from its own declared count, drops what the
// controller remembered about the old lane, and says so in one line.
func TestReviewerAutoscaleFollowsAChangeOfLaneAgent(t *testing.T) {
	cfg := radAutoscaleConfig(t)
	rec := cruxtest.NewRecorder()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	rt := &reviewerAutoscaleRuntime{}
	ctx := context.Background()
	plan := review.DispatchPlan{}

	rt.Run(ctx, cfg, plan, rec, nil, logger, radAutoscaleT0)
	if st := rt.Status(); !st.Enabled || st.Agent != "adjudicator" || st.Current != 1 {
		t.Fatalf("first cycle status = %+v", st)
	}
	// What a scale-up decision does: the old lane now runs adjudicator-2.
	if err := cfg.SetReviewerAutoscaleActive(2); err != nil {
		t.Fatal(err)
	}
	rt.Run(ctx, cfg, plan, rec, nil, logger, radAutoscaleT0.Add(time.Minute))
	if _, ok := cfg.Agents["adjudicator-2"]; !ok {
		t.Fatalf("setup: old lane should be at 2: %s", radEnabledAgentNames(cfg))
	}
	// Two idle cycles: the controller is draining the old lane's replica.
	if !rt.Draining("adjudicator-2") || rt.Status().LastDecision == nil {
		t.Fatalf("setup: adjudicator-2 should be draining: %+v", rt.Status())
	}

	logs.Reset()
	cfg.Review.Reviewers.Autoscale.Agent = "reviewer"
	rt.Run(ctx, cfg, plan, rec, nil, logger, radAutoscaleT0.Add(2*time.Minute))

	if got := radEnabledAgentNames(cfg); got != "adjudicator reviewer scanner" {
		t.Fatalf("agents after the lane moved = %q, want the old lane back on 1 and the new lane on its declared 1", got)
	}
	if got := cfg.Agents["adjudicator"].ReplicaCount; got != 1 {
		t.Fatalf("old lane replica count = %d, want its declared 1", got)
	}
	if got := radLastReconcile(t, rec); got != "adjudicator reviewer scanner" {
		t.Fatalf("agent manager reconciled to %q", got)
	}
	st := rt.Status()
	if !st.Enabled || st.Agent != "reviewer" || st.Current != 1 {
		t.Fatalf("status after the lane moved = %+v", st)
	}
	// The old lane's two idle cycles and its drain decision are gone: the
	// new lane has seen one cycle.
	if st.DownStreak != 1 || st.UpStreak != 0 || st.LastDecision != nil || len(st.Draining) != 0 || rt.Draining("adjudicator-2") {
		t.Fatalf("controller state not reset for the new lane: %+v", st)
	}
	if cfg.ReviewerAutoscaleActive != 1 || cfg.ReviewerAutoscaleActiveAgent != "reviewer" {
		t.Fatalf("lane target = %d for %q", cfg.ReviewerAutoscaleActive, cfg.ReviewerAutoscaleActiveAgent)
	}
	changed := radLogLines(&logs, "INFO", "lane agent changed")
	if len(changed) != 1 {
		t.Fatalf("want one INFO line for the lane change, got %d:\n%s", len(changed), logs.String())
	}
	for _, want := range []string{"from=adjudicator", "to=reviewer", "adjudicator-2"} {
		if !strings.Contains(changed[0], want) {
			t.Fatalf("lane change line lacks %q: %s", want, changed[0])
		}
	}

	// The next cycle is an ordinary one: nothing more to say or reconcile.
	reconciles := 0
	for _, m := range rec.Methods() {
		if m == "ReconcileAgents" {
			reconciles++
		}
	}
	rt.Run(ctx, cfg, plan, rec, nil, logger, radAutoscaleT0.Add(3*time.Minute))
	if got := radLogLines(&logs, "INFO", "lane agent changed"); len(got) != 1 {
		t.Fatalf("lane change logged again:\n%s", logs.String())
	}
	after := 0
	for _, m := range rec.Methods() {
		if m == "ReconcileAgents" {
			after++
		}
	}
	if after != reconciles {
		t.Fatalf("an unchanged lane was reconciled again (%d → %d)", reconciles, after)
	}
}

// Turning autoscale off puts the lane back on its declared replicas, and
// turning it on again (here for another agent, with a higher floor) starts
// that lane from its declared count clamped into the band, not at the next
// scaling decision.
func TestReviewerAutoscaleReenabledStartsLaneFromDeclaredCount(t *testing.T) {
	cfg := radAutoscaleConfig(t)
	rec := cruxtest.NewRecorder()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	rt := &reviewerAutoscaleRuntime{}
	ctx := context.Background()
	plan := review.DispatchPlan{}

	rt.Run(ctx, cfg, plan, rec, nil, logger, radAutoscaleT0)
	if err := cfg.SetReviewerAutoscaleActive(3); err != nil {
		t.Fatal(err)
	}
	if got := radEnabledAgentNames(cfg); got != "adjudicator adjudicator-2 adjudicator-3 reviewer scanner" {
		t.Fatalf("setup: %q", got)
	}

	cfg.Review.Reviewers.Autoscale.Enabled = false
	rt.Run(ctx, cfg, plan, rec, nil, logger, radAutoscaleT0.Add(time.Minute))
	if got := radEnabledAgentNames(cfg); got != "adjudicator reviewer scanner" {
		t.Fatalf("agents after autoscale was turned off = %q", got)
	}
	if rt.Status().Enabled || cfg.ReviewerAutoscaleActive != 0 || cfg.ReviewerAutoscaleActiveAgent != "" {
		t.Fatalf("autoscale still active: %+v target=%d agent=%q", rt.Status(), cfg.ReviewerAutoscaleActive, cfg.ReviewerAutoscaleActiveAgent)
	}
	if got := radLogLines(&logs, "INFO", "disabled"); len(got) != 1 || !strings.Contains(got[0], "agent=adjudicator") {
		t.Fatalf("disable line: %v", got)
	}
	if got := radLogLines(&logs, "WARN", ""); len(got) != 0 {
		t.Fatalf("turning autoscale off is not a warning: %v", got)
	}

	cfg.Review.Reviewers.Autoscale.Enabled = true
	cfg.Review.Reviewers.Autoscale.Agent = "reviewer"
	cfg.Review.Reviewers.Autoscale.Min = 2
	rt.Run(ctx, cfg, plan, rec, nil, logger, radAutoscaleT0.Add(2*time.Minute))
	if got := radEnabledAgentNames(cfg); got != "adjudicator reviewer reviewer-2 scanner" {
		t.Fatalf("agents after autoscale was turned on again = %q, want the new lane at its floor of 2", got)
	}
	if got := radLastReconcile(t, rec); got != "adjudicator reviewer reviewer-2 scanner" {
		t.Fatalf("agent manager reconciled to %q", got)
	}
	if st := rt.Status(); !st.Enabled || st.Agent != "reviewer" || st.Current != 2 {
		t.Fatalf("status after re-enable = %+v", st)
	}
}

// A config that names an agent the swarm does not dispatch to keeps running
// with autoscale off, and the reason is logged once, not every cycle.
func TestReviewerAutoscaleWarnsOnceWhenItsAgentTakesNoReviews(t *testing.T) {
	cfg := radAutoscaleConfig(t)
	cfg.Review.ReviewerAgents = []string{"reviewer"}
	rec := cruxtest.NewRecorder()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	rt := &reviewerAutoscaleRuntime{}
	ctx := context.Background()
	plan := review.DispatchPlan{}

	for i := 0; i < 3; i++ {
		rt.Run(ctx, cfg, plan, rec, nil, logger, radAutoscaleT0.Add(time.Duration(i)*time.Minute))
	}
	if rt.Status().Enabled {
		t.Fatalf("autoscale is running for an agent that takes no reviews: %+v", rt.Status())
	}
	if got := radEnabledAgentNames(cfg); got != "adjudicator reviewer scanner" {
		t.Fatalf("agents = %q", got)
	}
	warns := radLogLines(&logs, "WARN", "not scaling")
	if len(warns) != 1 || !strings.Contains(warns[0], "adjudicator") || !strings.Contains(warns[0], "review.reviewer_agents") {
		t.Fatalf("want one warning naming the agent and the reviewer list, got %d:\n%s", len(warns), logs.String())
	}
	if _, ok := rec.Last("ReconcileAgents"); ok {
		t.Fatal("nothing changed, so nothing should have been reconciled")
	}

	// Clearing the name lets the hive pick the swarm's reviewer.
	cfg.Review.Reviewers.Autoscale.Agent = ""
	rt.Run(ctx, cfg, plan, rec, nil, logger, radAutoscaleT0.Add(3*time.Minute))
	if st := rt.Status(); !st.Enabled || st.Agent != "reviewer" {
		t.Fatalf("status with no agent named = %+v", st)
	}
	if got := radLogLines(&logs, "WARN", "not scaling"); len(got) != 1 {
		t.Fatalf("warned again after the fix:\n%s", logs.String())
	}
}
