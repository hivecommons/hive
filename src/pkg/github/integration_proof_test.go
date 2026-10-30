package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

func TestIntegrationWorkflowProducerBinding(t *testing.T) {
	for _, mode := range []string{"passed", "rejected", "running", "wrong-app", "wrong-suite", "wrong-head", "wrong-blob", "untrusted-base", "missing-job", "ambiguous-job", "wrong-event", "wrong-repo", "wrong-target", "wrong-path", "read-error", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			sha, blob := strings.Repeat("a", 40), strings.Repeat("b", 40)
			policy := config.IntegrationPolicy{Repo: "o/r", Base: "v5", Engine: "native", Workflow: ".github/workflows/group.yml", Blob: blob, AppID: 42, Jobs: []string{"policy"}}
			run := &gh.WorkflowRun{ID: gh.Ptr(int64(1)), WorkflowID: gh.Ptr(int64(12)), CheckSuiteID: gh.Ptr(int64(2)), RunAttempt: gh.Ptr(1), Event: gh.Ptr("merge_group"),
				Path: gh.Ptr(policy.Workflow), HeadSHA: gh.Ptr(sha), HeadRepository: &gh.Repository{FullName: gh.Ptr("o/r")}, Repository: &gh.Repository{FullName: gh.Ptr("o/r")}, Status: gh.Ptr("completed"), Conclusion: gh.Ptr("success")}
			if mode == "wrong-event" {
				run.Event = gh.Ptr("pull_request_target")
			}
			if mode == "wrong-repo" {
				run.HeadRepository.FullName = gh.Ptr("attacker/r")
			}
			if mode == "wrong-target" {
				run.Repository.FullName = gh.Ptr("attacker/r")
			}
			if mode == "wrong-path" {
				run.Path = gh.Ptr(".github/workflows/other.yml")
			}
			if mode == "running" {
				run.Status = gh.Ptr("in_progress")
			}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if mode == "read-error" {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				if mode == "truncated" {
					w.Header().Set("Link", `<http://example.invalid?page=2>; rel="next"`)
				}
				switch {
				case strings.Contains(r.URL.Path, "/contents/"):
					if mode == "wrong-blob" || mode == "untrusted-base" && r.URL.Query().Get("ref") == policy.Base {
						blob = strings.Repeat("f", 40)
					}
					_, _ = fmt.Fprintf(w, `{"type":"file","sha":%q}`, blob)
				case strings.Contains(r.URL.Path, "/jobs"):
					job := fmt.Sprintf(`{"id":3,"run_id":1,"name":"policy","head_sha":%q,"check_run_url":"https://api.github.com/repos/o/r/check-runs/4","status":"completed","conclusion":"success"}`, sha)
					if mode == "missing-job" {
						_, _ = w.Write([]byte(`{"jobs":[]}`))
						return
					}
					if mode == "ambiguous-job" {
						job = job + "," + job
					}
					_, _ = fmt.Fprintf(w, `{"jobs":[%s]}`, job)
				case strings.Contains(r.URL.Path, "/check-runs"):
					app, suite, conclusion := 42, 2, "success"
					if mode == "wrong-app" {
						app = 99
					}
					if mode == "wrong-suite" {
						suite = 9
					}
					if mode == "wrong-head" {
						sha = strings.Repeat("f", 40)
					}
					if mode == "rejected" {
						conclusion = "failure"
					}
					_, _ = fmt.Fprintf(w, `{"check_runs":[{"id":4,"name":"policy","head_sha":%q,"app":{"id":%d},"check_suite":{"id":%d},"status":"completed","conclusion":%q}]}`, sha, app, suite, conclusion)
				default:
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer api.Close()
			client := NewClient("token", "o", []string{"r"}, nil, api.URL)
			proof, err := client.ReadIntegrationWorkflowProof(context.Background(), policy, run)
			switch mode {
			case "passed":
				if err != nil || !proof.Passed || proof.RunID != 1 || proof.SuiteID != 2 || len(proof.CheckIDs) != 1 {
					t.Fatalf("%+v %v", proof, err)
				}
			case "rejected":
				if err != nil || !proof.Rejected || proof.Passed {
					t.Fatalf("%+v %v", proof, err)
				}
			case "running":
				if err != nil || proof.Passed || proof.Rejected {
					t.Fatalf("%+v %v", proof, err)
				}
			default:
				if err == nil || proof.Passed {
					t.Fatalf("unbound proof accepted: %+v %v", proof, err)
				}
			}
		})
	}
	client := NewClient("token", "o", []string{"r"}, nil, "https://example.invalid")
	if _, err := client.ReadIntegrationWorkflowProof(context.Background(), config.IntegrationPolicy{}, nil); err == nil {
		t.Fatal("missing identity accepted")
	}
}
