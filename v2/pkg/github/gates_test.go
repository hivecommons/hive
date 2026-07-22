package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// newPullRequestGateTestServer supplies the empty review inventory used by
// gate tests that are not specifically exercising review-state behavior.
// Production inspection always reads this endpoint and fails closed if it is
// unavailable, so the common fixture must model the endpoint explicitly.
func newPullRequestGateTestServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/repos/owner/repo/pulls/") && strings.HasSuffix(request.URL.Path, "/reviews") {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `[]`)
			return
		}
		handler(writer, request)
	}))
}

func TestInspectPullRequestGateAndMergeExactSHA(t *testing.T) {
	mergeSHASeen := ""
	pullReads := 0
	diffReads := 0
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7" && strings.Contains(request.Header.Get("Accept"), "diff"):
			diffReads++
			writer.Header().Set("Content-Type", "application/vnd.github.v3.diff")
			_, _ = io.WriteString(writer, "diff --git a/tests/widget.test.ts b/tests/widget.test.ts\n-old\n+new\n")
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7":
			pullReads++
			_, _ = io.WriteString(writer, `{"number":7,"changed_files":1,"state":"open","draft":false,"html_url":"https://example.test/pull/7","mergeable":true,"mergeable_state":"clean","head":{"sha":"abc","ref":"feature","repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":"base-123"},"labels":[]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/widget.test.ts"}]`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42},{"context":"unit","app_id":42}]},"enforce_admins":{"enabled":true},"required_pull_request_reviews":{"required_approving_review_count":0}}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/commits/abc/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":2,"check_runs":[{"id":101,"name":"visual-hive","app":{"id":42},"check_suite":{"id":501},"head_sha":"abc","status":"completed","conclusion":"success","html_url":"https://example.test/run/1"},{"id":102,"name":"unit","app":{"id":42},"head_sha":"abc","status":"completed","conclusion":"success"}]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs":
			writePullRequestWorkflowRun(writer, 501, 601, 7, "abc", "base-123", "feature", "main", "success")
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/commits/abc/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		case request.Method == http.MethodPut && request.URL.Path == "/repos/owner/repo/pulls/7/merge":
			var body struct {
				SHA string `json:"sha"`
			}
			_ = json.NewDecoder(request.Body).Decode(&body)
			mergeSHASeen = body.SHA
			_, _ = io.WriteString(writer, `{"merged":true,"sha":"merge-sha","message":"merged"}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if gate.HeadSHA != "abc" || gate.BaseSHA != "base-123" || !gate.VisualHiveVerdictGreen || !gate.VisualHiveProvenanceVerified || gate.VisualHiveCheckRunID != 101 || gate.VisualHiveWorkflowRunID != 601 ||
		gate.VisualHiveWorkflowPath != visualHivePullRequestWorkflowPath || len(gate.Checks) != 2 || !gate.Checks[1].ProvenanceVerified || gate.Checks[1].WorkflowRunID != 601 ||
		!gate.BranchProtectionEnabled || !gate.BranchProtectionConfigured || !gate.BranchProtectionStrict || !gate.Mergeable {
		t.Fatalf("unexpected gate: %+v", gate)
	}
	if len(gate.RequiredCheckStates) != 2 || gate.RequiredCheckStates[0] != "success" || gate.RequiredCheckStates[1] != "success" {
		t.Fatalf("required checks = %v", gate.RequiredCheckStates)
	}
	mergeSHA, err := client.MergePullRequestExact(context.Background(), "owner/repo", 7, gate.HeadSHA, gate.BaseSHA, "main", func(live PullRequestGate, diffDigest string) error {
		if !live.Open || live.Merged || live.Hold || live.HeadSHA != gate.HeadSHA || live.BaseSHA != gate.BaseSHA || !live.VisualHiveVerdictGreen || !live.BranchProtectionEnabled {
			return fmt.Errorf("unsafe live gate: %+v", live)
		}
		if len(diffDigest) != 64 {
			return fmt.Errorf("live diff digest is not bound")
		}
		return nil
	})
	if err != nil || mergeSHA != "merge-sha" || mergeSHASeen != "abc" || pullReads != 3 || diffReads != 2 {
		t.Fatalf("merge = %q expected=%q pull reads=%d diff reads=%d err=%v", mergeSHA, mergeSHASeen, pullReads, diffReads, err)
	}
}

func writePullRequestWorkflowRun(writer http.ResponseWriter, suiteID, runID int64, number int, head, base, headRef, baseRef, conclusion string) {
	status := "completed"
	if conclusion == "" {
		status = "in_progress"
	}
	_, _ = fmt.Fprintf(writer, `{"total_count":1,"workflow_runs":[{"id":%d,"name":"Visual Hive PR","path":".github/workflows/visual-hive-pr.yml","event":"pull_request","head_branch":%q,"head_sha":%q,"status":%q,"conclusion":%q,"check_suite_id":%d,"repository":{"full_name":"owner/repo"},"head_repository":{"full_name":"owner/repo"},"pull_requests":[{"number":%d,"head":{"sha":%q,"ref":%q},"base":{"sha":%q,"ref":%q}}]}]}`, runID, headRef, head, status, conclusion, suiteID, number, head, headRef, base, baseRef)
}

func TestInspectPullRequestGateVerifiesVisualHiveProvenanceWithoutProtection(t *testing.T) {
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/9":
			_, _ = io.WriteString(writer, `{"number":9,"changed_files":1,"state":"open","draft":false,"html_url":"https://example.test/pull/9","mergeable":true,"mergeable_state":"clean","head":{"sha":"bootstrap-head","ref":"hive/setup-baseline-123","repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":"bootstrap-base"},"labels":[]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/9/files":
			_, _ = io.WriteString(writer, `[{"filename":".visual-hive/snapshots/linux/home.png"}]`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection":
			http.Error(writer, "missing", http.StatusNotFound)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/rules/branches/main":
			http.Error(writer, "missing", http.StatusNotFound)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/commits/bootstrap-head/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"check_runs":[{"id":901,"name":"visual-hive","app":{"id":42},"check_suite":{"id":902},"head_sha":"bootstrap-head","status":"completed","conclusion":"success","html_url":"https://example.test/run/903"}]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs":
			writePullRequestWorkflowRun(writer, 902, 903, 9, "bootstrap-head", "bootstrap-base", "hive/setup-baseline-123", "main", "success")
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/commits/bootstrap-head/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/9/reviews":
			_, _ = io.WriteString(writer, `[]`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 9)
	if err != nil {
		t.Fatal(err)
	}
	if gate.BranchProtectionConfigured || gate.BranchProtectionEnabled || gate.VisualHiveRequired || len(gate.RequiredCheckNames) != 0 || len(gate.RequiredCheckStates) != 0 {
		t.Fatalf("unconfigured bootstrap acquired imaginary protection: %+v", gate)
	}
	if !gate.BaselineChanged || gate.SecuritySensitive || gate.DeploymentChanged || gate.WorkflowChanged {
		t.Fatalf("hosted snapshot path was not classified as a restricted baseline-only change: %+v", gate)
	}
	if !gate.VisualHiveVerdictGreen || !gate.VisualHiveProvenanceVerified || gate.VisualHiveCheckState != "success" ||
		gate.VisualHiveCheckRunID != 901 || gate.VisualHiveWorkflowRunID != 903 || gate.VisualHiveWorkflowPath != visualHivePullRequestWorkflowPath ||
		gate.VisualHiveWorkflowEvent != visualHivePullRequestEvent {
		t.Fatalf("unprotected exact Visual Hive workflow provenance was not verified: %+v", gate)
	}
}

func TestClassifyGatePathsTreatsVisualHiveSnapshotsAsRestrictedBaselineImages(t *testing.T) {
	gate := PullRequestGate{ChangedFiles: []string{".visual-hive/snapshots/linux/auth/deploy.png"}}
	classifyGatePaths(&gate)
	if !gate.BaselineChanged {
		t.Fatal("Visual Hive hosted snapshot did not trigger the generic baseline merge restriction")
	}
	if gate.SecuritySensitive || gate.DeploymentChanged || gate.WorkflowChanged {
		t.Fatalf("reviewed image filename was misclassified as application code: %+v", gate)
	}
}

func TestMergePullRequestExactRejectsLiveRefDrift(t *testing.T) {
	for name, live := range map[string]struct {
		head   string
		base   string
		branch string
		state  string
	}{
		"head moved":      {head: "new-head", base: "expected-base", branch: "main", state: "open"},
		"base moved":      {head: "expected-head", base: "new-base", branch: "main", state: "open"},
		"base retargeted": {head: "expected-head", base: "expected-base", branch: "release", state: "open"},
		"pull closed":     {head: "expected-head", base: "expected-base", branch: "main", state: "closed"},
	} {
		t.Run(name, func(t *testing.T) {
			mergeRequests := 0
			server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7":
					_, _ = io.WriteString(writer, `{"number":7,"changed_files":1,"state":"`+live.state+`","head":{"sha":"`+live.head+`"},"base":{"ref":"`+live.branch+`","sha":"`+live.base+`"}}`)
				case request.Method == http.MethodPut && request.URL.Path == "/repos/owner/repo/pulls/7/merge":
					mergeRequests++
					_, _ = io.WriteString(writer, `{"merged":true,"sha":"must-not-merge"}`)
				default:
					http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
				}
			}))
			defer server.Close()

			client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
			if _, err := client.MergePullRequestExact(context.Background(), "owner/repo", 7, "expected-head", "expected-base", "main", func(PullRequestGate, string) error { return nil }); err == nil || mergeRequests != 0 {
				t.Fatalf("drift must fail closed before merge: requests=%d err=%v", mergeRequests, err)
			}
		})
	}
}

