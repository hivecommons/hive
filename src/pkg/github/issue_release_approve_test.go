package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type releaseApprovalFixture struct {
	mu              sync.Mutex
	head            string
	compareStatus   string
	tagExists       bool
	checkConclusion string
	version         string
	creates         int
	posted          []string
	added           []string
	removed         []string
	createBody      map[string]any
}

func releaseApprovalIssue(comment unparkWireComment) unparkWireIssue {
	step := testReleaseStep()
	return unparkWireIssue{
		Number:   42,
		Title:    "release v0.12.0",
		Body:     "Remaining step: cut the release.\n\n" + step.marker(),
		User:     wireUser{Login: "hive[bot]"},
		Labels:   []wireLabel{{Name: issueNeedsDecisionLabel}, {Name: "hold"}},
		Comments: []unparkWireComment{comment},
	}
}

func newReleaseApprovalServer(t *testing.T, issue unparkWireIssue, fx *releaseApprovalFixture, permission string) *httptest.Server {
	t.Helper()
	if fx.head == "" {
		fx.head = testReleaseStep().Target
	}
	if fx.compareStatus == "" {
		fx.compareStatus = "ahead"
	}
	if fx.checkConclusion == "" {
		fx.checkConclusion = "success"
	}
	if fx.version == "" {
		fx.version = "0.12.0\n"
	}

	base := "/repos/org/repo/"
	mux := http.NewServeMux()
	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, base)
		fx.mu.Lock()
		defer fx.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		switch {
		case path == "" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "repo", "default_branch": "main"})
		case path == "issues" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]unparkWireIssue{issue})
		case path == "issues/42/comments" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(issue.Comments)
		case path == "issues/42/comments" && r.Method == http.MethodPost:
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fx.posted = append(fx.posted, body.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9000 + len(fx.posted), "body": body.Body, "user": map[string]string{"login": "hive[bot]"}})
		case strings.HasPrefix(path, "issues/comments/") && r.Method == http.MethodPatch:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 200})
		case path == "issues/42/labels" && r.Method == http.MethodPost:
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			fx.added = append(fx.added, labels...)
			_ = json.NewEncoder(w).Encode([]wireLabel{})
		case strings.HasPrefix(path, "issues/42/labels/") && r.Method == http.MethodDelete:
			fx.removed = append(fx.removed, strings.TrimPrefix(path, "issues/42/labels/"))
			_ = json.NewEncoder(w).Encode([]wireLabel{})
		case path == "collaborators/maintainer/permission" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]string{"permission": permission})
		case path == "branches/main" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "commit": map[string]string{"sha": fx.head}})
		case strings.HasPrefix(path, "compare/") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]string{"status": fx.compareStatus})
		case path == "git/ref/tags/v0.12.0" && r.Method == http.MethodGet:
			if !fx.tagExists {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/tags/v0.12.0", "object": map[string]string{"sha": testReleaseStep().Target}})
		case path == "branches/main/protection/required_status_checks" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"strict": true, "contexts": []string{"ci"}})
		case strings.HasPrefix(path, "commits/") && strings.HasSuffix(path, "/status") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "statuses": []any{}})
		case strings.HasPrefix(path, "commits/") && strings.HasSuffix(path, "/check-runs") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"id": 1, "name": "ci", "status": "completed", "conclusion": fx.checkConclusion}}})
		case path == "contents/VERSION" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "file", "path": "VERSION", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(fx.version))})
		case path == "releases" && r.Method == http.MethodPost:
			fx.creates++
			fx.tagExists = true
			_ = json.NewDecoder(r.Body).Decode(&fx.createBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "tag_name": "v0.12.0", "target_commitish": testReleaseStep().Target, "html_url": "https://github.com/org/repo/releases/tag/v0.12.0"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func releaseApprovalOptions() IssueUnparkSweepOptions {
	return IssueUnparkSweepOptions{
		ReleaseOnApprove:     func(string) bool { return true },
		ReleaseVersionSource: func(string) string { return "VERSION" },
	}
}

func TestReleaseApprovalCreatesOncePostsURLAuditsAndClearsPark(t *testing.T) {
	command := humanComment(101, "maintainer", "/hive approve")
	issue := releaseApprovalIssue(command)
	fx := &releaseApprovalFixture{}
	server := newReleaseApprovalServer(t, issue, fx, "write")
	client := newTestClient(t, server, "org", []string{"repo"})
	client.SetAppBotLogin("hive[bot]")
	var auditAction, auditDetail string
	client.SetAttributionHooks(AttributionHooks{Audit: func(action, detail, _ string) {
		auditAction, auditDetail = action, detail
	}})

	result, err := client.SweepIssueUnparkCommands(context.Background(), releaseApprovalOptions())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if fx.creates != 1 {
		t.Fatalf("release creates = %d, want 1", fx.creates)
	}
	if got := fmt.Sprint(fx.createBody["tag_name"]); got != "v0.12.0" {
		t.Fatalf("created tag = %q", got)
	}
	if got := fmt.Sprint(fx.createBody["target_commitish"]); got != testReleaseStep().Target {
		t.Fatalf("created target = %q", got)
	}
	if len(result.Unparked) != 1 || result.Unparked[0].ReleaseTag != "v0.12.0" || result.Unparked[0].ReleaseURL == "" {
		t.Fatalf("unpark result = %+v", result.Unparked)
	}
	posted := strings.Join(fx.posted, "\n")
	if !strings.Contains(posted, "https://github.com/org/repo/releases/tag/v0.12.0") || !strings.Contains(posted, unparkAcceptedMarker) {
		t.Fatalf("success reply = %q", posted)
	}
	if !containsFold(fx.removed, issueNeedsDecisionLabel) || containsFold(fx.removed, "hold") {
		t.Fatalf("removed labels = %v", fx.removed)
	}
	if len(fx.added) != 1 || fx.added[0] != HumanAckLabel {
		t.Fatalf("added labels = %v", fx.added)
	}
	if auditAction != AuditActionReleaseCreated || !strings.Contains(auditDetail, "tag=v0.12.0") || !strings.Contains(auditDetail, "target="+testReleaseStep().Target) {
		t.Fatalf("audit = %q %q", auditAction, auditDetail)
	}

	// Replaying the same answered command is a no-op even though the tag now
	// exists. The executor must not attempt a second release or post a refusal.
	replayed := issue
	for i, body := range fx.posted {
		replayed.Comments = append(replayed.Comments, unparkWireComment{ID: int64(200 + i), Body: body, User: wireUser{Login: "hive[bot]"}, CreatedAt: unparkNow(), UpdatedAt: unparkNow()})
	}
	replayFixture := &releaseApprovalFixture{tagExists: true}
	replayServer := newReleaseApprovalServer(t, replayed, replayFixture, "write")
	replayClient := newTestClient(t, replayServer, "org", []string{"repo"})
	replayClient.SetAppBotLogin("hive[bot]")
	replayResult, err := replayClient.SweepIssueUnparkCommands(context.Background(), releaseApprovalOptions())
	if err != nil {
		t.Fatalf("replay sweep: %v", err)
	}
	if replayFixture.creates != 0 || len(replayResult.Unparked) != 0 || replayResult.Replies != 0 {
		t.Fatalf("replay created=%d unparked=%v replies=%d", replayFixture.creates, replayResult.Unparked, replayResult.Replies)
	}
}

func TestReleaseApprovalAcceptsNamedDefaultBranchAncestor(t *testing.T) {
	issue := releaseApprovalIssue(humanComment(101, "maintainer", "/hive approve"))
	fx := &releaseApprovalFixture{head: strings.Repeat("b", 40), compareStatus: "ahead"}
	server := newReleaseApprovalServer(t, issue, fx, "write")
	client := newTestClient(t, server, "org", []string{"repo"})

	result, err := client.SweepIssueUnparkCommands(context.Background(), releaseApprovalOptions())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if fx.creates != 1 || len(result.Unparked) != 1 {
		t.Fatalf("ancestor creates=%d unparked=%v", fx.creates, result.Unparked)
	}
}

func TestReleaseApprovalPreconditionRefusalsLeaveIssueParked(t *testing.T) {
	otherSHA := strings.Repeat("b", 40)
	cases := []struct {
		name string
		fx   releaseApprovalFixture
		want string
	}{
		{name: "moved head", fx: releaseApprovalFixture{head: otherSHA, compareStatus: "diverged"}, want: "no longer the `main` head or an ancestor"},
		{name: "existing tag", fx: releaseApprovalFixture{tagExists: true}, want: "tag `v0.12.0` already exists"},
		{name: "red required check", fx: releaseApprovalFixture{checkConclusion: "failure"}, want: "required checks are not green"},
		{name: "version mismatch", fx: releaseApprovalFixture{version: "0.13.0\n"}, want: "does not match `0.13.0`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := releaseApprovalIssue(humanComment(101, "maintainer", "/hive approve"))
			fx := tc.fx
			server := newReleaseApprovalServer(t, issue, &fx, "write")
			client := newTestClient(t, server, "org", []string{"repo"})

			result, err := client.SweepIssueUnparkCommands(context.Background(), releaseApprovalOptions())
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if fx.creates != 0 || len(result.Unparked) != 0 || result.Replies != 1 {
				t.Fatalf("creates=%d unparked=%v replies=%d", fx.creates, result.Unparked, result.Replies)
			}
			if len(fx.removed) != 0 || len(fx.added) != 0 {
				t.Fatalf("park changed: removed=%v added=%v", fx.removed, fx.added)
			}
			posted := strings.Join(fx.posted, "\n")
			if !strings.Contains(posted, tc.want) || !strings.Contains(posted, "issue remains parked") {
				t.Fatalf("reply = %q, want %q", posted, tc.want)
			}
		})
	}
}

