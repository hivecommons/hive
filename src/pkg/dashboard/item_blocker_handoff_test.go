package dashboard

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func setupWorkflowsHandoff(t *testing.T) (*Server, *repoItemDecisionMock) {
	t.Helper()
	l := swapBlockerLedger(t)
	l.Observe("testorg/testrepo", 7, scannerWorkflowsReport, time.Now().Add(-time.Hour))
	mock := &repoItemDecisionMock{issueLabels: []string{"agent/sec-check", "needs-decision", "stage/045-needs-decision"}}
	s := newDelegatedServer(t, mock)
	s.deps.Config.Agents = map[string]config.AgentConfig{
		"sec-check": {Role: "sec-check", Mode: "ISSUES_AND_PRS", Enabled: true},
		"scanner":   {Role: "scanner", Mode: "ISSUES_PRS_MERGE", Enabled: true},
	}
	fakeInstallationProbe(t, map[string]string{"workflows": "write"}, nil)
	return s, mock
}

func TestWorkflowsHandoffAction(t *testing.T) {
	s, mock := setupWorkflowsHandoff(t)
	w := postBlockerAction(t, s, "handoff", "writer")
	if w.Code != http.StatusOK {
		t.Fatalf("handoff: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(strings.Join(mock.added, ","), ghpkg.WorkflowsLaneLabel) || !strings.Contains(strings.Join(mock.comments, " "), "scanner") {
		t.Fatalf("handoff not recorded: labels=%v comments=%v", mock.added, mock.comments)
	}
	if stageTrackerBlockerFor("testorg/testrepo", 7) != nil {
		t.Fatal("blocker remains active")
	}
	if len(mock.removed) == 0 {
		t.Fatal("park not lifted")
	}
}

func TestWorkflowsHandoffRequiresPermissionAndWriter(t *testing.T) {
	s, mock := setupWorkflowsHandoff(t)
	if w := postBlockerAction(t, s, "handoff", "reader"); w.Code != http.StatusForbidden {
		t.Fatalf("reader: %d", w.Code)
	}
	fakeInstallationProbe(t, map[string]string{}, nil)
	if w := postBlockerAction(t, s, "handoff", "writer"); w.Code != http.StatusConflict {
		t.Fatalf("missing permission: %d %s", w.Code, w.Body.String())
	}
	if len(mock.added) != 0 || len(mock.comments) != 0 || len(mock.removed) != 0 {
		t.Fatal("unexpected writes")
	}
}

func TestDelegatedWorkflowsHandoffAndPreview(t *testing.T) {
	s, mock := setupWorkflowsHandoff(t)
	plan, err := s.planDelegatedDecision(context.Background(), "testorg/testrepo", 7, "issue", false, delegatedDecisionRequest{})
	if err != nil || plan.Rule != "blocker:workflows-handoff" || !strings.Contains(plan.Summary, "scanner") {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if len(mock.added) != 0 || len(mock.comments) != 0 {
		t.Fatal("preview wrote to GitHub")
	}
	out := s.decideDelegatedOne(context.Background(), "testorg/testrepo", 7, "issue", "writer", false, delegatedDecisionRequest{})
	if out.Err != nil || out.Decision == nil || out.Decision.Verdict != delegatedecision.VerdictDirection || out.Result == nil || out.Result.Action != rowActionReleased {
		t.Fatalf("outcome=%+v", out)
	}
	if !strings.Contains(strings.Join(mock.added, ","), ghpkg.WorkflowsLaneLabel) || !strings.Contains(strings.Join(mock.comments, " "), "scanner") {
		t.Fatalf("labels=%v comments=%v", mock.added, mock.comments)
	}
}

func TestWorkflowsHandoffRespectsHoldAndRollsBackFailure(t *testing.T) {
	t.Run("hold", func(t *testing.T) {
		s, mock := setupWorkflowsHandoff(t)
		mock.issueLabels = append(mock.issueLabels, "hive-pause/h1")
		w := postBlockerAction(t, s, "handoff", "writer")
		if w.Code != http.StatusConflict || len(mock.added) != 0 {
			t.Fatalf("held: %d labels=%v", w.Code, mock.added)
		}
	})
	t.Run("decision failure", func(t *testing.T) {
		s, mock := setupWorkflowsHandoff(t)
		mock.commentError = http.StatusForbidden
		w := postBlockerAction(t, s, "handoff", "writer")
		if w.Code != http.StatusBadGateway || !strings.Contains(strings.Join(mock.removed, ","), ghpkg.WorkflowsLaneLabel) {
			t.Fatalf("failure: %d removed=%v", w.Code, mock.removed)
		}
		if stageTrackerBlockerFor("testorg/testrepo", 7) == nil {
			t.Fatal("failure cleared blocker")
		}
	})
}

func TestDelegatedWorkflowsGrantedWithoutLane(t *testing.T) {
	s, mock := setupWorkflowsHandoff(t)
	delete(s.deps.Config.Agents, "scanner")
	out := s.decideDelegatedOne(context.Background(), "testorg/testrepo", 7, "issue", "writer", false, delegatedDecisionRequest{})
	if out.Err != nil || out.Result == nil || out.Result.Action != rowActionNoEffect || !strings.Contains(out.Result.Outcome, "App has `workflows: write`") || !strings.Contains(out.Result.Outcome, "sec-check") {
		t.Fatalf("outcome=%+v", out)
	}
	for _, option := range out.Result.Options {
		if strings.Contains(option.Label, "Grant the App") {
			t.Fatalf("unexpected grant option: %+v", option)
		}
	}
	if len(mock.added) != 0 || len(mock.comments) != 0 {
		t.Fatal("unexpected writes")
	}
}