func TestMergePullRequestExactRequiresBothExpectedSHAs(t *testing.T) {
	client := NewClientForTest("https://example.invalid", "owner", []string{"repo"}, slog.Default())
	if _, err := client.MergePullRequestExact(context.Background(), "owner/repo", 7, "expected-head", "", "main", func(PullRequestGate, string) error { return nil }); err == nil {
		t.Fatal("missing expected base SHA must be rejected before any GitHub request")
	}
	if _, err := client.MergePullRequestExact(context.Background(), "owner/repo", 7, "expected-head", "expected-base", "main", nil); err == nil {
		t.Fatal("missing complete live gate authorizer must be rejected before any GitHub request")
	}
}

func TestMergePullRequestExactRejectsCompleteGateDriftDuringDurableAuthorization(t *testing.T) {
	for _, change := range []string{"hold added", "protection weakened", "required check changed"} {
		t.Run(change, func(t *testing.T) {
			phase := "authorized"
			mergeRequests := 0
			server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch {
				case request.URL.Path == "/apps/github-actions":
					_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
				case request.URL.Path == "/repos/owner/repo/pulls/7" && strings.Contains(request.Header.Get("Accept"), "diff"):
					writer.Header().Set("Content-Type", "application/vnd.github.v3.diff")
					_, _ = io.WriteString(writer, "diff --git a/tests/widget.test.ts b/tests/widget.test.ts\n-old\n+new\n")
				case request.URL.Path == "/repos/owner/repo/pulls/7":
					labels := "[]"
					if phase == "changed" && change == "hold added" {
						labels = `[{"name":"do-not-merge"}]`
					}
					_, _ = fmt.Fprintf(writer, `{"number":7,"changed_files":1,"state":"open","draft":false,"mergeable":true,"mergeable_state":"clean","head":{"sha":"abc","ref":"feature","repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":"base-123"},"labels":%s}`, labels)
				case request.URL.Path == "/repos/owner/repo/pulls/7/files":
					_, _ = io.WriteString(writer, `[{"filename":"tests/widget.test.ts"}]`)
				case request.URL.Path == "/repos/owner/repo/branches/main/protection":
					strict, contextName := true, "visual-hive"
					if phase == "changed" && change == "protection weakened" {
						strict = false
					}
					if phase == "changed" && change == "required check changed" {
						contextName = "security-scan"
					}
					_, _ = fmt.Fprintf(writer, `{"required_status_checks":{"strict":%t,"checks":[{"context":%q,"app_id":42}]},"enforce_admins":{"enabled":true}}`, strict, contextName)
				case request.URL.Path == "/repos/owner/repo/rules/branches/main":
					_, _ = io.WriteString(writer, `[]`)
				case request.URL.Path == "/repos/owner/repo/commits/abc/check-runs":
					_, _ = io.WriteString(writer, `{"total_count":2,"check_runs":[{"id":101,"name":"visual-hive","app":{"id":42},"check_suite":{"id":501},"head_sha":"abc","status":"completed","conclusion":"success"},{"id":102,"name":"security-scan","app":{"id":42},"head_sha":"abc","status":"completed","conclusion":"success"}]}`)
				case request.URL.Path == "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs":
					writePullRequestWorkflowRun(writer, 501, 601, 7, "abc", "base-123", "feature", "main", "success")
				case request.URL.Path == "/repos/owner/repo/commits/abc/status":
					_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
				case request.Method == http.MethodPut && request.URL.Path == "/repos/owner/repo/pulls/7/merge":
					mergeRequests++
					_, _ = io.WriteString(writer, `{"merged":true,"sha":"must-not-merge"}`)
				default:
					http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
				}
			}))
			defer server.Close()

			client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
			_, err := client.MergePullRequestExact(context.Background(), "owner/repo", 7, "abc", "base-123", "main", func(live PullRequestGate, _ string) error {
				if live.Hold || !live.BranchProtectionEnabled || !live.VisualHiveVerdictGreen {
					return fmt.Errorf("initial live gate unexpectedly unsafe: %+v", live)
				}
				phase = "changed"
				return nil
			})
			var finalGateErr *FinalMergeGateError
			if err == nil || !errors.As(err, &finalGateErr) || mergeRequests != 0 {
				t.Fatalf("%s drift reached merge: requests=%d err=%v", change, mergeRequests, err)
			}
		})
	}
}

