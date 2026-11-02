package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const delegatedOrderingHead = "feedfacefeedfacefeedfacefeedfacefeedface"

func TestDelegatedAppliedApprovalSurvivesReportingFailureAndRestartWithoutPaidRepeat(t *testing.T) {
	for _, status := range []int{0, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			mock := &repoItemDecisionMock{commentError: status}
			s := newDelegatedServer(t, mock)
			path := filepath.Join(t.TempDir(), "approvals.json")
			repoItemDecisionChangeRequests = func() review.HumanChangeRequestStore { return review.NewHumanChangeRequests(path) }
			decider := &fakeDecider{err: errors.New("use deterministic default")}
			withDecider(s, decider, true)
			job := decodeDelegatedJob(t, postDelegated(t, s, 8, "writer", `{"type":"pr"}`, map[string]string{"X-Hive-Role": "merger"}))
			if job.Status != delegatedJobStatusDone || job.ApprovalRecordedAt == nil || job.Error != "" {
				t.Fatalf("applied native decision did not complete: %+v", job)
			}
			if job.Result == nil || !strings.HasPrefix(job.Result.Outcome, "Native approval recorded") || strings.Contains(job.Result.Outcome, "`/hive decision` recorded") {
				t.Fatalf("AI comment and applied native approval are conflated: %+v", job.Result)
			}
			approval, ok := repoItemDecisionChangeRequests().For("testorg/testrepo", 8, delegatedOrderingHead)
			if !ok || !approval.IsApproval() || approval.Actor != "writer" {
				t.Fatalf("exact-head approval missing after reporting: %+v %v", approval, ok)
			}
			if status != 0 && !strings.Contains(strings.Join(job.Notes, "\n"), "external reporting incomplete") {
				t.Fatalf("partial reporting was hidden: %+v", job)
			}
			if status != 0 && !strings.Contains(job.Result.Next, "reporting is incomplete") {
				t.Fatalf("next gate hides failed external reporting: %+v", job.Result)
			}
			calls := decider.calls
			// A new process has neither the job nor the AI comment (the 403
			// case never posted it); only the persistent native approval.
			s.delegatedJobs = delegatedDecisionJobs{}
			out := s.decideDelegatedOne(context.Background(), "testorg/testrepo", 8, "pr", "writer", true, delegatedDecisionRequest{})
			if out.Err != nil || out.Result == nil || out.Result.Action != rowActionNoop || decider.calls != calls {
				t.Fatalf("restart repeated a paid decision after durable application: %+v calls=%d -> %d", out, calls, decider.calls)
			}
		})
	}
}

