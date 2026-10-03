package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	resetLifecycleStore()
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

// Resume on a Spektacular run is not gated on its stage-lease identity: a
// freshly admitted run is held by hive-triage, which no operator is
// (hivecommons/hive#10059).
func TestCampaignResumeSpektacularRunHeldByStageIdentity(t *testing.T) {
	s, _ := runsTestServer(t)
	if err := s.contributeHub.recordLeaseForKeyStage(runAdmissionIdentity, "task-10059", "myorg/repo1", 10059, "myorg/repo1!stable-spec-10059:spec", "contributor", StageSpec, 1, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	resume := doOwnerPostAsUser(s, "/api/campaigns/stable-spec-10059/resume", "owner", map[string]string{"surface": "dashboard"})
	if resume.Code != http.StatusOK {
		t.Fatalf("owner resume of admitted run = %d body=%s", resume.Code, resume.Body.String())
	}
	if !strings.Contains(resume.Body.String(), "stable-spec-10059") {
		t.Fatalf("resume body missing run: %s", resume.Body.String())
	}
}

// Pending server-side stage leases are also run lifecycle state, not
// dashboard campaign pickup leases, and must survive campaign release.
func TestCampaignReleaseSpektacularRunHeldByStageIdentity(t *testing.T) {
	s, _ := runsTestServer(t)
	s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)
	key := "myorg/repo1!stable-spec-10060:spec"
	if err := s.contributeHub.recordLeaseForKeyStage(runAdmissionIdentity, "task-10059b", "myorg/repo1", 10060, key, "contributor", StageSpec, 1, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	before, ok := s.contributeHub.runLeaseHolder(key, time.Now())
	if !ok {
		t.Fatal("missing initial stage lease")
	}
	release := doOwnerPostAsUser(s, "/api/campaigns/stable-spec-10060/release", "owner", map[string]string{})
	if release.Code != http.StatusConflict {
		t.Fatalf("owner release of admitted run = %d body=%s", release.Code, release.Body.String())
	}
	after, ok := s.contributeHub.runLeaseHolder(key, time.Now())
	if !ok || after.identity != before.identity || after.taskID != before.taskID || after.stage != before.stage || after.gen != before.gen || !after.expiresAt.Equal(before.expiresAt) {
		t.Fatalf("release changed pending stage lease: before=%+v after=%+v held=%v", before, after, ok)
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
	if !resp.OK || resp.Campaign.ID != state.IdeaSlug || resp.Campaign.RevisionOf != "" || resp.Campaign.Revision == 0 || resp.Campaign.LeaseOwner != "bob" {
		t.Fatalf("revision campaign = %+v", resp.Campaign)
	}
}

func TestCampaignReviseTwiceAndContinueUsesStableCampaign(t *testing.T) {
	s := newMinimalServer(t)
	s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)
	state, err := s.deps.Inception.Start("what can I add to console to make it unique")
	if err != nil {
		t.Fatalf("start inception: %v", err)
	}
	if err := s.deps.Inception.SetQuestions([]knowledge.Question{{ID: "goal", Text: "Goal?"}}); err != nil {
		t.Fatalf("set questions: %v", err)
	}
	if reset := doOwnerPost(s, "/api/inception/reset", map[string]interface{}{}); reset.Code != http.StatusOK {
		t.Fatalf("reset = %d body=%s", reset.Code, reset.Body.String())
	}

	for i := 0; i < 2; i++ {
		revise := doOwnerPostAsUser(s, "/api/campaigns/"+state.IdeaSlug+"/revise", "alice", map[string]string{})
		if revise.Code != http.StatusOK {
			t.Fatalf("revise #%d = %d body=%s", i+1, revise.Code, revise.Body.String())
		}
	}
	list := doOwnerGet(s, "/api/campaigns?search=unique")
	if list.Code != http.StatusOK {
		t.Fatalf("campaigns list after revise = %d body=%s", list.Code, list.Body.String())
	}
	campaigns := decodeCampaignList(t, list.Body.Bytes())
	if len(campaigns) != 1 {
		t.Fatalf("campaigns after double revise len = %d, want 1: %+v", len(campaigns), campaigns)
	}
	if campaigns[0].ID != state.IdeaSlug || campaigns[0].CurrentStep != string(knowledge.PhaseCapture) {
		t.Fatalf("campaign after double revise = %+v, want stable id %q rewound to capture", campaigns[0], state.IdeaSlug)
	}

	resume := doOwnerPostAsUser(s, "/api/campaigns/"+state.IdeaSlug+"/resume", "alice", map[string]string{"surface": "dashboard"})
	if resume.Code != http.StatusOK {
		t.Fatalf("continue after revise = %d body=%s", resume.Code, resume.Body.String())
	}
	list = doOwnerGet(s, "/api/campaigns?search=unique")
	if list.Code != http.StatusOK {
		t.Fatalf("campaigns list after continue = %d body=%s", list.Code, list.Body.String())
	}
	campaigns = decodeCampaignList(t, list.Body.Bytes())
	if len(campaigns) != 1 || campaigns[0].ID != state.IdeaSlug {
		t.Fatalf("campaigns after continue = %+v, want one stable campaign %q", campaigns, state.IdeaSlug)
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
	if !resp.OK || resp.Campaign.ID != "stable-spec-8665" || resp.Campaign.RevisionOf != "" || resp.Campaign.Engine != "Spektacular" || resp.Campaign.Type != "spektacular" {
		t.Fatalf("spektacular revision = %+v", resp.Campaign)
	}
	if resp.Campaign.CurrentStage != StagePlan || !strings.Contains(spektacularResumeCommand(resp.Campaign), "spektacular plan status ") {
		t.Fatalf("spektacular revision resume shape = %+v command=%q", resp.Campaign, spektacularResumeCommand(resp.Campaign))
	}
}

func TestCampaignReleasePreservesSpektacularRunLease(t *testing.T) {
	for _, stage := range []string{StageSpec, StagePlan, StageImplement} {
		t.Run(stage, func(t *testing.T) {
			s, _ := runsTestServer(t)
			s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)
			leaseKey := "myorg/repo1!stable-spec-8665:" + stage
			if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-8665", "myorg/repo1", 8665, leaseKey, "contributor", stage, 3, time.Now()); err != nil {
				t.Fatalf("record lease: %v", err)
			}
			before, ok := s.contributeHub.runLeaseHolder(leaseKey, time.Now())
			if !ok {
				t.Fatal("missing initial stage lease")
			}
			list := doOwnerGet(s, "/api/campaigns")
			campaigns := decodeCampaignList(t, list.Body.Bytes())
			if len(campaigns) != 1 {
				t.Fatalf("initial campaigns = %+v, want one run-backed campaign", campaigns)
			}
			campaign := campaigns[0]

			// No timeline history: losing the stage lease would drop the run.
			for _, user := range []string{"alice", "bob"} {
				for _, id := range []string{campaign.ID, campaign.RunKey} {
					released := doOwnerPostAsUser(s, "/api/campaigns/"+url.PathEscape(id)+"/release", user, map[string]string{})
					after, ok := s.contributeHub.runLeaseHolder(leaseKey, time.Now())
					if !ok || after.identity != before.identity || after.taskID != before.taskID || after.stage != before.stage || after.gen != before.gen || !after.expiresAt.Equal(before.expiresAt) {
						t.Fatalf("release changed stage lease: before=%+v after=%+v held=%v", before, after, ok)
					}
					if released.Code != http.StatusConflict || !strings.Contains(released.Body.String(), "run-backed campaigns cannot be released here") {
						t.Fatalf("%s release %q = %d body=%s, want explicit conflict", user, id, released.Code, released.Body.String())
					}
					list = doOwnerGet(s, "/api/campaigns")
					campaigns = decodeCampaignList(t, list.Body.Bytes())
					if len(campaigns) != 1 || campaigns[0].ID != campaign.ID || campaigns[0].LeaseOwner != "alice" || campaigns[0].CurrentStage != stage {
						t.Fatalf("campaign disappeared or changed after release: %+v", campaigns)
					}
					runRec := doOwnerGet(s, "/api/runs/"+url.PathEscape(campaign.RunKey))
					if runRec.Code != http.StatusOK {
						t.Fatalf("run after release = %d body=%s", runRec.Code, runRec.Body.String())
					}
					var run Run
					if err := json.Unmarshal(runRec.Body.Bytes(), &run); err != nil {
						t.Fatalf("decode run: %v", err)
					}
					if run.Stage != stage || run.Assignee != "alice" || run.Gen != 3 {
						t.Fatalf("run changed after release: %+v", run)
					}
				}
			}
		})
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

// Issue-numbered runs ("owner/repo#N") have no embedded artifact slug: the id
// must be Spektacular's "owner-repo-n" artifact name, not the bare worksource
// key, or the generated resume command addresses an artifact that does not
// exist on disk (hivecommons/hive#10091).
func TestCampaignFromIssueRunUsesSpektacularArtifactName(t *testing.T) {
	campaign := campaignFromRun(Run{
		Key:   "myorg/repo#42",
		Repo:  "myorg/repo",
		Stage: StagePlan,
		State: "active",
	})
	if campaign.ID != "myorg-repo-42" {
		t.Fatalf("campaign id = %q, want spektacular artifact name", campaign.ID)
	}
	if campaign.RunKey != "myorg/repo#42" {
		t.Fatalf("campaign run key = %q, want worksource key preserved", campaign.RunKey)
	}
	if got := spektacularResumeCommand(campaign); got != "spektacular plan status myorg-repo-42" {
		t.Fatalf("resume command = %q, want artifact name", got)
	}
}