func TestEnsureMinimumBranchProtectionCreatesOnlyWhenAbsent(t *testing.T) {
	updates := 0
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection":
			http.Error(writer, "missing", http.StatusNotFound)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/rules/branches/main":
			http.Error(writer, "missing", http.StatusNotFound)
		case request.Method == http.MethodPut && request.URL.Path == "/repos/owner/repo/branches/main/protection":
			updates++
			var body struct {
				EnforceAdmins        bool `json:"enforce_admins"`
				RequiredStatusChecks struct {
					Strict bool                    `json:"strict"`
					Checks []RequiredCheckIdentity `json:"checks"`
				} `json:"required_status_checks"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || !body.EnforceAdmins || !body.RequiredStatusChecks.Strict || len(body.RequiredStatusChecks.Checks) != 1 || body.RequiredStatusChecks.Checks[0].Context != "visual-hive" || body.RequiredStatusChecks.Checks[0].AppID != 42 {
				t.Fatalf("unsafe protection request: %+v err=%v", body, err)
			}
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true}}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	created, err := client.EnsureMinimumBranchProtection(context.Background(), "owner/repo", "main", []RequiredCheckIdentity{{Context: "visual-hive", AppID: 42}, {Context: "visual-hive", AppID: 42}})
	if err != nil || !created || updates != 1 {
		t.Fatalf("minimum protection = created %t updates %d err %v", created, updates, err)
	}
}

func TestEnsureMinimumBranchProtectionRejectsUnboundExpectedIdentity(t *testing.T) {
	client := NewClientForTest("https://example.invalid", "owner", []string{"repo"}, slog.Default())
	if _, err := client.EnsureMinimumBranchProtection(context.Background(), "owner/repo", "main", []RequiredCheckIdentity{{Context: "visual-hive", AppID: -1}}); err == nil {
		t.Fatal("name-only expected identity was accepted")
	}
}

func TestEnsureMinimumBranchProtectionRejectsExistingPolicyMissingVisualHive(t *testing.T) {
	updates := 0
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection" {
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":["organization-ci"]},"required_pull_request_reviews":{"required_approving_review_count":2}}`)
			return
		}
		if request.Method == http.MethodPut {
			updates++
		}
		http.Error(writer, "unexpected", http.StatusNotFound)
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	created, err := client.EnsureMinimumBranchProtection(context.Background(), "owner/repo", "main", []RequiredCheckIdentity{{Context: "visual-hive", AppID: 42}})
	if err == nil || created || updates != 0 {
		t.Fatalf("policy without Visual Hive was accepted or replaced: created %t updates %d err %v", created, updates, err)
	}
}

