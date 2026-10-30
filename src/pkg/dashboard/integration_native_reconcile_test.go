package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	"github.com/hivecommons/hive/pkg/github"
)

func TestIntegrationReconciliationRecoversMissedGroupAndBoundWorkflow(t *testing.T) {
	now := time.Now().UTC()
	base, tested, head, tree, blob := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("e", 40)
	server := orchestrationServer(t, config.IntegrationAdvisory)
	policy := config.IntegrationPolicy{Repo: "o/r", Base: "v5", Engine: "native", Workflow: ".github/workflows/group.yml", Blob: blob, AppID: 42, Jobs: []string{"policy"}}
	server.deps.Config.IntegrationOrchestration.Policies = []config.IntegrationPolicy{policy}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/o/r/pulls":
			_, _ = w.Write([]byte("[]"))
		case r.URL.Path == "/repos/o/r/actions/runs":
			_, _ = fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":10,"workflow_id":12,"check_suite_id":20,"run_attempt":1,"event":"merge_group","status":"completed","conclusion":"success","head_sha":%q,"head_branch":"gh-readonly-queue/v5/pr-7","head_repository":{"full_name":"o/r"},"repository":{"full_name":"o/r"},"path":%q,"created_at":%q,"updated_at":%q}]}`, tested, policy.Workflow, now.Add(-15*time.Minute).Format(time.RFC3339), now.Format(time.RFC3339))
		case r.URL.Path == "/graphql":
			_, _ = fmt.Fprintf(w, `{"data":{"repository":{"mergeQueue":{"entries":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"entry-7","position":1,"baseCommit":{"oid":%q},"headCommit":{"oid":%q},"pullRequest":{"number":7,"headRefOid":%q,"baseRefName":"v5"}}]}}}}}`, base, tested, head)
		case strings.Contains(r.URL.Path, "/git/commits/"):
			_, _ = fmt.Fprintf(w, `{"sha":%q,"tree":{"sha":%q}}`, tested, tree)
		case strings.Contains(r.URL.Path, "/git/ref/"):
			_, _ = fmt.Fprintf(w, `{"object":{"sha":%q}}`, tested)
		case strings.Contains(r.URL.Path, "/contents/"):
			_, _ = fmt.Fprintf(w, `{"type":"file","sha":%q}`, blob)
		case strings.Contains(r.URL.Path, "/attempts/1/jobs"):
			_, _ = fmt.Fprintf(w, `{"total_count":1,"jobs":[{"id":30,"run_id":10,"name":"policy","head_sha":%q,"runner_id":50,"status":"completed","conclusion":"success","check_run_url":"https://api.github.com/repos/o/r/check-runs/40","started_at":%q,"completed_at":%q}]}`, tested, now.Add(-10*time.Minute).Format(time.RFC3339), now.Add(-5*time.Minute).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			_, _ = fmt.Fprintf(w, `{"check_runs":[{"id":40,"name":"policy","head_sha":%q,"status":"completed","conclusion":"success","app":{"id":42},"check_suite":{"id":20}}]}`, tested)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()
	server.deps.GHClient = github.NewClient("token", "o", []string{"r"}, nil, api.URL)
	if err := server.ReconcileIntegration(context.Background()); err != nil {
		t.Fatal(err)
	}
	store, err := server.integrationStore()
	if err != nil {
		t.Fatal(err)
	}
	receipts := store.ForTestedHead("o/r", tested)
	if len(receipts) != 1 || receipts[0].State != integration.Passed || receipts[0].Subject.Policy != policy.Fingerprint() ||
		receipts[0].Subject.TestedTree != tree || len(receipts[0].Subject.Members) != 1 || receipts[0].Subject.Members[0].Head != head {
		t.Fatalf("missed group did not recover exact bound evidence: %+v", receipts)
	}
	snapshot := store.Snapshot(now)
	linked := false
	if len(snapshot.Jobs) == 1 {
		for _, candidate := range snapshot.Jobs[0].Candidates {
			if candidate == receipts[0].Subject.ID() {
				linked = true
			}
		}
	}
	if !linked {
		t.Fatalf("physical job uncorrelated: %+v", snapshot.Jobs)
	}
	if err := store.SetPolicies(nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := store.ForTestedHead("o/r", tested)[0]; got.State != integration.Superseded {
		t.Fatal("removed producer policy retained proof")
	}
}
