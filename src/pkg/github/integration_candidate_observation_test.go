package github

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/forgestate"
)

func TestIntegrationCandidateObserverBindsActualRunJobCheckArtifactAndOrderedSubject(t *testing.T) {
	for _, scenario := range []string{"passed", "rejected", "latest_attempt", "changed_during_read", "wrong_app", "wrong_tree", "missing_job", "digest", "unknown_job_cohort", "wrong_receipt_subject", "materialized", "materialized_partial", "materialized_wrong_closure", "v2_passed", "v2_missing", "v2_context_app", "v2_metadata"} {
		t.Run(scenario, func(t *testing.T) {
			key, binding, claims := producerIdentityFixture(t)
			subject := forgestate.IntegrationCandidateSubject{
				Schema: 1, Protocol: "integration-prefix-v1", Engine: "hive", Repo: binding.Repo,
				Base: "v5", BaseSHA: strings.Repeat("1", 40), Policy: strings.Repeat("c", 64),
				WorkflowTree: strings.Repeat("d", 40), Generation: 1, ID: binding.SubjectID,
				Members:  []forgestate.IntegrationCandidateMember{{PR: 10, Head: strings.Repeat("2", 40)}},
				Prefixes: []forgestate.IntegrationCandidatePrefix{{Commit: binding.WorkflowSHA, Tree: strings.Repeat("3", 40)}},
			}
			expected := IntegrationCandidateExpectation{Subject: subject, RepositoryID: 1, ActorID: 2,
				WorkflowID: 7, CheckAppID: 9, RunID: 3, Attempt: 1, RequiredJobs: []string{"ci / build-and-test"}}
			materializer := strings.HasPrefix(scenario, "materialized")
			names := []string{"ci / build-and-test", integrationCandidateFinalizer}
			v2 := strings.HasPrefix(scenario, "v2_")
			memberPR := &gh.PullRequest{Number: gh.Int(10), State: gh.String("open"), Title: gh.String("✨ feature"),
				User: &gh.User{ID: gh.Int64(42), Login: gh.String("author")},
				Base: &gh.PullRequestBranch{Ref: gh.String("v5"), Repo: &gh.Repository{FullName: gh.String(subject.Repo)}},
				Head: &gh.PullRequestBranch{SHA: gh.String(subject.Members[0].Head), Ref: gh.String("feature"), Repo: &gh.Repository{FullName: gh.String(subject.Repo)}}}
			if v2 {
				expected.RequiredJobs = forgestate.IntegrationRequiredContexts()
				names = append(forgestate.IntegrationRequiredContexts(), integrationCandidateFinalizer)
			}
			workflowPath, event, artifactPrefix, filename := integrationCandidateWorkflow, "workflow_dispatch", integrationReceiptArtifact, integrationReceiptFile
			preparation := IntegrationPreparationRequest{
				Schema: 1, Protocol: subject.Protocol, Engine: subject.Engine, Repo: subject.Repo,
				Base: subject.Base, BaseSHA: subject.BaseSHA, Policy: subject.Policy, Generation: 1,
				Members: subject.Members, CreatedAt: binding.JobStartedAt.Format("2006-01-02T15:04:05.000Z"),
			}
			receiptSubject := subject
			if scenario == "wrong_receipt_subject" {
				receiptSubject.BaseSHA = strings.Repeat("4", 40)
			}
			payload, err := json.Marshal(map[string]any{
				"schema": 1, "protocol": "integration-producer-receipt-v1", "subject": receiptSubject,
				"runID": "3", "runAttempt": "1", "outcomes": map[string]string{"ci": "success", "tests": "success", "security": "success"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if v2 {
				contexts := map[string]forgestate.IntegrationContextReceipt{}
				for i, name := range forgestate.IntegrationRequiredContexts() {
					contexts[name] = forgestate.IntegrationContextReceipt{JobID: int64(80 + i), CheckID: int64(100 + i), AppID: 9, SuiteID: 8, Conclusion: "success"}
				}
				if scenario == "v2_missing" {
					delete(contexts, "DCO")
				}
				if scenario == "v2_context_app" {
					record := contexts["DCO"]
					record.AppID++
					contexts["DCO"] = record
				}
				row, rowErr := json.Marshal([][]any{integrationGovernanceRow(memberPR)})
				if rowErr != nil {
					t.Fatal(rowErr)
				}
				payload, err = json.Marshal(map[string]any{"schema": 2, "protocol": "integration-producer-receipt-v2", "subject": subject,
					"runID": "3", "runAttempt": "1", "metadataDigest": integrationGovernanceHash(string(row)), "contexts": contexts})
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "v2_metadata" {
					memberPR.Body = gh.String("changed after signed governance")
				}
			}
			if materializer {
				workflowPath, event = integrationMaterializerWorkflow, "repository_dispatch"
				artifactPrefix, filename = integrationMaterializerArtifact, "materialization.json"
				names = []string{integrationMaterializerJob, integrationMaterializerFinalizer}
				source := forgestate.IntegrationPreparationSource{SourceSHA: binding.WorkflowSHA, RequestDigest: binding.SubjectID, GitVersion: "fixture"}
				subject.PreparedBy = source
				prepared := IntegrationPreparedSubject{Subject: subject, Publication: "accepted", Dispatch: "accepted"}
				body := IntegrationMaterializationReceipt{Schema: 1, Protocol: "integration-materializer-receipt-v1",
					Request: preparation, PreparedBy: source, Prepared: []IntegrationPreparedSubject{prepared}}
				if scenario == "materialized_partial" {
					body.Prepared[0].Publication, body.Prepared[0].Dispatch = "unresolved", "not_requested"
					body.Failure = &IntegrationPreparationFailure{Reason: "provider_unknown"}
				}
				payload, err = json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				claims["workflow_ref"] = binding.Repo + "/" + workflowPath + "@refs/heads/v5"
				claims["ref"], claims["ref_type"], claims["event_name"] = "refs/heads/v5", "branch", event
			}
			binding.PayloadSHA256 = fmt.Sprintf("%x", sha256.Sum256(payload))
			claims["aud"] = integrationProducerAudience + binding.PayloadSHA256
			if materializer {
				claims["aud"] = integrationMaterializerAudience + binding.PayloadSHA256
			}
			envelope, err := json.Marshal(integrationReceiptEnvelope{Schema: 1,
				Payload: base64.StdEncoding.EncodeToString(payload), Token: signProducerToken(t, key, claims)})
			if err != nil {
				t.Fatal(err)
			}
			var buffer bytes.Buffer
			archive := zip.NewWriter(&buffer)
			file, err := archive.Create(filename)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(envelope); err != nil {
				t.Fatal(err)
			}
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}
			digest := fmt.Sprintf("sha256:%x", sha256.Sum256(buffer.Bytes()))
			if scenario == "digest" {
				digest = "sha256:" + strings.Repeat("f", 64)
			}
			reads := 0
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				emit := func(value any) {
					t.Helper()
					if err := json.NewEncoder(w).Encode(value); err != nil {
						t.Error(err)
					}
				}
				switch r.URL.Path {
				case "/repos/owner/repo/actions/runs/3":
					reads++
					attempt := 1
					if scenario == "latest_attempt" || scenario == "changed_during_read" && reads > 1 {
						attempt = 2
					}
					conclusion := "success"
					if scenario == "rejected" {
						conclusion = "failure"
					}
					emit(map[string]any{"id": 3, "run_attempt": attempt, "workflow_id": 7, "check_suite_id": 8,
						"event": event, "path": workflowPath,
						"head_sha": binding.WorkflowSHA, "actor": map[string]int{"id": 2},
						"repository":      map[string]any{"id": 1, "full_name": "owner/repo"},
						"head_repository": map[string]string{"full_name": "owner/repo"},
						"status":          "completed", "conclusion": conclusion})
				case "/repos/owner/repo/actions/runs/3/attempts/1/jobs":
					jobs := []map[string]any{}
					for i, name := range names {
						if scenario == "missing_job" && i == 0 {
							continue
						}
						checkID := 5
						if i == 1 {
							checkID = 4
						}
						if v2 {
							checkID = 100 + i
							if i == len(names)-1 {
								checkID = 4
							}
						}
						result := "success"
						if scenario == "rejected" && i == 0 {
							result = "failure"
						}
						jobs = append(jobs, map[string]any{"id": 80 + i, "run_id": 3, "head_sha": binding.WorkflowSHA,
							"name": name, "runner_id": 100 + i, "status": "completed", "conclusion": result,
							"started_at": binding.JobStartedAt, "completed_at": binding.JobCompletedAt,
							"check_run_url": fmt.Sprintf("%s/repos/owner/repo/check-runs/%d", server.URL, checkID)})
					}
					count := len(jobs)
					if scenario == "unknown_job_cohort" {
						count++
					}
					emit(map[string]any{"total_count": count, "jobs": jobs})
				case "/repos/owner/repo/commits/" + binding.WorkflowSHA + "/check-runs":
					app := 9
					if scenario == "wrong_app" {
						app = 10
					}
					checks := []map[string]any{}
					for i, name := range names {
						id, result := 5, "success"
						if i == 1 {
							id = 4
						}
						if v2 {
							id = 100 + i
							if i == len(names)-1 {
								id = 4
							}
						}
						if scenario == "rejected" && i == 0 {
							result = "failure"
						}
						checks = append(checks, map[string]any{"id": id, "name": name, "head_sha": binding.WorkflowSHA,
							"status": "completed", "conclusion": result, "check_suite": map[string]int{"id": 8}, "app": map[string]int{"id": app}})
					}
					emit(map[string]any{"total_count": len(checks), "check_runs": checks})
				case "/repos/owner/repo/pulls/10":
					emit(memberPR)
				case "/repos/owner/repo/actions/runs/3/artifacts":
					emit(map[string]any{"total_count": 1, "artifacts": []any{map[string]any{"id": 10,
						"name": artifactPrefix + "1", "size_in_bytes": buffer.Len(), "digest": digest,
						"workflow_run": map[string]any{"id": 3, "head_sha": binding.WorkflowSHA}}}})
				case "/repos/owner/repo/actions/artifacts/10/zip":
					w.Header().Set("Location", server.URL+"/asset")
					w.WriteHeader(http.StatusFound)
				case "/asset":
					if r.Header.Get("Authorization") != "" {
						t.Error("forge credential leaked to artifact storage")
					}
					if _, err := w.Write(buffer.Bytes()); err != nil {
						t.Error(err)
					}
				case "/graphql":
					tree := subject.Prefixes[0].Tree
					closure := subject.WorkflowTree
					if scenario == "materialized_wrong_closure" {
						closure = strings.Repeat("e", 40)
					}
					if scenario == "wrong_tree" {
						tree = strings.Repeat("4", 40)
					}
					emit(map[string]any{"data": map[string]any{"repository": map[string]any{
						"nameWithOwner": "owner/repo", "closure": map[string]string{"oid": closure},
						"prefix0": map[string]any{"oid": binding.WorkflowSHA, "tree": map[string]string{"oid": tree},
							"parents": map[string]any{"nodes": []any{map[string]string{"oid": subject.BaseSHA}, map[string]string{"oid": subject.Members[0].Head}}}},
					}}})
				default:
					t.Errorf("unexpected provider request %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := gh.NewClient(server.Client())
			client.BaseURL, err = url.Parse(server.URL + "/")
			if err != nil {
				t.Fatal(err)
			}
			observer := &Client{client: client}
			verifier := NewIntegrationProducerVerifier()
			verifier.keys, verifier.refreshedAt = map[string]*rsa.PublicKey{"github-key": &key.PublicKey}, time.Now()
			if materializer {
				result, err := observer.ReadIntegrationMaterializerObservation(context.Background(), IntegrationMaterializerExpectation{
					Request: preparation, RequestDigest: binding.SubjectID, DefaultBranch: "v5", EventName: event,
					WorkflowTree: subject.WorkflowTree, RepositoryID: 1, ActorID: 2, WorkflowID: 7, CheckAppID: 9, RunID: 3, Attempt: 1,
				}, verifier, binding.ObservedAt)
				if scenario == "materialized_wrong_closure" {
					if err == nil || result.Evidence.State != "unknown" {
						t.Fatalf("untrusted control closure accepted: %+v %v", result, err)
					}
					return
				}
				if err != nil || result.Evidence.State != "passed" || len(result.Receipt.Prepared) != 1 ||
					result.Evidence.Identity.Role != "materializer" || result.Evidence.Identity.FinalizerCheckRunID != 4 {
					t.Fatalf("materialization provenance: %+v %v", result, err)
				}
				if scenario == "materialized_partial" && result.Receipt.Failure == nil {
					t.Fatal("partial preparation lost")
				}
				return
			}
			result, err := observer.ReadIntegrationCandidateObservation(context.Background(), expected, verifier, binding.ObservedAt)
			switch scenario {
			case "v2_passed":
				if err != nil || result.State != "passed" || len(result.Jobs) != len(names) ||
					len(result.CheckIDs) != len(names) || result.Identity.FinalizerCheckRunID != 4 {
					t.Fatalf("complete actual v2 subject not verified: %+v %v", result, err)
				}
			case "v2_metadata":
				if err == nil || result.State == "passed" {
					t.Fatal("metadata drift accepted as passing exact proof")
				}
			case "passed", "rejected":
				if err != nil || result.State != scenario || result.ArtifactID != 10 ||
					result.Identity.FinalizerCheckRunID != 4 || len(result.Jobs) != 2 || result.JobCompleteness != "complete" {
					t.Fatalf("actual observation: %+v %v", result, err)
				}
			case "latest_attempt", "changed_during_read":
				if err != nil || result.State != "superseded" {
					t.Fatalf("attempt race: %+v %v", result, err)
				}
			default:
				if err == nil || result.State != "unknown" {
					t.Fatalf("unverified subject accepted: %+v %v", result, err)
				}
			}
		})
	}
}