func TestEnsureMinimumVisualHiveProtectionResolvesAndBindsGitHubActionsApp(t *testing.T) {
	updates := 0
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":15368,"slug":"github-actions"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection":
			http.Error(writer, "missing", http.StatusNotFound)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/rules/branches/main":
			http.Error(writer, "missing", http.StatusNotFound)
		case request.Method == http.MethodPut && request.URL.Path == "/repos/owner/repo/branches/main/protection":
			updates++
			var body struct {
				RequiredStatusChecks struct {
					Checks []RequiredCheckIdentity `json:"checks"`
				} `json:"required_status_checks"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || len(body.RequiredStatusChecks.Checks) != 1 || body.RequiredStatusChecks.Checks[0].Context != "visual-hive" || body.RequiredStatusChecks.Checks[0].AppID != 15368 {
				t.Fatalf("Visual Hive protection did not bind GitHub Actions: %+v err=%v", body, err)
			}
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":15368}]},"enforce_admins":{"enabled":true}}`)
		default:
			http.Error(writer, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	created, err := client.EnsureMinimumVisualHiveBranchProtection(context.Background(), "owner/repo", "main")
	if err != nil || !created || updates != 1 {
		t.Fatalf("Visual Hive protection = created %t updates %d err %v", created, updates, err)
	}
}

func TestEnsureMinimumBranchProtectionRejectsNameOnlyVisualHive(t *testing.T) {
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection" {
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":["visual-hive"]},"enforce_admins":{"enabled":true}}`)
			return
		}
		http.Error(writer, "unexpected", http.StatusNotFound)
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	created, err := client.EnsureMinimumBranchProtection(context.Background(), "owner/repo", "main", []RequiredCheckIdentity{{Context: "visual-hive", AppID: 42}})
	if err == nil || created {
		t.Fatalf("name-only Visual Hive requirement was accepted: created=%t err=%v", created, err)
	}
}

func TestEnsureMinimumBranchProtectionRejectsNonStrictExistingPolicy(t *testing.T) {
	updates := 0
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection" {
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":false,"contexts":["visual-hive"]}}`)
			return
		}
		if request.Method == http.MethodPut {
			updates++
		}
		http.Error(writer, "unexpected", http.StatusNotFound)
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	created, err := client.EnsureMinimumBranchProtection(context.Background(), "owner/repo", "main", []RequiredCheckIdentity{{Context: "visual-hive", AppID: 42}})
	if err == nil || created || updates != 0 {
		t.Fatalf("non-strict protection must fail closed without replacement: created=%t updates=%d err=%v", created, updates, err)
	}
}

func TestBranchProtectionDoesNotAssumeRulesetHasNoWriterBypass(t *testing.T) {
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":false,"contexts":["classic-ci"]},"enforce_admins":{"enabled":true}}`)
		case "/repos/owner/repo/rules/branches/main":
			_, _ = io.WriteString(writer, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"visual-hive"}],"strict_required_status_checks_policy":true}}]`)
		default:
			http.Error(writer, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	summary, err := client.BranchProtection(context.Background(), "owner/repo", "main")
	if err != nil || !summary.Enabled || !summary.Strict || summary.AdminEnforced || len(summary.RequiredChecks) != 2 || summary.RequiredChecks[0] != "classic-ci" || summary.RequiredChecks[1] != "visual-hive" {
		t.Fatalf("ruleset with unknown bypass authority was not represented fail-closed: %+v err=%v", summary, err)
	}
}

func TestInspectPullRequestGateUnionsClassicAndRulesetChecksButFailsClosedOnUnknownBypass(t *testing.T) {
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case "/repos/owner/repo/pulls/17":
			_, _ = io.WriteString(writer, `{"number":17,"changed_files":1,"state":"open","mergeable":true,"head":{"sha":"head-17","ref":"hive/repair-proof","repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":"base-17"},"labels":[]}`)
		case "/repos/owner/repo/pulls/17/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/exact.test.ts"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true}}`)
		case "/repos/owner/repo/rules/branches/main":
			_, _ = io.WriteString(writer, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"security-scan","integration_id":77}],"strict_required_status_checks_policy":true}},{"type":"required_deployments","parameters":{"required_deployment_environments":["production"]}}]`)
		case "/repos/owner/repo/commits/head-17/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":2,"check_runs":[{"id":171,"name":"visual-hive","app":{"id":42},"check_suite":{"id":173},"head_sha":"head-17","status":"completed","conclusion":"success"},{"id":172,"name":"security-scan","app":{"id":77},"head_sha":"head-17","status":"completed","conclusion":"failure"}]}`)
		case "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs":
			writePullRequestWorkflowRun(writer, 173, 174, 17, "head-17", "base-17", "hive/repair-proof", "main", "success")
		case "/repos/owner/repo/commits/head-17/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 17)
	if err != nil {
		t.Fatal(err)
	}
	if !gate.BranchProtectionConfigured || !gate.BranchProtectionStrict || gate.BranchProtectionAdminEnforced || gate.BranchProtectionEnabled {
		t.Fatalf("ruleset with unknown bypass authority was trusted beside classic protection: %+v", gate)
	}
	if len(gate.RequiredCheckNames) != 2 || gate.RequiredCheckNames[0] != "security-scan" || gate.RequiredCheckNames[1] != "visual-hive" ||
		len(gate.RequiredCheckStates) != 2 || gate.RequiredCheckStates[0] != "failure" || gate.RequiredCheckStates[1] != "success" || !gate.VisualHiveVerdictGreen {
		t.Fatalf("classic and ruleset checks were not unioned with the red security gate preserved: %+v", gate)
	}
}

