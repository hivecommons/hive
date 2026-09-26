package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/timeline"
)

func checkpointTestServer(t *testing.T, title string, gen uint64) (*Server, *beads.Store, string, string) {
	t.Helper()
	s, deps := runsTestServer(t)
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	epic, err := store.Create("mobile checkpoint plan", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	runKey := "myorg/repo1#8618"
	if err := store.Update(epic.ID, func(b *beads.Bead) {
		b.Metadata[planning.MetaPlanStatus] = planning.PlanStatusDraft
		b.Metadata[planning.MetaIssueRepo] = "myorg/repo1"
		b.Metadata[planning.MetaIssueNumber] = "8618"
		b.Metadata[planning.MetaRunKey] = runKey
	}); err != nil {
		t.Fatalf("update epic: %v", err)
	}
	deps.BeadStores = map[string]*beads.Store{"architect": store}
	if title == "" {
		title = "Approve mobile checkpoint contract"
	}
	if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-8618", "myorg/repo1", 8618, runKey, "contributor", StagePlan, gen, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	s.contributeHub.leaseMu.Lock()
	for _, l := range s.contributeHub.leases {
		if l != nil && l.taskID == "task-8618" {
			l.title = title
		}
	}
	s.contributeHub.leaseMu.Unlock()
	return s, store, epic.ID, runKey
}

func doPostNoOwner(s *Server, path string, body interface{}) *httptest.ResponseRecorder {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, &b)
	req.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeCheckpointPayload(t *testing.T, rec *httptest.ResponseRecorder) RunCheckpointPayload {
	t.Helper()
	var payload RunCheckpointPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode checkpoint payload: %v body=%s", err, rec.Body.String())
	}
	return payload
}

func TestRunCheckpointPayloadCapsSummary(t *testing.T) {
	s, _, _, runKey := checkpointTestServer(t, strings.Repeat("å", RunCheckpointSummaryMaxBytes), 7)

	rec := doOwnerGet(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}

	payload := decodeCheckpointPayload(t, rec)
	if len(payload.Summary) > RunCheckpointSummaryMaxBytes {
		t.Fatalf("summary len = %d, want <= %d", len(payload.Summary), RunCheckpointSummaryMaxBytes)
	}
	if !utf8.ValidString(payload.Summary) {
		t.Fatalf("summary is not valid UTF-8")
	}
	if payload.SummaryMaxBytes != RunCheckpointSummaryMaxBytes {
		t.Fatalf("summary max = %d, want %d", payload.SummaryMaxBytes, RunCheckpointSummaryMaxBytes)
	}
}

func TestRunCheckpointPayloadIncludesApprovalProvenance(t *testing.T) {
	s, _, epicID, runKey := checkpointTestServer(t, "approved by ACMM", 7)
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: runKey,
		Kind:     timeline.KindStageApproval,
		Agent:    runCheckpointAutoActor,
		At:       time.Now().UnixMilli(),
		Attrs: map[string]string{
			stageAttrStage:               StagePlan,
			stageAttrGen:                 "7",
			runCheckpointActorKey:        runCheckpointAutoActor,
			runCheckpointReasonKey:       "checkpoint_disabled; ACMM L6 plan_auto_approve",
			runCheckpointConfigSourceKey: "hive.yaml",
			runCheckpointEpicKey:         epicID,
		},
	})

	rec := doOwnerGet(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	payload := decodeCheckpointPayload(t, rec)
	if payload.Approval[runCheckpointActorKey] != runCheckpointAutoActor ||
		payload.Approval[runCheckpointConfigSourceKey] != "hive.yaml" ||
		payload.Approval[runCheckpointEpicKey] != epicID ||
		!strings.Contains(payload.Approval[runCheckpointReasonKey], "ACMM L6") {
		t.Fatalf("approval provenance = %+v", payload.Approval)
	}
}

func TestRunCheckpointDecisionRefusesStaleGeneration(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "", 9)

	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionApprove, Gen: 8})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale approve = %d body=%s", rec.Code, rec.Body.String())
	}
	epic, _ := store.Get(epicID)
	if got := epic.Meta(planning.MetaPlanStatus); got != planning.PlanStatusDraft {
		t.Fatalf("plan status after stale approve = %q, want draft", got)
	}
}

func TestRunCheckpointDecisionRequiresOwner(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "", 3)

	rec := doPostNoOwner(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionApprove, Gen: 3})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner approve = %d body=%s", rec.Code, rec.Body.String())
	}
	epic, _ := store.Get(epicID)
	if got := epic.Meta(planning.MetaPlanStatus); got != planning.PlanStatusDraft {
		t.Fatalf("plan status after unauthorized approve = %q, want draft", got)
	}
}

