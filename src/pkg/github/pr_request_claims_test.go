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

// TestValidatePRRequestClaims_DowngradesHumanFiledBugWithoutConfirmation is the
// break-it-proof for kubestellar/hive#6781. Before the humanFiledBugReason
// gate, a human maintainer's bug labeled "bug" with no hive attribution
// trailer was auto-closed on merge via "Closes #N", leaving the reporter
// unable to reopen (the App bot was the closer). The regression that
// #6500 → #6762/#6767 was filed against was exactly this shape.
//
// With the gate, that Closes # is downgraded to Refs #. Neutering
// humanFiledBugReason to return "" restores the old behaviour and fails this
// test — proving the enforcement is behavioural, not documentation.
func TestValidatePRRequestClaims_DowngradesHumanFiledBugWithoutConfirmation(t *testing.T) {
	// A maintainer-filed bug: has the "bug" label, no hive attribution
	// trailer in the body, and User.Type is "User" (not "Bot").
	issue := map[string]any{
		"number": 6500,
		"title":  "Copilot license check false-fails",
		"body":   "The strategist agent claims no Copilot license. Steps to reproduce:\n1. Do X\n2. Y happens",
		"state":  "open",
		"labels": []map[string]string{{"name": "bug"}},
		"user":   map[string]any{"login": "MikeSpreitzer", "type": "User"},
	}
	srv := claimValidationServer(t, nil, issue)
	defer srv.Close()
	c := claimValidationClient(t, srv)

	title, body, err := c.validatePRRequestClaims(context.Background(), PRRequest{
		Repo: "o/r", Head: "agent/fix", Title: "fix copilot check", Body: "Closes #6500\n\nDetails",
	})
	if err != nil {
		t.Fatalf("validatePRRequestClaims: %v", err)
	}
	if body != "Refs #6500\n\nDetails" {
		t.Fatalf("body was not downgraded (Closes # would let the App bot auto-close a maintainer bug on merge, and the reporter cannot reopen): got %q", body)
	}
	if title != "fix copilot check" {
		t.Fatalf("title mutated unexpectedly: %q", title)
	}
}

// TestValidatePRRequestClaims_LeavesAgentFiledBugClosingReference confirms the
// gate is scoped: an agent's own bug-labeled finding still auto-closes on
// merge. Detection: the body carries AttributionTrailerPrefix.
func TestValidatePRRequestClaims_LeavesAgentFiledBugClosingReference(t *testing.T) {
	issue := map[string]any{
		"number": 6781,
		"title":  "[strategist] closed-unverified loop",
		"body":   "Details.\n\n— hive: agent=strategist backend=copilot model=claude-sonnet-4-6",
		"state":  "open",
		"labels": []map[string]string{{"name": "bug"}, {"name": "agent/strategist"}},
		"user":   map[string]any{"login": "kubestellar-hive", "type": "Bot"},
	}
	srv := claimValidationServer(t, nil, issue)
	defer srv.Close()
	c := claimValidationClient(t, srv)

	title, body, err := c.validatePRRequestClaims(context.Background(), PRRequest{
		Repo: "o/r", Head: "agent/fix", Title: "fix loop", Body: "Closes #6781",
	})
	if err != nil {
		t.Fatalf("validatePRRequestClaims: %v", err)
	}
	if title != "fix loop" || body != "Closes #6781" {
		t.Fatalf("agent-filed bug should still be closeable: %q / %q", title, body)
	}
}

// TestValidatePRRequestClaims_HonoursReporterConfirmation confirms that a
// maintainer/reporter can opt in to auto-close by dropping the
// humanFiledBugConfirmationMarker in the body — a documented, low-friction
// override that keeps the gate from becoming a hard block on all human bugs.
func TestValidatePRRequestClaims_HonoursReporterConfirmation(t *testing.T) {
	issue := map[string]any{
		"number": 42,
		"title":  "reporter-confirmed bug",
		"body":   "Steps to reproduce ...\n\nhive: reporter-confirmed",
		"state":  "open",
		"labels": []map[string]string{{"name": "bug"}},
		"user":   map[string]any{"login": "some-maintainer", "type": "User"},
	}
	srv := claimValidationServer(t, nil, issue)
	defer srv.Close()
	c := claimValidationClient(t, srv)

	title, body, err := c.validatePRRequestClaims(context.Background(), PRRequest{
		Repo: "o/r", Head: "agent/fix", Title: "fix it", Body: "Closes #42",
	})
	if err != nil {
		t.Fatalf("validatePRRequestClaims: %v", err)
	}
	if title != "fix it" || body != "Closes #42" {
		t.Fatalf("reporter-confirmed bug should remain Closes-able: %q / %q", title, body)
	}
}