func TestBranchProtectionFailsClosedForUnsupportedApplicableRulesetRule(t *testing.T) {
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true}}`)
		case "/repos/owner/repo/rules/branches/main":
			_, _ = io.WriteString(writer, `[{"type":"file_path_restriction","parameters":{"restricted_file_paths":["security/**"]}}]`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	summary, err := client.BranchProtection(context.Background(), "owner/repo", "main")
	if err != nil || !summary.Enabled || !summary.Strict || summary.AdminEnforced || len(summary.RequiredChecks) != 1 || summary.RequiredChecks[0] != "visual-hive" {
		t.Fatalf("unsupported applicable ruleset semantics were trusted: %+v err=%v", summary, err)
	}
}

func TestApplicableBranchRulesAreExhaustivelyPaginated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/repos/owner/repo/rules/branches/main" {
			http.Error(writer, "unexpected", http.StatusNotFound)
			return
		}
		if request.URL.Query().Get("page") == "2" {
			_, _ = io.WriteString(writer, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"page-two-security","integration_id":77}],"strict_required_status_checks_policy":true}}]`)
			return
		}
		writer.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2&per_page=100>; rel="next"`, request.Host, request.URL.Path))
		_, _ = io.WriteString(writer, `[{"type":"required_deployments","parameters":{"required_deployment_environments":["production"]}}]`)
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	rules, _, err := client.allRulesForBranch(context.Background(), "owner", "repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if branchRuleCount(rules) != 2 || len(rules.RequiredStatusChecks) != 1 || rules.RequiredStatusChecks[0].Parameters.RequiredStatusChecks[0].Context != "page-two-security" {
		t.Fatalf("applicable branch rules were truncated before page two: %+v", rules)
	}
}

func TestDeleteRepairBranchRequiresExactUnmovedHead(t *testing.T) {
	deleted := 0
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/repair-proof":
			_, _ = io.WriteString(writer, `{"ref":"refs/heads/hive/repair-proof","object":{"sha":"repair-head","type":"commit"}}`)
		case request.Method == http.MethodDelete && request.URL.Path == "/repos/owner/repo/git/refs/heads/hive/repair-proof":
			deleted++
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := client.DeleteRepairBranchExact(context.Background(), "owner/repo", "hive/repair-proof", "different-head"); err == nil || deleted != 0 {
		t.Fatalf("moved repair branch must be preserved: deleted=%d err=%v", deleted, err)
	}
	if err := client.DeleteRepairBranchExact(context.Background(), "owner/repo", "hive/repair-proof", "repair-head"); err != nil || deleted != 1 {
		t.Fatalf("exact merged repair branch was not deleted: deleted=%d err=%v", deleted, err)
	}
	if err := client.DeleteRepairBranchExact(context.Background(), "owner/repo", "codex/not-hive", "repair-head"); err == nil {
		t.Fatal("non-Hive branch deletion must be rejected")
	}
}

func TestDeleteBaselineBranchRequiresExactReviewedHead(t *testing.T) {
	deleted := 0
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/baseline-proof":
			_, _ = io.WriteString(writer, `{"ref":"refs/heads/hive/baseline-proof","object":{"sha":"reviewed-head","type":"commit"}}`)
		case request.Method == http.MethodDelete && request.URL.Path == "/repos/owner/repo/git/refs/heads/hive/baseline-proof":
			deleted++
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if _, err := client.DeleteBaselineBranchExact(context.Background(), "owner/repo", "hive/baseline-proof", "different"); err == nil || deleted != 0 {
		t.Fatalf("moved baseline branch must be preserved: deleted=%d err=%v", deleted, err)
	}
	wasDeleted, err := client.DeleteBaselineBranchExact(context.Background(), "owner/repo", "hive/baseline-proof", "reviewed-head")
	if err != nil || !wasDeleted || deleted != 1 {
		t.Fatalf("exact baseline branch was not deleted: wasDeleted=%t deleted=%d err=%v", wasDeleted, deleted, err)
	}
	if _, err := client.DeleteBaselineBranchExact(context.Background(), "owner/repo", "hive/repair-proof", "reviewed-head"); err == nil {
		t.Fatal("non-baseline branch deletion must be rejected")
	}
}

func TestInspectPullRequestGateKeepsUnsafeAndPendingSignals(t *testing.T) {
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case "/repos/owner/repo/pulls/8":
			_, _ = io.WriteString(writer, `{"number":8,"changed_files":4,"state":"open","draft":true,"mergeable":false,"head":{"sha":"def"},"base":{"ref":"main"},"labels":[{"name":"hold"}]}`)
		case "/repos/owner/repo/pulls/8/files":
			_, _ = io.WriteString(writer, `[{"filename":".github/workflows/release.yml"},{"filename":"tests/__screenshots__/home.png"},{"filename":"src/auth/session.ts"},{"filename":"deploy/app.yaml"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":["Visual Hive PR"]},"enforce_admins":{"enabled":true}}`)
		case "/repos/owner/repo/commits/def/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"check_runs":[{"name":"Visual Hive PR","head_sha":"def","status":"in_progress"}]}`)
		case "/repos/owner/repo/commits/def/status":
			_, _ = io.WriteString(writer, `{"state":"pending","statuses":[]}`)
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 8)
	if err != nil {
		t.Fatal(err)
	}
	if !gate.Hold || gate.VisualHiveVerdictGreen || gate.RequiredCheckStates[0] != "pending" || !gate.WorkflowChanged || !gate.BaselineChanged || !gate.SecuritySensitive || !gate.DeploymentChanged {
		t.Fatalf("unsafe signals were lost: %+v", gate)
	}
}

func TestInspectPullRequestGateTreatsNonStrictProtectionAsUnsafe(t *testing.T) {
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case "/repos/owner/repo/pulls/11":
			_, _ = io.WriteString(writer, `{"number":11,"changed_files":1,"state":"open","mergeable":true,"head":{"sha":"head-11"},"base":{"ref":"main","sha":"base-11"},"labels":[]}`)
		case "/repos/owner/repo/pulls/11/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/widget.test.ts"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":false,"contexts":["visual-hive"]}}`)
		case "/repos/owner/repo/commits/head-11/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"check_runs":[{"name":"visual-hive","head_sha":"head-11","status":"completed","conclusion":"success"}]}`)
		case "/repos/owner/repo/commits/head-11/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 11)
	if err != nil {
		t.Fatal(err)
	}
	if !gate.BranchProtectionConfigured || gate.BranchProtectionStrict || gate.BranchProtectionEnabled {
		t.Fatalf("non-strict protection must not satisfy Hive's merge-safety signal: %+v", gate)
	}
}

