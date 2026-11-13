package dashboard

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// hivecommons/hive#18021: /api/status lists project.reference_repos apart
// from the managed repos, each with a reference_tier, and with none of the
// ACMM, work-item or pause fields a managed repo row carries.
func TestStatusReferenceReposCarryTierAndNoManagedFields(t *testing.T) {
	srv := newFullServer(t)
	cfg := srv.deps.Config
	cfg.Project.ReferenceRepos = []config.ReferenceRepo{
		{Repo: "hivecommons/docs"},
		{Repo: "hivecommons/hive", Write: []string{config.ReferenceWriteIssues, config.ReferenceWritePRs}},
	}

	// Config validation rejects a repo in both project.repos and
	// project.reference_repos, so a reference repo can only leak if the
	// builders start reading reference_repos. Pin the exact managed set
	// (newFullServer's testorg/testrepo) so that regression fails here.
	wantManaged := []string{"testorg/testrepo"}

	repos := buildRepos(cfg, nil, governor.State{})
	var managed []string
	for _, r := range repos {
		managed = append(managed, r.Full)
	}
	if !slices.Equal(managed, wantManaged) {
		t.Fatalf("managed repos list = %v, want exactly %v (reference repos must not appear)", managed, wantManaged)
	}
	srv.UpdateStatus(&StatusPayload{Repos: repos, ReferenceRepos: buildReferenceRepos(cfg)})

	if got := srv.contributeAvailableRepos(); !slices.Equal(got, wantManaged) {
		t.Fatalf("contribute enable/disable list = %v, want exactly %v (reference repos must not be offered)", got, wantManaged)
	}

	rec := doGet(srv, "/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/status = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ReferenceRepos []map[string]any `json:"referenceRepos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /api/status: %v", err)
	}
	if len(body.ReferenceRepos) != 2 {
		t.Fatalf("referenceRepos = %v, want both entries", body.ReferenceRepos)
	}

	want := map[string]string{"hivecommons/docs": "read-only", "hivecommons/hive": "issues+prs"}
	managedOnly := []string{
		"issues", "prs", "mode", "paused", "acmmLevel", "acmmLevelConfigured",
		"acmmPackAgents", "acmmAdvice", "sla", "countsIncomplete", "countsStale",
	}
	for _, rr := range body.ReferenceRepos {
		full, _ := rr["full"].(string)
		tier, ok := want[full]
		if !ok {
			t.Fatalf("unexpected reference entry %v", rr)
		}
		if got := rr["reference_tier"]; got != tier {
			t.Errorf("%s reference_tier = %v, want %q", full, got, tier)
		}
		if got := rr["tier"]; got != tier {
			t.Errorf("%s tier = %v, want %q (kept for existing consumers)", full, got, tier)
		}
		for _, k := range managedOnly {
			if _, present := rr[k]; present {
				t.Errorf("%s carries managed-repo field %q: %v", full, k, rr)
			}
		}
	}
}
