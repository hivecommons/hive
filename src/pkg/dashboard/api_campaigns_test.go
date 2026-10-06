package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/timeline"
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

const recheckTestRunKey = "myorg/repo1#8665"

func recheckTestServer(t *testing.T) *Server {
	t.Helper()
	s, deps := runsTestServer(t)
	deps.Config.Runs.Spektacular = config.SpektacularConfig{Enabled: true}
	disableSpekHubExecutorForRelayTests(s)
	s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)
	old := runReceiptsDir
	runReceiptsDir = filepath.Join(t.TempDir(), "receipts")
	t.Cleanup(func() { runReceiptsDir = old })
	return s
}

func recordCompletedRecheckRun(s *Server, gen uint64, at time.Time) {
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: recheckTestRunKey, Kind: timeline.KindStageCompleted, At: at.UnixMilli(),
		Attrs: map[string]string{"stage_from": StageImplement, "stage_to": "completed", "gen": strconv.FormatUint(gen, 10), "title": "Converge widgets"},
	})
}

func recheckSpecLeaseKey() string {
	return leaseKey(runAdmissionIdentity, runAdmissionTaskPrefix+sanitizeReceiptSegment(recheckTestRunKey))
}

func recheckSpecLease(t *testing.T, s *Server) taskLease {
	t.Helper()
	s.contributeHub.leaseMu.Lock()
	defer s.contributeHub.leaseMu.Unlock()
	for _, l := range s.contributeHub.leases {
		if l != nil && strings.HasPrefix(l.taskID, "spek-recheck-") {
			t.Fatalf("recheck minted a second spek-recheck lease: %+v", *l)
		}
	}
	l := s.contributeHub.leases[recheckSpecLeaseKey()]
	if l == nil {
		t.Fatal("rewound generation has no spec lease on the run's own admission key")
	}
	return *l
}

func dropRecheckSpecLease(s *Server) {
	s.contributeHub.leaseMu.Lock()
	defer s.contributeHub.leaseMu.Unlock()
	delete(s.contributeHub.leases, recheckSpecLeaseKey())
}

func recheckCampaign(t *testing.T, s *Server) Campaign {
	t.Helper()
	list := doOwnerGet(s, "/api/campaigns")
	if list.Code != http.StatusOK {
		t.Fatalf("campaigns list = %d body=%s", list.Code, list.Body.String())
	}
	campaigns := decodeCampaignList(t, list.Body.Bytes())
	if len(campaigns) != 1 || campaigns[0].ID != recheckTestRunKey {
		t.Fatalf("campaigns = %+v, want one card for %q", campaigns, recheckTestRunKey)
	}
	return campaigns[0]
}

func TestCampaignRecheckManualForceAndConflict(t *testing.T) {
	s := recheckTestServer(t)
	recordCompletedRecheckRun(s, 3, time.Now().Add(-time.Hour))
	path := "/api/campaigns/" + url.PathEscape(recheckTestRunKey) + "/recheck"

	disabled := doOwnerPostAsUser(s, path, "bob", map[string]string{})
	if disabled.Code != http.StatusNotFound {
		t.Fatalf("disabled recheck = %d body=%s, want 404", disabled.Code, disabled.Body.String())
	}
	before := recheckCampaign(t, s)
	if before.CurrentStage != "completed" || before.Recheck == nil || before.Recheck.InFlight {
		t.Fatalf("campaign before recheck = %+v", before)
	}
	forced := doOwnerPostAsUser(s, path+"?force=true", "bob", map[string]string{})
	if forced.Code != http.StatusOK {
		t.Fatalf("forced recheck = %d body=%s", forced.Code, forced.Body.String())
	}
	var resp campaignRecheckResponse
	if err := json.Unmarshal(forced.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode forced recheck: %v", err)
	}
	if !resp.OK || resp.Campaign.ID != recheckTestRunKey || resp.Campaign.Revision != before.Revision+1 || resp.Campaign.RevisionOf != "" ||
		resp.Campaign.CurrentStage != StageSpec || resp.Campaign.Drift == nil || resp.Campaign.Drift.RecheckReason != recheckReasonManual ||
		resp.Campaign.Drift.PriorRevision != strconv.Itoa(before.Revision) || resp.Campaign.Recheck == nil || !resp.Campaign.Recheck.InFlight {
		t.Fatalf("forced recheck campaign = %+v", resp.Campaign)
	}
	archive, err := s.deps.Inception.LoadCampaignArchive(recheckTestRunKey)
	if err != nil {
		t.Fatalf("load archive: %v", err)
	}
	if archive.Revision != before.Revision+1 || archive.Lease != nil || archive.Source != recheckTestRunKey || len(archive.History) != 1 {
		t.Fatalf("archive after rewind = %+v", archive)
	}
	if entry := archive.History[0]; entry.Revision != before.Revision || entry.LastGen != 3 || entry.Reason != recheckReasonManual || entry.Actor != "bob" || entry.Drift == nil {
		t.Fatalf("generation log entry = %+v", entry)
	}
	lease := recheckSpecLease(t, s)
	if lease.stage != StageSpec || lease.gen <= 3 || lease.key != "myorg/repo1!"+recheckTestRunKey+":"+StageSpec || runKeyOfLease(lease.key, lease.repo) != recheckTestRunKey {
		t.Fatalf("rewound spec lease = %+v", lease)
	}
	listed := recheckCampaign(t, s)
	if listed.Revision != before.Revision+1 || listed.CurrentStage != StageSpec || listed.Recheck == nil || !listed.Recheck.InFlight || len(listed.History) != 1 {
		t.Fatalf("listed campaign after rewind = %+v", listed)
	}

	conflict := doOwnerPostAsUser(s, path+"?force=true", "bob", map[string]string{})
	if conflict.Code != http.StatusConflict {
		t.Fatalf("second recheck = %d body=%s, want 409", conflict.Code, conflict.Body.String())
	}

	// The closed generation's completion never completes the rewound one.
	dropRecheckSpecLease(s)
	stale := recheckCampaign(t, s)
	if stale.CurrentStage != StageSpec || stale.Drift == nil || !stale.Recheck.InFlight {
		t.Fatalf("prior generation completion leaked into rewound generation: %+v", stale)
	}
	if again := doOwnerPostAsUser(s, path+"?force=true", "bob", map[string]string{}); again.Code != http.StatusConflict {
		t.Fatalf("recheck while drift in flight = %d body=%s, want 409", again.Code, again.Body.String())
	}
	if archive, err := s.deps.Inception.LoadCampaignArchive(recheckTestRunKey); err != nil || archive.Revision != before.Revision+1 || len(archive.History) != 1 {
		t.Fatalf("refused recheck changed archive = %+v err=%v", archive, err)
	}
}

