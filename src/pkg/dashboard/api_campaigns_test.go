package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

func decodeCampaignList(t *testing.T, body []byte) []Campaign {
	t.Helper()
	var resp struct {
		OK        bool       `json:"ok"`
		Campaigns []Campaign `json:"campaigns"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode campaigns: %v body=%s", err, string(body))
	}
	if !resp.OK {
		t.Fatalf("campaign response not ok: %s", string(body))
	}
	return resp.Campaigns
}

func TestCampaignsArchiveInceptionOnResetAndResume(t *testing.T) {
	s := newMinimalServer(t)
	s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)
	state, err := s.deps.Inception.Start("Prepare bootc-installer for a beta release")
	if err != nil {
		t.Fatalf("start inception: %v", err)
	}

	reset := doOwnerPost(s, "/api/inception/reset", map[string]interface{}{})
	if reset.Code != http.StatusOK {
		t.Fatalf("reset = %d body=%s", reset.Code, reset.Body.String())
	}
	if active := s.deps.Inception.GetState(); active != nil {
		t.Fatalf("state after reset = %+v, want nil", active)
	}

	list := doOwnerGet(s, "/api/campaigns?search=bootc")
	if list.Code != http.StatusOK {
		t.Fatalf("campaigns list = %d body=%s", list.Code, list.Body.String())
	}
	campaigns := decodeCampaignList(t, list.Body.Bytes())
	if len(campaigns) != 1 {
		t.Fatalf("campaigns len = %d, want 1: %+v", len(campaigns), campaigns)
	}
	if campaigns[0].ID != state.IdeaSlug || campaigns[0].Engine != "Spec Kit" || campaigns[0].Type != "inception" {
		t.Fatalf("archived campaign = %+v", campaigns[0])
	}

	resume := doOwnerPost(s, "/api/campaigns/"+state.IdeaSlug+"/resume", map[string]string{"surface": "chat"})
	if resume.Code != http.StatusOK {
		t.Fatalf("resume = %d body=%s", resume.Code, resume.Body.String())
	}
	restored := s.deps.Inception.GetState()
	if restored == nil || restored.IdeaText != state.IdeaText || restored.IdeaSlug != state.IdeaSlug {
		t.Fatalf("restored state = %+v, want idea %q slug %q", restored, state.IdeaText, state.IdeaSlug)
	}

	reset = doOwnerPost(s, "/api/inception/reset", map[string]interface{}{})
	if reset.Code != http.StatusOK {
		t.Fatalf("second reset = %d body=%s", reset.Code, reset.Body.String())
	}
	second, err := s.deps.Inception.Start("Second campaign that must not be lost")
	if err != nil {
		t.Fatalf("start second inception: %v", err)
	}
	resume = doOwnerPost(s, "/api/campaigns/"+state.IdeaSlug+"/resume", map[string]string{"surface": "chat"})
	if resume.Code != http.StatusOK {
		t.Fatalf("resume with active state = %d body=%s", resume.Code, resume.Body.String())
	}
	list = doOwnerGet(s, "/api/campaigns?search=Second")
	campaigns = decodeCampaignList(t, list.Body.Bytes())
	if len(campaigns) != 1 || campaigns[0].ID != second.IdeaSlug {
		t.Fatalf("active state was not archived before resume: %+v", campaigns)
	}
}

func TestCampaignsArchiveCompletedInceptionBeforeStart(t *testing.T) {
	s := newMinimalServer(t)
	s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)
	first, err := s.deps.Inception.Start("Completed campaign should stay resumable")
	if err != nil {
		t.Fatalf("start first inception: %v", err)
	}
	if err := s.deps.Inception.SetQuestions([]knowledge.Question{{ID: "goal", Text: "Goal?"}}); err != nil {
		t.Fatalf("set questions: %v", err)
	}
	if _, err := s.deps.Inception.SubmitAnswers(map[string]string{"goal": "Ship it"}); err != nil {
		t.Fatalf("submit answers: %v", err)
	}
	if err := s.deps.Inception.RecordFacts(context.Background(), []knowledge.IdeationFact{{Type: knowledge.FactRequirement, Title: "Ship", Body: "Ship it"}}); err != nil {
		t.Fatalf("record facts: %v", err)
	}
	if err := s.deps.Inception.AdvanceToComplete(); err != nil {
		t.Fatalf("complete inception: %v", err)
	}

	rec := doOwnerPost(s, "/api/inception/start", map[string]string{"idea": "Brand new campaign"})
	if rec.Code != http.StatusOK {
		t.Fatalf("start replacement = %d body=%s", rec.Code, rec.Body.String())
	}
	list := doOwnerGet(s, "/api/campaigns?search=Completed")
	if list.Code != http.StatusOK {
		t.Fatalf("campaigns list = %d body=%s", list.Code, list.Body.String())
	}
	campaigns := decodeCampaignList(t, list.Body.Bytes())
	if len(campaigns) != 1 || campaigns[0].ID != first.IdeaSlug || campaigns[0].Status != "shipped" {
		t.Fatalf("completed inception was not archived before replacement: %+v", campaigns)
	}
}

func TestCampaignsIncludeSpektacularRunsAndFilters(t *testing.T) {
	s, _ := runsTestServer(t)
	now := time.Now()
	if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-8665", "myorg/repo1", 8665, "myorg/repo1!stable-spec-8665:plan", "contributor", StagePlan, 3, now); err != nil {
		t.Fatalf("record lease: %v", err)
	}

	rec := doOwnerGet(s, "/api/campaigns?repo=repo1&stage=plan&status=planned&owner=alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("campaigns list = %d body=%s", rec.Code, rec.Body.String())
	}
	campaigns := decodeCampaignList(t, rec.Body.Bytes())
	if len(campaigns) != 1 {
		t.Fatalf("campaigns len = %d, want 1: %+v", len(campaigns), campaigns)
	}
	got := campaigns[0]
	if got.ID != "stable-spec-8665" || got.Engine != "Spektacular" || got.RunURL == "" || got.Status != "planned" {
		t.Fatalf("run campaign = %+v", got)
	}

	resume := doOwnerPost(s, "/api/campaigns/stable-spec-8665/resume", map[string]string{"surface": "cli"})
	if resume.Code != http.StatusOK {
		t.Fatalf("resume = %d body=%s", resume.Code, resume.Body.String())
	}
	if !strings.Contains(resume.Body.String(), "spektacular plan status stable-spec-8665") {
		t.Fatalf("resume body missing CLI command: %s", resume.Body.String())
	}
}

func TestCampaignFromCompletedRunUsesStableSpecID(t *testing.T) {
	campaign := campaignFromRun(Run{
		Key:            "myorg/repo1!stable-spec-8665:implement",
		Repo:           "myorg/repo1",
		Stage:          "completed",
		State:          "completed",
		StageStartedAt: "2026-09-24T12:00:00Z",
	})
	if campaign.ID != "stable-spec-8665" {
		t.Fatalf("campaign id = %q, want stable spec id", campaign.ID)
	}
	if !strings.Contains(spektacularResumeCommand(campaign), "stable-spec-8665") {
		t.Fatalf("resume command = %q, want stable spec id", spektacularResumeCommand(campaign))
	}
}