func TestInspectPullRequestGateGreenProductionCannotMaskRedPRWorkflow(t *testing.T) {
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case "/repos/owner/repo/pulls/10":
			_, _ = io.WriteString(writer, `{"number":10,"changed_files":1,"state":"open","mergeable":true,"head":{"sha":"same-head","ref":"feature","repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":"base-10"},"labels":[]}`)
		case "/repos/owner/repo/pulls/10/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/widget.test.ts"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true}}`)
		case "/repos/owner/repo/commits/same-head/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":2,"check_runs":[{"id":200,"name":"visual-hive","app":{"id":42},"check_suite":{"id":2000},"head_sha":"same-head","status":"completed","conclusion":"success"},{"id":100,"name":"visual-hive","app":{"id":42},"check_suite":{"id":1000},"head_sha":"same-head","status":"completed","conclusion":"failure"}]}`)
		case "/repos/owner/repo/commits/same-head/status":
			_, _ = io.WriteString(writer, `{"state":"failure","statuses":[{"id":300,"context":"visual-hive","state":"failure"}]}`)
		case "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs":
			if request.URL.Query().Get("event") != "pull_request" || request.URL.Query().Get("head_sha") != "same-head" {
				t.Fatalf("workflow provenance query is not exact: %s", request.URL.RawQuery)
			}
			switch request.URL.Query().Get("check_suite_id") {
			case "2000":
				_, _ = io.WriteString(writer, `{"total_count":1,"workflow_runs":[{"id":2001,"name":"Hive Visual Hive Production","path":".github/workflows/hive-visual-hive.yml","event":"workflow_dispatch","head_branch":"feature","head_sha":"same-head","status":"completed","conclusion":"success","check_suite_id":2000}]}`)
			case "1000":
				writePullRequestWorkflowRun(writer, 1000, 1001, 10, "same-head", "base-10", "feature", "main", "failure")
			default:
				t.Fatalf("unexpected check suite query: %s", request.URL.RawQuery)
			}
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if gate.VisualHiveVerdictGreen || !gate.VisualHiveProvenanceVerified || gate.VisualHiveCheckState != "failure" || gate.VisualHiveCheckRunID != 100 ||
		len(gate.RequiredCheckStates) != 1 || gate.RequiredCheckStates[0] != "failure" || len(gate.Checks) != 1 || gate.Checks[0].State != "failure" || !gate.Checks[0].ProvenanceVerified {
		t.Fatalf("green production check masked the exact red PR workflow check: %+v", gate)
	}
}

func TestInspectPullRequestGateGreenManualCheckCannotMaskMissingPRWorkflow(t *testing.T) {
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case "/repos/owner/repo/pulls/12":
			_, _ = io.WriteString(writer, `{"number":12,"changed_files":1,"state":"open","mergeable":true,"head":{"sha":"head-12","ref":"feature","repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":"base-12"},"labels":[]}`)
		case "/repos/owner/repo/pulls/12/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/widget.test.ts"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true}}`)
		case "/repos/owner/repo/commits/head-12/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"check_runs":[{"id":120,"name":"visual-hive","app":{"id":42},"check_suite":{"id":121},"head_sha":"head-12","status":"completed","conclusion":"success"}]}`)
		case "/repos/owner/repo/commits/head-12/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		case "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"workflow_runs":[{"id":122,"name":"Hive Visual Hive Production","path":".github/workflows/hive-visual-hive.yml","event":"workflow_dispatch","head_branch":"feature","head_sha":"head-12","status":"completed","conclusion":"success","check_suite_id":121}]}`)
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if gate.VisualHiveVerdictGreen || gate.VisualHiveProvenanceVerified || len(gate.RequiredCheckStates) != 1 || gate.RequiredCheckStates[0] != "pending" {
		t.Fatalf("manual/production check masked a missing PR workflow check: %+v", gate)
	}
}

func TestInspectPullRequestGateRejectsWrongPRBaseAssociation(t *testing.T) {
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case "/repos/owner/repo/pulls/13":
			_, _ = io.WriteString(writer, `{"number":13,"changed_files":1,"state":"open","mergeable":true,"head":{"sha":"head-13","ref":"feature","repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":"base-13"},"labels":[]}`)
		case "/repos/owner/repo/pulls/13/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/widget.test.ts"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true}}`)
		case "/repos/owner/repo/commits/head-13/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"check_runs":[{"id":130,"name":"visual-hive","app":{"id":42},"check_suite":{"id":131},"head_sha":"head-13","status":"completed","conclusion":"success"}]}`)
		case "/repos/owner/repo/commits/head-13/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		case "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs":
			writePullRequestWorkflowRun(writer, 131, 132, 13, "head-13", "different-base", "feature", "main", "success")
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 13)
	if err != nil {
		t.Fatal(err)
	}
	if gate.VisualHiveVerdictGreen || gate.VisualHiveProvenanceVerified || gate.RequiredCheckStates[0] != "pending" {
		t.Fatalf("wrong PR base association was trusted: %+v", gate)
	}
}