func TestCampaignRecheckRefusesLiveStageLease(t *testing.T) {
	s := recheckTestServer(t)
	if err := s.AdmitTriagedRun("myorg/repo1", 8665, "Converge widgets", "spec", "feature", time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := doOwnerPostAsUser(s, "/api/campaigns/"+url.PathEscape(recheckTestRunKey)+"/recheck?force=true", "bob", map[string]string{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("recheck during live stage = %d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if _, err := s.deps.Inception.LoadCampaignArchive(recheckTestRunKey); err == nil {
		t.Fatal("refused recheck wrote a campaign archive")
	}
}

func TestCampaignRecheckCadenceTicksWithInjectedClock(t *testing.T) {
	s := recheckTestServer(t)
	s.deps.Config.Runs.Spektacular.Recheck.Enabled = true
	s.deps.Config.Runs.Spektacular.Recheck.DefaultInterval = time.Hour
	completedAt := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	recordCompletedRecheckRun(s, 3, completedAt)

	s.TickCampaignRechecks(context.Background(), completedAt.Add(30*time.Minute))
	if _, err := s.deps.Inception.LoadCampaignArchive(recheckTestRunKey); err == nil {
		t.Fatal("tick before next_at rewound the campaign")
	}

	first := completedAt.Add(2 * time.Hour)
	s.TickCampaignRechecks(context.Background(), first)
	archive, err := s.deps.Inception.LoadCampaignArchive(recheckTestRunKey)
	if err != nil || archive.Revision != 1 || archive.Drift == nil || archive.Drift.RecheckReason != recheckReasonCadence || len(archive.History) != 1 || archive.History[0].Actor != "system" {
		t.Fatalf("archive after cadence tick = %+v err=%v", archive, err)
	}
	lease := recheckSpecLease(t, s)

	s.TickCampaignRechecks(context.Background(), first.Add(3*time.Hour))
	if archive, err := s.deps.Inception.LoadCampaignArchive(recheckTestRunKey); err != nil || archive.Revision != 1 {
		t.Fatalf("tick while in flight rewound again: %+v err=%v", archive, err)
	}

	// The rewound generation completes: Drift clears and in-flight ends.
	dropRecheckSpecLease(s)
	recordCompletedRecheckRun(s, lease.gen, completedAt.Add(time.Minute))
	converged := recheckCampaign(t, s)
	if converged.CurrentStage != "completed" || converged.Drift != nil || converged.Recheck == nil || converged.Recheck.InFlight || converged.Revision != 1 {
		t.Fatalf("converged campaign = %+v", converged)
	}
	if archive, err := s.deps.Inception.LoadCampaignArchive(recheckTestRunKey); err != nil || archive.Drift != nil {
		t.Fatalf("converged archive kept drift: %+v err=%v", archive, err)
	}

	s.TickCampaignRechecks(context.Background(), first.Add(2*time.Hour))
	archive, err = s.deps.Inception.LoadCampaignArchive(recheckTestRunKey)
	if err != nil || archive.Revision != 2 || len(archive.History) != 2 || archive.History[1].LastGen != lease.gen || archive.Drift == nil || archive.Drift.PriorRevision != "1" {
		t.Fatalf("archive after second cadence tick = %+v err=%v", archive, err)
	}
	if next := recheckSpecLease(t, s); next.gen <= lease.gen {
		t.Fatalf("second generation gen = %d, want past %d", next.gen, lease.gen)
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

// Issue-numbered campaigns retain their worksource identity, while resume
// commands address Spektacular's artifact name rather than the worksource key.
func TestCampaignFromIssueRunSeparatesIdentityFromArtifactName(t *testing.T) {
	campaign := campaignFromRun(Run{
		Key:   "myorg/repo#42",
		Repo:  "myorg/repo",
		Stage: StagePlan,
		State: "active",
	})
	if campaign.ID != "myorg/repo#42" {
		t.Fatalf("campaign id = %q, want lossless worksource identity", campaign.ID)
	}
	if campaign.RunKey != "myorg/repo#42" {
		t.Fatalf("campaign run key = %q, want worksource key preserved", campaign.RunKey)
	}
	if got := spektacularResumeCommand(campaign); got != "spektacular plan status myorg-repo-42" {
		t.Fatalf("resume command = %q, want artifact name", got)
	}
}
