package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

func doOwnerPostAsUser(s *Server, path, user string, body interface{}) *httptest.ResponseRecorder {
	var b bytes.Buffer
	json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, &b)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", user)
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

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

	resume := doOwnerPostAsUser(s, "/api/campaigns/stable-spec-8665/resume", "alice", map[string]string{"surface": "cli"})
	if resume.Code != http.StatusOK {
		t.Fatalf("resume = %d body=%s", resume.Code, resume.Body.String())
	}
	if !strings.Contains(resume.Body.String(), "spektacular plan status stable-spec-8665") {
		t.Fatalf("resume body missing CLI command: %s", resume.Body.String())
	}
}

func TestCampaignLeaseReleaseAndReviseFlows(t *testing.T) {
	s := newMinimalServer(t)
	s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)
	state, err := s.deps.Inception.Start("Lease protected campaign")
	if err != nil {
		t.Fatalf("start inception: %v", err)
	}
	if reset := doOwnerPost(s, "/api/inception/reset", map[string]interface{}{}); reset.Code != http.StatusOK {
		t.Fatalf("reset = %d body=%s", reset.Code, reset.Body.String())
	}

	resume := doOwnerPostAsUser(s, "/api/campaigns/"+state.IdeaSlug+"/resume", "alice", map[string]string{"surface": "chat"})
	if resume.Code != http.StatusOK {
		t.Fatalf("alice resume = %d body=%s", resume.Code, resume.Body.String())
	}
	blocked := doOwnerPostAsUser(s, "/api/campaigns/"+state.IdeaSlug+"/resume", "bob", map[string]string{"surface": "chat"})
	if blocked.Code != http.StatusConflict {
		t.Fatalf("bob resume = %d body=%s, want conflict", blocked.Code, blocked.Body.String())
	}
	releaseBlocked := doOwnerPostAsUser(s, "/api/campaigns/"+state.IdeaSlug+"/release", "bob", map[string]string{})
	if releaseBlocked.Code != http.StatusConflict {
		t.Fatalf("bob release = %d body=%s, want conflict", releaseBlocked.Code, releaseBlocked.Body.String())
	}
	released := doOwnerPostAsUser(s, "/api/campaigns/"+state.IdeaSlug+"/release", "alice", map[string]string{})
	if released.Code != http.StatusOK {
		t.Fatalf("alice release = %d body=%s", released.Code, released.Body.String())
	}
	resume = doOwnerPostAsUser(s, "/api/campaigns/"+state.IdeaSlug+"/resume", "bob", map[string]string{"surface": "chat"})
	if resume.Code != http.StatusOK {
		t.Fatalf("bob resume after release = %d body=%s", resume.Code, resume.Body.String())
	}

	revise := doOwnerPostAsUser(s, "/api/campaigns/"+state.IdeaSlug+"/revise", "bob", map[string]string{})
	if revise.Code != http.StatusOK {
		t.Fatalf("revise = %d body=%s", revise.Code, revise.Body.String())
	}
	var resp struct {
		OK       bool     `json:"ok"`
		Campaign Campaign `json:"campaign"`
	}
	if err := json.Unmarshal(revise.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode revise: %v", err)
	}
	if !resp.OK || resp.Campaign.RevisionOf != state.IdeaSlug || resp.Campaign.Revision == 0 || resp.Campaign.LeaseOwner != "bob" {
		t.Fatalf("revision campaign = %+v", resp.Campaign)
	}
}

func TestCampaignReviseSpektacularRunCreatesLinkedRevision(t *testing.T) {
	s, _ := runsTestServer(t)
	now := time.Now()
	if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-8665", "myorg/repo1", 8665, "myorg/repo1!stable-spec-8665:implement", "contributor", StageImplement, 3, now); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)

	revise := doOwnerPostAsUser(s, "/api/campaigns/stable-spec-8665/revise", "bob", map[string]string{})
	if revise.Code != http.StatusOK {
		t.Fatalf("revise = %d body=%s", revise.Code, revise.Body.String())
	}
	var resp struct {
		OK       bool     `json:"ok"`
		Campaign Campaign `json:"campaign"`
	}
	if err := json.Unmarshal(revise.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode revise: %v", err)
	}
	if !resp.OK || resp.Campaign.RevisionOf != "stable-spec-8665" || resp.Campaign.Engine != "Spektacular" || resp.Campaign.Type != "spektacular" {
		t.Fatalf("spektacular revision = %+v", resp.Campaign)
	}
	if resp.Campaign.CurrentStage != StagePlan || !strings.Contains(spektacularResumeCommand(resp.Campaign), "spektacular plan status ") {
		t.Fatalf("spektacular revision resume shape = %+v command=%q", resp.Campaign, spektacularResumeCommand(resp.Campaign))
	}
}

func TestCampaignReleaseSpektacularRunLease(t *testing.T) {
	s, _ := runsTestServer(t)
	s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)
	if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-8665", "myorg/repo1", 8665, "myorg/repo1!stable-spec-8665:plan", "contributor", StagePlan, 3, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}

	blocked := doOwnerPostAsUser(s, "/api/campaigns/stable-spec-8665/release", "bob", map[string]string{})
	if blocked.Code != http.StatusConflict {
		t.Fatalf("bob release = %d body=%s, want conflict", blocked.Code, blocked.Body.String())
	}
	released := doOwnerPostAsUser(s, "/api/campaigns/stable-spec-8665/release", "alice", map[string]string{})
	if released.Code != http.StatusOK {
		t.Fatalf("alice release = %d body=%s", released.Code, released.Body.String())
	}
	if _, ok := s.contributeHub.runLeaseHolder("myorg/repo1!stable-spec-8665:plan", time.Now()); ok {
		t.Fatalf("spektacular run lease still held after release")
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