func TestInspectPullRequestGateUsesExactRequiredContextAndApp(t *testing.T) {
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case "/repos/owner/repo/pulls/14":
			_, _ = io.WriteString(writer, `{"number":14,"changed_files":1,"state":"open","mergeable":true,"head":{"sha":"head-14","ref":"feature","repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":"base-14"},"labels":[]}`)
		case "/repos/owner/repo/pulls/14/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/exact.test.ts"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42},{"context":"security-check","app_id":42}]},"enforce_admins":{"enabled":true}}`)
		case "/repos/owner/repo/commits/head-14/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":3,"check_runs":[{"id":3,"name":"visual-hive","app":{"id":42},"check_suite":{"id":314},"head_sha":"head-14","status":"completed","conclusion":"success"},{"id":2,"name":"security_check","app":{"id":42},"status":"completed","conclusion":"success"},{"id":1,"name":"security-check","app":{"id":41},"status":"completed","conclusion":"success"}]}`)
		case "/repos/owner/repo/commits/head-14/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		case "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs":
			writePullRequestWorkflowRun(writer, 314, 1414, 14, "head-14", "base-14", "feature", "main", "success")
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 14)
	if err != nil {
		t.Fatal(err)
	}
	if !gate.VisualHiveVerdictGreen || len(gate.RequiredCheckStates) != 2 || gate.RequiredCheckStates[0] != "pending" || gate.RequiredCheckStates[1] != "success" {
		t.Fatalf("fuzzy context or wrong App ID satisfied exact protection: %+v", gate)
	}
}

func TestInspectPullRequestGateRejectsVisualHiveFromUnexpectedApp(t *testing.T) {
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case "/repos/owner/repo/pulls/16":
			_, _ = io.WriteString(writer, `{"number":16,"changed_files":1,"state":"open","mergeable":true,"head":{"sha":"head-16"},"base":{"ref":"main","sha":"base-16"},"labels":[]}`)
		case "/repos/owner/repo/pulls/16/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/exact.test.ts"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":41}]},"enforce_admins":{"enabled":true}}`)
		case "/repos/owner/repo/commits/head-16/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":2,"check_runs":[{"id":2,"name":"visual-hive","app":{"id":41},"status":"completed","conclusion":"success"},{"id":1,"name":"visual-hive","app":{"id":42},"status":"completed","conclusion":"success"}]}`)
		case "/repos/owner/repo/commits/head-16/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 16)
	if err != nil {
		t.Fatal(err)
	}
	if gate.VisualHiveVerdictGreen || len(gate.RequiredCheckStates) != 1 || gate.RequiredCheckStates[0] != "success" {
		t.Fatalf("unexpected App was allowed to establish Visual Hive identity: %+v", gate)
	}
}

func TestInspectPullRequestGateRejectsAdminBypassProtection(t *testing.T) {
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case "/repos/owner/repo/pulls/15":
			_, _ = io.WriteString(writer, `{"number":15,"changed_files":1,"state":"open","mergeable":true,"head":{"sha":"head-15"},"base":{"ref":"main","sha":"base-15"},"labels":[]}`)
		case "/repos/owner/repo/pulls/15/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/exact.test.ts"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":["visual-hive"]},"enforce_admins":{"enabled":false}}`)
		case "/repos/owner/repo/rules/branches/main":
			_, _ = io.WriteString(writer, `[]`)
		case "/repos/owner/repo/commits/head-15/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"check_runs":[{"id":1,"name":"visual-hive","status":"completed","conclusion":"success"}]}`)
		case "/repos/owner/repo/commits/head-15/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 15)
	if err != nil {
		t.Fatal(err)
	}
	if !gate.BranchProtectionConfigured || !gate.BranchProtectionStrict || gate.BranchProtectionAdminEnforced || gate.BranchProtectionEnabled {
		t.Fatalf("admin-bypassable protection was treated as merge safe: %+v", gate)
	}
}

func TestBaselineImageNameDoesNotImplyAuthCodeChange(t *testing.T) {
	gate := PullRequestGate{ChangedFiles: []string{
		"visual-hive.baselines/linux/public-auth-boundary__mobile.png",
	}}
	classifyGatePaths(&gate)
	if !gate.BaselineChanged || gate.SecuritySensitive || gate.DeploymentChanged || gate.WorkflowChanged {
		t.Fatalf("reviewed baseline filename was misclassified as code risk: %+v", gate)
	}
}

func TestInspectPullRequestGateReportsExternalMerge(t *testing.T) {
	server := newPullRequestGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case "/repos/owner/repo/pulls/9":
			_, _ = io.WriteString(writer, `{"number":9,"changed_files":1,"state":"closed","merged":true,"merge_commit_sha":"merge-123","merged_by":{"login":"outside-user"},"head":{"sha":"head-123","ref":"feature","repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":"base-123"},"labels":[]}`)
		case "/repos/owner/repo/pulls/9/files":
			_, _ = io.WriteString(writer, `[{"filename":"index.html"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true},"required_pull_request_reviews":{"required_approving_review_count":0}}`)
		case "/repos/owner/repo/commits/head-123/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"check_runs":[{"id":909,"name":"visual-hive","app":{"id":42},"check_suite":{"id":919},"head_sha":"head-123","status":"completed","conclusion":"success"}]}`)
		case "/repos/owner/repo/commits/head-123/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		case "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs":
			writePullRequestWorkflowRun(writer, 919, 929, 9, "head-123", "base-123", "feature", "main", "success")
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 9)
	if err != nil {
		t.Fatal(err)
	}
	if gate.Open || !gate.Merged || gate.MergeSHA != "merge-123" || gate.MergedBy != "outside-user" || gate.HeadSHA != "head-123" || gate.BaseSHA != "base-123" || !gate.VisualHiveVerdictGreen {
		t.Fatalf("external merge signals were lost: %+v", gate)
	}
}