func TestReleaseApprovalDefaultsOff(t *testing.T) {
	issue := releaseApprovalIssue(humanComment(101, "maintainer", "/hive approve"))
	fx := &releaseApprovalFixture{}
	server := newReleaseApprovalServer(t, issue, fx, "write")
	client := newTestClient(t, server, "org", []string{"repo"})

	result, err := client.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if fx.creates != 0 || len(result.Unparked) != 1 {
		t.Fatalf("default-off creates=%d unparked=%v", fx.creates, result.Unparked)
	}
}
func TestReleaseApprovalIgnoresIneligibleCommenter(t *testing.T) {
	issue := releaseApprovalIssue(humanComment(101, "maintainer", "/hive approve"))
	fx := &releaseApprovalFixture{}
	server := newReleaseApprovalServer(t, issue, fx, "read")
	client := newTestClient(t, server, "org", []string{"repo"})

	result, err := client.SweepIssueUnparkCommands(context.Background(), releaseApprovalOptions())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if fx.creates != 0 || len(result.Unparked) != 0 || result.Replies != 0 || len(fx.removed) != 0 {
		t.Fatalf("creates=%d unparked=%v replies=%d removed=%v", fx.creates, result.Unparked, result.Replies, fx.removed)
	}
	for _, body := range fx.posted {
		if !strings.Contains(body, unparkNoticeMarker) {
			t.Fatalf("ineligible commenter received reply %q", body)
		}
	}
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}
