package github

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInspectRepairPullRequestRefreshExactIsReadOnly(t *testing.T) {
	oldHead := strings.Repeat("a", 40)
	baseSHA := strings.Repeat("b", 40)
	mutationCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true}}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7":
			_, _ = fmt.Fprintf(writer, `{"number":7,"changed_files":1,"state":"open","merged":false,"body":"<!-- hive-repair: finding -->","mergeable":false,"mergeable_state":"behind","head":{"ref":"hive/repair-proof-a1","sha":%q,"repo":{"id":123,"full_name":"owner/repo"}},"base":{"ref":"main","sha":%q,"repo":{"id":123,"full_name":"owner/repo"}}}`, oldHead, baseSHA)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/proof.test.ts"}]`)
		case request.Method == http.MethodPut && request.URL.Path == "/repos/owner/repo/pulls/7/update-branch":
			mutationCalls++
			http.Error(writer, "mutation forbidden", http.StatusInternalServerError)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	result, err := client.InspectRepairPullRequestRefreshExact(context.Background(), RepairBranchRefreshRequest{
		Repository: "owner/repo", RepositoryID: "123", PRNumber: 7, Branch: "hive/repair-proof-a1",
		ExpectedHeadSHA: oldHead, ExpectedBaseBranch: "main", ExpectedBaseSHA: baseSHA,
		ExpectedChangedFiles: []string{"tests/proof.test.ts"}, RepositoryFingerprint: "finding",
	})
	if err != nil {
		t.Fatal(err)
	}
	if mutationCalls != 0 || result.HeadSHA != oldHead || result.BaseSHA != baseSHA || result.BaseMoved || len(result.ChangedFiles) != 1 {
		t.Fatalf("unexpected read-only exact refresh observation: mutations=%d result=%+v", mutationCalls, result)
	}
}

func TestRefreshRepairPullRequestBranchRejectsForkBeforeMutation(t *testing.T) {
	oldHead := strings.Repeat("a", 40)
	baseSHA := strings.Repeat("b", 40)
	updates := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":["visual-hive"]},"enforce_admins":{"enabled":true}}`)
		case "/repos/owner/repo/pulls/7":
			_, _ = fmt.Fprintf(writer, `{"number":7,"changed_files":1,"state":"open","body":"<!-- hive-repair: finding -->","mergeable_state":"behind","head":{"ref":"hive/repair-proof-a1","sha":%q,"repo":{"id":999,"full_name":"fork/repo"}},"base":{"ref":"main","sha":%q,"repo":{"id":123,"full_name":"owner/repo"}}}`, oldHead, baseSHA)
		case "/repos/owner/repo/pulls/7/update-branch":
			updates++
			writer.WriteHeader(http.StatusAccepted)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, err := client.InspectRepairPullRequestRefreshExact(context.Background(), RepairBranchRefreshRequest{
		Repository: "owner/repo", RepositoryID: "123", PRNumber: 7, Branch: "hive/repair-proof-a1", ExpectedHeadSHA: oldHead,
		ExpectedBaseBranch: "main", ExpectedBaseSHA: baseSHA, ExpectedChangedFiles: []string{"tests/proof.test.ts"}, RepositoryFingerprint: "finding",
	})
	if err == nil || !strings.Contains(err.Error(), "forked") || updates != 0 {
		t.Fatalf("fork refresh was not rejected before mutation: updates=%d err=%v", updates, err)
	}
}

func TestInspectRepairPullRequestRefreshReportsBaseMovementWithoutMutation(t *testing.T) {
	oldHead := strings.Repeat("a", 40)
	reviewedBaseSHA := strings.Repeat("b", 40)
	liveBaseSHA := strings.Repeat("c", 40)
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":["visual-hive"]},"enforce_admins":{"enabled":true}}`)
		case "/repos/owner/repo/pulls/7":
			_, _ = fmt.Fprintf(writer, `{"number":7,"changed_files":1,"state":"open","body":"<!-- hive-repair: finding -->","mergeable_state":"behind","head":{"ref":"hive/repair-proof-a1","sha":%q,"repo":{"id":123,"full_name":"owner/repo"}},"base":{"ref":"main","sha":%q,"repo":{"id":123,"full_name":"owner/repo"}}}`, oldHead, liveBaseSHA)
		case "/repos/owner/repo/pulls/7/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/proof.test.ts"}]`)
		case "/repos/owner/repo/pulls/7/update-branch":
			mutations++
			http.Error(writer, "mutation forbidden", http.StatusInternalServerError)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	observed, err := client.InspectRepairPullRequestRefreshExact(context.Background(), RepairBranchRefreshRequest{
		Repository: "owner/repo", RepositoryID: "123", PRNumber: 7, Branch: "hive/repair-proof-a1", ExpectedHeadSHA: oldHead,
		ExpectedBaseBranch: "main", ExpectedBaseSHA: reviewedBaseSHA, ExpectedChangedFiles: []string{"tests/proof.test.ts"}, RepositoryFingerprint: "finding",
	})
	if err != nil || !observed.BaseMoved || observed.BaseSHA != liveBaseSHA || mutations != 0 {
		t.Fatalf("base movement was not observed read-only: observation=%+v mutations=%d err=%v", observed, mutations, err)
	}
}