func TestDelegatedFailedNativeRecordCannotBecomeAppliedReplay(t *testing.T) {
	mock := &repoItemDecisionMock{}
	s := newDelegatedServer(t, mock)
	path := filepath.Join(t.TempDir(), "approvals.json")
	store := review.NewHumanChangeRequests(path)
	repoItemDecisionChangeRequests = func() review.HumanChangeRequestStore { return store }
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"pr"}`
	headers := map[string]string{"X-Hive-Role": "merger"}
	failed := decodeDelegatedJob(t, postDelegated(t, s, 8, "writer", body, headers))
	if failed.Status != delegatedJobStatusFailed || failed.ApprovalRecordedAt != nil {
		t.Fatalf("failed persistence was acknowledged: %+v", failed)
	}
	if _, ok := store.For("testorg/testrepo", 8, delegatedOrderingHead); ok || len(mock.comments) != 0 {
		t.Fatal("failed native record leaked an applied receipt or reporting")
	}
	if err := os.Remove(path + ".tmp"); err != nil {
		t.Fatal(err)
	}
	retried := decodeDelegatedJob(t, postDelegated(t, s, 8, "writer", body, headers))
	if retried.Status != delegatedJobStatusDone || retried.ApprovalRecordedAt == nil || retried.Result == nil || retried.Result.Action == rowActionNoop {
		t.Fatalf("same-process retry falsely replayed a failed approval: %+v", retried)
	}
	if got, ok := review.NewHumanChangeRequests(path).For("testorg/testrepo", 8, delegatedOrderingHead); !ok || !got.IsApproval() {
		t.Fatal("healthy retry did not persist native approval")
	}
}

func TestDelegatedNativeApprovalAndProjectionPrecedeHungProwWarning(t *testing.T) {
	started := make(chan struct{})
	mock := &repoItemDecisionMock{
		statusError: http.StatusForbidden, commentBlockText: "could not set the `prow/lgtm`",
		commentStarted: started, prLabels: []string{stagelabels.NeedsDecision.Label(), "needs-human"},
	}
	s := newDelegatedServer(t, mock)
	delegatedDecisionRunAsync = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.deps.Ctx = ctx
	s.deps.GHClient.SetAutoMergeLabel("lgtm")
	w := postDelegated(t, s, 8, "writer", `{"type":"pr","head":"`+delegatedOrderingHead+`"}`, map[string]string{"X-Hive-Role": "merger"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("job not accepted: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("optional Prow warning was not reached")
	}
	job, _ := s.delegatedJobs.get(delegatedJobKey("testorg/testrepo", 8))
	if job.ApprovalRecordedAt == nil {
		t.Fatal("hung bridge still withheld native decision checkpoint")
	}
	approval, ok := repoItemDecisionChangeRequests().For("testorg/testrepo", 8, delegatedOrderingHead)
	if !ok || !approval.IsApproval() {
		t.Fatal("native approval absent while optional bridge is blocked")
	}
	items := []StageTrackerItem{{Repo: "testorg/testrepo", Kind: "pr", Number: 8, Head: delegatedOrderingHead, Stage: 85, Modifiers: []string{modNeedsHuman, modNeedsDecision}}}
	s.applyOperatorActions(items, time.Now())
	s.applyDecidedProjection(items, time.Now())
	if items[0].Stage == 85 || items[0].DecisionNeeded != nil || items[0].Decided == nil {
		t.Fatalf("tracker still asks for the already-applied human decision: %+v", items[0])
	}
	s.stageTrackerCacheMu.Lock()
	stale := s.stageTrackerCache.stale
	s.stageTrackerCacheMu.Unlock()
	if !stale {
		t.Fatal("native approval did not invalidate cached projection")
	}
	cancel() // Supported server lifecycle cancellation ends optional transport.
	deadline := time.After(3 * time.Second)
	for {
		job, _ = s.delegatedJobs.get(delegatedJobKey("testorg/testrepo", 8))
		if job.Status != delegatedJobStatusRunning {
			break
		}
		select {
		case <-deadline:
			t.Fatal("applied decision remained running after reporting cancellation")
		case <-time.After(time.Millisecond):
		}
	}
	if job.Status != delegatedJobStatusDone || job.Error != "" || !strings.Contains(strings.Join(job.Notes, "\n"), "Prow bridge incomplete") {
		t.Fatalf("Prow reporting failure lost or reversed the decision: %+v", job)
	}
	if !strings.Contains(strings.Join(decisionAuditActions(s), ","), auditRepoItemDecisionDelegated+"_reporting_failed") {
		t.Fatal("Prow reporting failure not audited")
	}
}

func TestDelegatedChangedApprovalTargetFailsBeforeDecisionComment(t *testing.T) {
	for _, mode := range []string{"head", "body", "author", "actor"} {
		t.Run(mode, func(t *testing.T) {
			mock := &repoItemDecisionMock{}
			s := newDelegatedServer(t, mock)
			in := delegatedecision.Input{Repo: "testorg/testrepo", Number: 8, Kind: delegatedecision.KindPR, HeadSHA: delegatedOrderingHead, Author: "hive[bot]", HiveAuthored: true}
			actor := "writer"
			switch mode {
			case "head":
				in.HeadSHA = "old-head"
			case "body":
				in.Body = "old-body"
			case "author":
				in.Author = "old-author"
			case "actor":
				actor = "reader"
			}
			d := delegatedecision.Decision{Verdict: delegatedecision.VerdictApprove, Text: "approve",
				Actions: []delegatedecision.Action{{Kind: delegatedecision.ActionCommentDecision}, {Kind: delegatedecision.ActionApproveReview}}}
			if _, err := s.executeDelegatedDecision(context.Background(), in, d, actor, true); err == nil {
				t.Fatal("changed approval target was accepted")
			}
			if _, ok := repoItemDecisionChangeRequests().For("testorg/testrepo", 8, delegatedOrderingHead); ok || len(mock.comments) != 0 {
				t.Fatalf("changed target got native approval or success-shaped comment: comments=%v", mock.comments)
			}
		})
	}
}

func TestOperatorQueueApprovalCommitsBeforeForbiddenReporting(t *testing.T) {
	for _, mode := range []string{"comment", "prow"} {
		t.Run(mode, func(t *testing.T) {
			mock := &repoItemDecisionMock{}
			if mode == "comment" {
				mock.commentError = http.StatusForbidden
			} else {
				mock.statusError = http.StatusForbidden
			}
			s := newRepoItemDecisionServer(t, mock)
			s.deps.GHClient.SetAutoMergeLabel("lgtm")
			w := postRepoItemDecisionAs(t, s, 8, "writer", "merger", `{"decision":"queue-merge","type":"pr"}`)
			var response repoItemDecisionResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || response.Status != "approval-recorded-reporting-incomplete" ||
				response.Result == nil || response.Result.Action != rowActionApproved || !strings.Contains(response.Result.Next, "merge gates still apply") {
				t.Fatalf("native queue approval was withheld or falsely claimed queued: %d %+v", w.Code, response)
			}
			if approval, ok := repoItemDecisionChangeRequests().For("testorg/testrepo", 8, delegatedOrderingHead); !ok || !approval.IsApproval() || approval.Actor != "writer" {
				t.Fatal("optional queue reporting prevented exact-head operator approval")
			}
			if len(mock.merges) != 0 {
				t.Fatal("queue approval bypassed the merge engine")
			}
		})
	}
}

func TestOperatorQueueApprovalCompletesAfterHungBridgeCancellation(t *testing.T) {
	started := make(chan struct{})
	mock := &repoItemDecisionMock{statusError: http.StatusForbidden, commentBlockText: "could not set the `prow/lgtm`", commentStarted: started}
	s := newRepoItemDecisionServer(t, mock)
	s.deps.GHClient.SetAutoMergeLabel("lgtm")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.deps.Ctx = ctx
	done := make(chan []byte, 1)
	go func() {
		w := postRepoItemDecisionAs(t, s, 8, "writer", "merger", `{"decision":"queue-merge","type":"pr"}`)
		done <- w.Body.Bytes()
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("operator queue did not reach optional bridge")
	}
	if approval, ok := repoItemDecisionChangeRequests().For("testorg/testrepo", 8, delegatedOrderingHead); !ok || !approval.IsApproval() {
		t.Fatal("operator queue still records approval after optional bridge")
	}
	cancel()
	select {
	case body := <-done:
		var response repoItemDecisionResponse
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatal(err)
		}
		if response.Status != "approval-recorded-reporting-incomplete" || response.Result == nil || response.Result.Action != rowActionApproved {
			t.Fatalf("operator queue did not report applied decision separately: %s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("operator queue remained blocked after lifecycle cancellation")
	}
}

func TestDelegatedRejectedDecisionDoesNotRecordNativeApproval(t *testing.T) {
	mock := &repoItemDecisionMock{prLabels: []string{"hold"}}
	s := newDelegatedServer(t, mock)
	job := decodeDelegatedJob(t, postDelegated(t, s, 8, "writer", `{"type":"pr"}`, map[string]string{"X-Hive-Role": "merger"}))
	if job.Decision == nil || job.Decision.Verdict != delegatedecision.VerdictRefused || job.ApprovalRecordedAt != nil {
		t.Fatalf("rejection behavior changed: %+v", job)
	}
	if _, ok := repoItemDecisionChangeRequests().For("testorg/testrepo", 8, delegatedOrderingHead); ok {
		t.Fatal("rejected decision recorded approval")
	}
}
