package dashboard

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

func TestCampaignReviseIssueRunsPreservesIdentityAndStageLease(t *testing.T) {
	s, deps := runsTestServer(t)
	deps.Config.Runs.Spektacular.Enabled = true
	disableSpekHubExecutorForRelayTests(s)
	s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)
	runs := []struct {
		repo  string
		issue int
		key   string
	}{
		{"myorg/repo1", 8450, "myorg/repo1#8450"},
		{"myorg/repo", 18450, "myorg/repo#18450"},
	}
	for _, run := range runs {
		if err := s.AdmitTriagedRun(run.repo, run.issue, run.key, "spec", "feature", time.Now()); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			rec := doOwnerPostAsUser(s, "/api/campaigns/"+url.PathEscape(run.key)+"/revise", "alice", map[string]string{})
			if rec.Code != http.StatusOK {
				t.Fatalf("revise %q = %d: %s", run.key, rec.Code, rec.Body.String())
			}
			var response campaignReviseResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Campaign.ID != run.key || response.Campaign.Revision != 1 {
				t.Fatalf("revision = %+v", response.Campaign)
			}
		}
	}
	list := doOwnerGet(s, "/api/campaigns")
	if list.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", list.Code, list.Body.String())
	}
	campaigns := decodeCampaignList(t, list.Body.Bytes())
	if len(campaigns) != 2 {
		t.Fatalf("phantom campaigns: %+v", campaigns)
	}
	for _, campaign := range campaigns {
		if campaign.Revision != 1 || campaign.LeaseOwner != "alice" || campaign.CurrentStage != StageSpec {
			t.Fatalf("live revision = %+v", campaign)
		}
		resume := doOwnerPostAsUser(s, "/api/campaigns/"+url.PathEscape(campaign.ID)+"/resume", "alice", map[string]string{"surface": "cli"})
		if resume.Code != http.StatusOK || !strings.Contains(resume.Body.String(), "spektacular spec status "+campaignArtifactID(campaign.ID)) {
			t.Fatalf("resume = %d: %s", resume.Code, resume.Body.String())
		}
		archive, err := s.deps.Inception.LoadCampaignArchive(campaign.ID)
		if err != nil || archive.Source != campaign.RunKey {
			t.Fatalf("archive = %+v, err = %v", archive, err)
		}
		held, ok := s.contributeHub.runLeaseHolder(campaign.RunKey, time.Now())
		if !ok {
			t.Fatalf("stage lease missing for %q", campaign.RunKey)
		}
		blocked := doOwnerPostAsUser(s, "/api/campaigns/"+url.PathEscape(campaign.ID)+"/release", "bob", map[string]string{})
		if blocked.Code != http.StatusConflict {
			t.Fatalf("bob release = %d: %s", blocked.Code, blocked.Body.String())
		}
		released := doOwnerPostAsUser(s, "/api/campaigns/"+url.PathEscape(campaign.ID)+"/release", "alice", map[string]string{})
		if released.Code != http.StatusOK {
			t.Fatalf("alice release = %d: %s", released.Code, released.Body.String())
		}
		archive, err = s.deps.Inception.LoadCampaignArchive(campaign.ID)
		if err != nil || archive.Lease != nil {
			t.Fatalf("revise lease not released: %+v, err = %v", archive, err)
		}
		// Once the archive lease is cleared, another campaign release reports
		// "no active lease" (409, ErrCampaignNoLease) and must not fall through
		// to revoking the still-live stage lease.
		again := doOwnerPostAsUser(s, "/api/campaigns/"+url.PathEscape(campaign.ID)+"/release", "alice", map[string]string{})
		if again.Code != http.StatusConflict {
			t.Fatalf("second release = %d: %s", again.Code, again.Body.String())
		}
		after, ok := s.contributeHub.runLeaseHolder(campaign.RunKey, time.Now())
		if !ok || after.identity != held.identity || after.taskID != held.taskID {
			t.Fatalf("release changed stage lease for %q", campaign.RunKey)
		}
	}
}