func TestExactPullRequestWorkflowRunRequiresExactAssociationFromCheckOrRun(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	repository := &gh.Repository{FullName: gh.Ptr("owner/repo")}
	pull := &gh.PullRequest{
		Number: gh.Ptr(7), Head: &gh.PullRequestBranch{Ref: gh.Ptr("hive/repair-proof"), SHA: gh.Ptr(head), Repo: repository},
		Base: &gh.PullRequestBranch{Ref: gh.Ptr("main"), SHA: gh.Ptr(base), Repo: repository},
	}
	check := &gh.CheckRun{
		ID: gh.Ptr(int64(101)), Name: gh.Ptr(visualHivePullRequestContext), HeadSHA: gh.Ptr(head),
		Status: gh.Ptr("completed"), Conclusion: gh.Ptr("success"), CheckSuite: &gh.CheckSuite{ID: gh.Ptr(int64(501))},
	}
	run := &gh.WorkflowRun{
		ID: gh.Ptr(int64(601)), Name: gh.Ptr(visualHivePullRequestWorkflowName), Path: gh.Ptr(visualHivePullRequestWorkflowPath),
		Event: gh.Ptr(visualHivePullRequestEvent), HeadSHA: gh.Ptr(head), HeadBranch: gh.Ptr("hive/repair-proof"), CheckSuiteID: gh.Ptr(int64(501)),
		Status: gh.Ptr("completed"), Conclusion: gh.Ptr("success"), Repository: repository, HeadRepository: repository,
	}
	if exactPullRequestWorkflowRun(run, "owner/repo", pull, check) {
		t.Fatal("same-head check and workflow with both PR association arrays absent were accepted")
	}
	exactAssociation := &gh.PullRequest{Number: gh.Ptr(7), Head: pull.Head, Base: pull.Base}
	check.PullRequests = []*gh.PullRequest{exactAssociation}
	if !exactPullRequestWorkflowRun(run, "owner/repo", pull, check) {
		t.Fatal("exact CheckRun PR/base association was not accepted when WorkflowRun association was absent")
	}
	wrongBase := &gh.PullRequest{Number: gh.Ptr(7), Head: pull.Head, Base: &gh.PullRequestBranch{Ref: gh.Ptr("release"), SHA: gh.Ptr(strings.Repeat("c", 40))}}
	run.PullRequests = []*gh.PullRequest{wrongBase}
	if exactPullRequestWorkflowRun(run, "owner/repo", pull, check) {
		t.Fatal("same-head workflow associated with a different PR base was accepted")
	}
}

func TestListPullRequestFilesRequiresCompleteBoundedUniqueInventory(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Query().Get("page") {
		case "", "0":
			writer.Header().Set("Link", fmt.Sprintf("<%s/repos/owner/repo/pulls/7/files?page=2>; rel=\"next\"", "http://"+request.Host))
			_, _ = io.WriteString(writer, `[{"filename":"tests/one.test.ts"}]`)
		case "2":
			_, _ = io.WriteString(writer, `[{"filename":"tests/two.test.ts"}]`)
		default:
			http.Error(writer, "unexpected page", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	files, err := client.listPullRequestFiles(context.Background(), "owner", "repo", 7, 2)
	if err != nil || requests != 2 || len(files) != 2 || files[0] != "tests/one.test.ts" || files[1] != "tests/two.test.ts" {
		t.Fatalf("complete inventory = %v requests=%d err=%v", files, requests, err)
	}
	if _, err := client.listPullRequestFiles(context.Background(), "owner", "repo", 7, maxPullRequestFiles+1); err == nil || !strings.Contains(err.Error(), "at most 3000") {
		t.Fatalf("oversized changed-file count was not rejected before enumeration: %v", err)
	}
}

func TestListPullRequestFilesRejectsCountMismatchAndDuplicates(t *testing.T) {
	for name, body := range map[string]string{
		"missing":   `[{"filename":"tests/one.test.ts"}]`,
		"duplicate": `[{"filename":"tests/one.test.ts"},{"filename":"tests/one.test.ts"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, body)
			}))
			defer server.Close()
			client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
			if _, err := client.listPullRequestFiles(context.Background(), "owner", "repo", 7, 2); err == nil {
				t.Fatal("non-exact changed-file inventory was accepted")
			}
		})
	}
}

func TestReviewStatePaginatesAndUsesLatestDecisiveReviewPerImmutableUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("page") == "2" {
			_, _ = io.WriteString(writer, `[{"id":102,"state":"CHANGES_REQUESTED","submitted_at":"2026-07-12T12:00:00Z","user":{"id":55,"login":"reviewer-renamed"}},{"id":103,"state":"APPROVED","submitted_at":"2026-07-12T12:01:00Z","user":{"id":77,"login":"other"}}]`)
			return
		}
		writer.Header().Set("Link", fmt.Sprintf("<%s/repos/owner/repo/pulls/7/reviews?page=2>; rel=\"next\"", "http://"+request.Host))
		_, _ = io.WriteString(writer, `[{"id":101,"state":"APPROVED","submitted_at":"2026-07-12T11:00:00Z","user":{"id":55,"login":"reviewer"}}]`)
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	approvals, changes, err := client.reviewState(context.Background(), "owner", "repo", 7)
	if err != nil || approvals != 1 || !changes {
		t.Fatalf("review state approvals=%d changes=%t err=%v", approvals, changes, err)
	}
}

func TestReviewStateRejectsDuplicateReviewAcrossPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("page") != "2" {
			writer.Header().Set("Link", fmt.Sprintf("<%s/repos/owner/repo/pulls/7/reviews?page=2>; rel=\"next\"", "http://"+request.Host))
		}
		_, _ = io.WriteString(writer, `[{"id":101,"state":"APPROVED","submitted_at":"2026-07-12T11:00:00Z","user":{"id":55,"login":"reviewer"}}]`)
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if _, _, err := client.reviewState(context.Background(), "owner", "repo", 7); err == nil || !strings.Contains(err.Error(), "duplicate review ID") {
		t.Fatalf("duplicate review inventory was not rejected: %v", err)
	}
}
