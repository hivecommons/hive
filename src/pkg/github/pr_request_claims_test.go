package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func claimValidationServer(t *testing.T, files []string, issue map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/compare/"):
			changed := make([]map[string]string, 0, len(files))
			for _, file := range files {
				changed = append(changed, map[string]string{"filename": file, "status": "modified"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "files": changed})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues/"):
			_ = json.NewEncoder(w).Encode(issue)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func claimValidationClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestValidatePRRequestClaims_ArtifactTitleMatchesDiff(t *testing.T) {
	tests := []struct {
		name  string
		title string
		file  string
	}{
		{"workflow", "ci: add upstream sync workflow", ".github/workflows/sync.yml"},
		{"test", "test: cover retry behavior", "src/retry_test.go"},
		{"cross-language test", "test: cover sync helper", "build-aux/sync_test.py"},
		{"migration", "db: add user migration", "db/migrations/001_users.sql"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := claimValidationServer(t, []string{tt.file}, nil)
			defer srv.Close()
			c := claimValidationClient(t, srv)
			gotTitle, _, err := c.validatePRRequestClaims(context.Background(), PRRequest{
				Repo: "o/r", Base: "main", Head: "agent/change", Title: tt.title,
			})
			if err != nil {
				t.Fatalf("validatePRRequestClaims: %v", err)
			}
			if gotTitle != tt.title {
				t.Fatalf("title = %q, want %q", gotTitle, tt.title)
			}
		})
	}
}

func TestValidatePRRequestClaims_RejectsMissingClaimedArtifact(t *testing.T) {
	srv := claimValidationServer(t, []string{"docs/UPSTREAM.md", "build-aux/sync-upstream.sh"}, nil)
	defer srv.Close()
	c := claimValidationClient(t, srv)

	_, _, err := c.validatePRRequestClaims(context.Background(), PRRequest{
		Repo: "o/r", Base: "main", Head: "agent/change", Title: "ci: add upstream sync workflow",
	})
	reason, ok := prRequestPolicyReason(err)
	if !ok || reason != "title claims workflow but diff contains no workflow file" {
		t.Fatalf("error = %v (%q, policy=%v)", err, reason, ok)
	}
}

func TestPRRequestWatcher_QuarantinesArtifactClaimMismatch(t *testing.T) {
	created := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/compare/"):
			_, _ = io.WriteString(w, `{"status":"ahead","files":[{"filename":"docs/UPSTREAM.md"}]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			created++
			_, _ = io.WriteString(w, `{"number":42}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := claimValidationClient(t, srv)
	c.prAuthz = func(string, int) error { return nil }

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()
	reqPath, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Base: "main", Head: "agent/change", Title: "ci: add upstream sync workflow", Body: "adds the workflow", Agent: "scanner",
	})
	if err != nil {
		t.Fatal(err)
	}

	c.ProcessPRRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("mismatched title created %d PRs, want 0", created)
	}
	if _, err := os.Stat(reqPath + ".rejected"); err != nil {
		t.Fatalf("rejected request was not quarantined: %v", err)
	}
	result, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), "title claims workflow but diff contains no workflow file") {
		t.Fatalf("result lacks actionable mismatch: %s", result)
	}
}

func TestValidatePRRequestClaims_DowngradesIncompleteIssues(t *testing.T) {
	tests := []struct {
		name  string
		issue map[string]any
	}{
		{"unchecked task", map[string]any{"number": 60, "title": "work", "body": "- [x] first\n- [ ] remaining", "state": "open"}},
		{"epic label", map[string]any{"number": 60, "title": "work", "body": "several phases", "state": "open", "labels": []map[string]string{{"name": "epic"}}}},
		{"tracker title", map[string]any{"number": 60, "title": "[Tracker] program", "body": "children", "state": "open"}},
		{"epic title", map[string]any{"number": 60, "title": "[EPIC] program", "body": "children", "state": "open"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := claimValidationServer(t, nil, tt.issue)
			defer srv.Close()
			c := claimValidationClient(t, srv)
			title, body, err := c.validatePRRequestClaims(context.Background(), PRRequest{
				Repo: "o/r", Head: "agent/change", Title: "Fixes #60: partial work", Body: "Closes: #60\n\nDetails",
			})
			if err != nil {
				t.Fatalf("validatePRRequestClaims: %v", err)
			}
			if title != "Refs #60: partial work" || body != "Refs: #60\n\nDetails" {
				t.Fatalf("downgraded title/body = %q / %q", title, body)
			}
		})
	}
}

func TestValidatePRRequestClaims_LeavesCompleteIssueClosingReference(t *testing.T) {
	issue := map[string]any{"number": 7, "title": "focused bug", "body": "One concrete acceptance criterion", "state": "open"}
	srv := claimValidationServer(t, nil, issue)
	defer srv.Close()
	c := claimValidationClient(t, srv)

	title, body, err := c.validatePRRequestClaims(context.Background(), PRRequest{
		Repo: "o/r", Head: "agent/change", Title: "fix: focused bug", Body: "Fixes #7",
	})
	if err != nil {
		t.Fatalf("validatePRRequestClaims: %v", err)
	}
	if title != "fix: focused bug" || body != "Fixes #7" {
		t.Fatalf("title/body changed: %q / %q", title, body)
	}
}