func TestRunCheckpointDecisionApprovesCurrentGeneration(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "", 4)

	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionApprove, Gen: 4})
	if rec.Code != http.StatusOK {
		t.Fatalf("current-generation approve = %d body=%s", rec.Code, rec.Body.String())
	}
	epic, _ := store.Get(epicID)
	if got := epic.Meta(planning.MetaPlanStatus); got != planning.PlanStatusApproved {
		t.Fatalf("plan status after approve = %q, want approved", got)
	}
}

func TestRunCheckpointRejectResetsImplementLease(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "", 6)
	if err := store.Update(epicID, func(b *beads.Bead) {
		b.Metadata[planning.MetaPlanStatus] = planning.PlanStatusApproved
		b.Metadata[planning.MetaRunWaitingReason] = planning.WaitingReasonStalePlan
	}); err != nil {
		t.Fatalf("mark stale approved plan: %v", err)
	}
	s.contributeHub.leaseMu.Lock()
	for _, l := range s.contributeHub.leases {
		if l != nil && l.taskID == "task-8618" {
			l.stage = StageImplement
			l.gen = 6
		}
	}
	s.contributeHub.leaseMu.Unlock()

	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionReject, Gen: 6})
	if rec.Code != http.StatusOK {
		t.Fatalf("reject checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	epic, _ := store.Get(epicID)
	if got := epic.Meta(planning.MetaPlanStatus); got != planning.PlanStatusDraft {
		t.Fatalf("plan status after reject = %q, want draft", got)
	}
	lease, ok := s.contributeHub.runLeaseHolder(runKey, time.Now())
	if !ok {
		t.Fatalf("run lease not found after reject")
	}
	if lease.stage != StagePlan || lease.gen <= 6 {
		t.Fatalf("lease after reject = stage %q gen %d, want plan and new generation", lease.stage, lease.gen)
	}
}

func TestRunCheckpointPayloadIdenticalAcrossSurfaces(t *testing.T) {
	s, _, _, runKey := checkpointTestServer(t, "", 5)

	rec := doOwnerGet(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}

	fromHTTP := decodeCheckpointPayload(t, rec)
	fromChat, err := s.RunCheckpointNotificationPayload(runKey, "chat")
	if err != nil {
		t.Fatalf("chat payload: %v", err)
	}
	fromPush, err := s.RunCheckpointNotificationPayload(runKey, "push")
	if err != nil {
		t.Fatalf("push payload: %v", err)
	}
	if !reflect.DeepEqual(fromHTTP, fromChat) || !reflect.DeepEqual(fromHTTP, fromPush) {
		t.Fatalf("payloads differ\nhttp=%+v\nchat=%+v\npush=%+v", fromHTTP, fromChat, fromPush)
	}
}

func TestRunCheckpointPayloadSummarizesSpecDocument(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "Review spec checkpoint", 12)
	oldReceipts := runReceiptsDir
	runReceiptsDir = t.TempDir()
	t.Cleanup(func() { runReceiptsDir = oldReceipts })
	if err := store.Update(epicID, func(b *beads.Bead) {
		b.Metadata[planning.MetaDesignVia] = planning.DesignViaSpektacular
		b.Metadata[planning.MetaDesignStatus] = planning.DesignStatusRequested
	}); err != nil {
		t.Fatalf("mark design epic: %v", err)
	}
	s.contributeHub.leaseMu.Lock()
	for _, l := range s.contributeHub.leases {
		if l != nil && l.taskID == "task-8618" {
			l.stage = StageSpec
			l.gen = 12
			l.key = "myorg/repo1!" + runKey + ":" + StageSpec
		}
	}
	s.contributeHub.leaseMu.Unlock()
	if _, err := writeStageReceipt(runKey, StageSpec, 12, []byte(`{}`)); err != nil {
		t.Fatalf("write spec receipt: %v", err)
	}
	if err := writeSpekStageCapture(runKey, StageSpec, 12, RunDetailStageCapture{
		SchemaVersion: "1",
		RunKey:        runKey,
		Stage:         StageSpec,
		Generation:    12,
		Documents: []RunDetailStageDocument{{
			Path:     "spec.md",
			Markdown: "# Checkout redesign\n\n## Goals\n\n## Non-goals",
			Content:  "# Checkout redesign\n\n## Goals\n\n## Non-goals",
		}},
	}); err != nil {
		t.Fatalf("write spec capture: %v", err)
	}

	rec := doOwnerGet(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET spec checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	payload := decodeCheckpointPayload(t, rec)
	if payload.Stage != StageSpec || payload.Gen != 12 {
		t.Fatalf("payload stage/gen = %s/%d, want spec/12", payload.Stage, payload.Gen)
	}
	for _, want := range []string{"Checkout redesign", "- Goals", "- Non-goals", "Spec detail:"} {
		if !strings.Contains(payload.Summary, want) {
			t.Fatalf("spec summary missing %q: %s", want, payload.Summary)
		}
	}
}
