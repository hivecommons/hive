package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

func driveInceptionToScaffold(t *testing.T, s *Server) {
	t.Helper()
	if rec := doPost(s, "/api/inception/start", map[string]interface{}{"idea": "ship a run-backed idea"}); rec.Code != http.StatusOK {
		t.Fatalf("start: %d body=%s", rec.Code, rec.Body.String())
	}
	qs := []knowledge.Question{{ID: "scope", Text: "What scope?", Category: "features"}}
	if rec := doPost(s, "/api/inception/questions", map[string]interface{}{"questions": qs}); rec.Code != http.StatusOK {
		t.Fatalf("questions: %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := doPost(s, "/api/inception/answer", map[string]interface{}{"answers": map[string]string{"scope": "first spec"}}); rec.Code != http.StatusOK {
		t.Fatalf("answer: %d body=%s", rec.Code, rec.Body.String())
	}
	facts := []knowledge.IdeationFact{
		{Title: "Vision", Body: "Run-backed inception", Type: knowledge.FactVision},
		{Title: "Constitution", Body: "Use Go", Type: knowledge.FactConstitution},
		{Title: "Requirement", Body: "Create the first spec run", Type: knowledge.FactRequirement},
	}
	if rec := doPost(s, "/api/inception/facts", map[string]interface{}{"facts": facts}); rec.Code != http.StatusOK {
		t.Fatalf("facts: %d body=%s", rec.Code, rec.Body.String())
	}
}

func pendingRunStages(t *testing.T, s *Server) int {
	t.Helper()
	stages, err := s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil {
		t.Fatalf("PendingRunStages: %v", err)
	}
	return len(stages)
}

func TestInceptionApproveAdmitsSpecRunWhenSpektacularEnabled(t *testing.T) {
	s, _, _ := covFInceptionServer(t)
	s.contributeHub.persistTaskLedgers = false
	s.deps.Config.Runs.Spektacular.Enabled = true
	disableSpekHubExecutorForRelayTests(s)
	driveInceptionToScaffold(t, s)

	rec := doPost(s, "/api/inception/approve", map[string]interface{}{
		"issue_url": "https://github.com/myorg/repo1/issues/8290",
		"title":     "Inception follow-up",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d body=%s", rec.Code, rec.Body.String())
	}

	stages, err := s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 || stages[0].Repo != "myorg/repo1" || stages[0].Stage != StageSpec {
		t.Fatalf("pending stages = %+v", stages)
	}
	foundAudit := false
	for _, entry := range s.GetAudit().Recent(0) {
		if entry.Action == "inception_run_admitted" && strings.Contains(entry.Detail, "repo=myorg/repo1") {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Fatalf("inception_run_admitted audit entry not recorded: %+v", s.GetAudit().Recent(0))
	}
}

func TestInceptionApproveSpektacularDisabledDoesNotAdmitRun(t *testing.T) {
	s, _, _ := covFInceptionServer(t)
	s.contributeHub.persistTaskLedgers = false
	driveInceptionToScaffold(t, s)

	rec := doPost(s, "/api/inception/approve", map[string]interface{}{
		"issue_url": "https://github.com/myorg/repo1/issues/8290",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d body=%s", rec.Code, rec.Body.String())
	}
	if got := pendingRunStages(t, s); got != 0 {
		t.Fatalf("pending stages = %d, want 0", got)
	}
}

func TestInceptionApproveTwiceAdmitsOneSpecRun(t *testing.T) {
	s, _, _ := covFInceptionServer(t)
	s.contributeHub.persistTaskLedgers = false
	s.deps.Config.Runs.Spektacular.Enabled = true
	disableSpekHubExecutorForRelayTests(s)
	driveInceptionToScaffold(t, s)
	body := map[string]interface{}{
		"repo":         "repo1",
		"issue_number": 8290,
	}

	if rec := doPost(s, "/api/inception/approve", body); rec.Code != http.StatusOK {
		t.Fatalf("first approve: %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := doPost(s, "/api/inception/approve", body); rec.Code != http.StatusOK {
		t.Fatalf("second approve: %d body=%s", rec.Code, rec.Body.String())
	}
	if got := pendingRunStages(t, s); got != 1 {
		t.Fatalf("pending stages = %d, want 1", got)
	}
}

func TestInceptionApproveRequiresExplicitRepoTarget(t *testing.T) {
	s, _, _ := covFInceptionServer(t)
	s.contributeHub.persistTaskLedgers = false
	s.deps.Config.Runs.Spektacular.Enabled = true
	disableSpekHubExecutorForRelayTests(s)
	driveInceptionToScaffold(t, s)

	for _, body := range []map[string]interface{}{{"issue_number": 8290}, {"repo": "repo1"}} {
		if rec := doPost(s, "/api/inception/approve", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("approve %v: got %d body=%s, want 400", body, rec.Code, rec.Body.String())
		}
	}
	if got := pendingRunStages(t, s); got != 0 {
		t.Fatalf("pending stages = %d, want 0", got)
	}
	if st := s.deps.Inception.GetState(); st == nil || st.Phase != knowledge.PhaseScaffold {
		t.Fatalf("half-specified target changed inception state: %+v", st)
	}
}

func TestInceptionApproveReportsAdmittedRunKey(t *testing.T) {
	s, _, _ := covFInceptionServer(t)
	s.contributeHub.persistTaskLedgers = false
	s.deps.Config.Runs.Spektacular.Enabled = true
	disableSpekHubExecutorForRelayTests(s)
	driveInceptionToScaffold(t, s)

	rec := doPost(s, "/api/inception/approve", map[string]interface{}{"repo": "repo1", "issue_number": 8290})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK       bool   `json:"ok"`
		Admitted bool   `json:"admitted"`
		RunKey   string `json:"run_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if !resp.OK || !resp.Admitted || resp.RunKey != "myorg/repo1#8290" {
		t.Fatalf("response = %+v, want admitted run_key myorg/repo1#8290", resp)
	}
	st := s.deps.Inception.GetState()
	if st == nil || st.Phase != knowledge.PhaseComplete || st.AdmittedRunKey != "myorg/repo1#8290" {
		t.Fatalf("state = %+v, want complete with admitted_run_key", st)
	}
}

func TestInceptionApproveAdmissionFailureLeavesInceptionIncomplete(t *testing.T) {
	s, _, _ := covFInceptionServer(t)
	s.deps.Config.Runs.Spektacular.Enabled = true
	disableSpekHubExecutorForRelayTests(s)
	driveInceptionToScaffold(t, s)
	hub := s.contributeHub
	s.contributeHub = nil
	rec := doPost(s, "/api/inception/approve", map[string]interface{}{"repo": "repo1", "issue_number": 8290})
	s.contributeHub = hub
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("approve: got %d body=%s, want 503", rec.Code, rec.Body.String())
	}
	if st := s.deps.Inception.GetState(); st == nil || st.Phase != knowledge.PhaseScaffold || st.AdmittedRunKey != "" {
		t.Fatalf("failed admission changed inception state: %+v", st)
	}
}

func TestInceptionApproveRejectsNonGitHubIssueURL(t *testing.T) {
	s, _, _ := covFInceptionServer(t)
	s.contributeHub.persistTaskLedgers = false
	s.deps.Config.Runs.Spektacular.Enabled = true
	disableSpekHubExecutorForRelayTests(s)
	driveInceptionToScaffold(t, s)

	rec := doPost(s, "/api/inception/approve", map[string]interface{}{
		"issue_url": "https://example.com/myorg/repo1/issues/8290",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("approve: got %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if got := pendingRunStages(t, s); got != 0 {
		t.Fatalf("pending stages = %d, want 0", got)
	}
}
