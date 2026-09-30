package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// newReviewRequestMockServer records the reviewer requests a request_review
// request makes.
func newReviewRequestMockServer(t *testing.T, calls *int, reviewers, teams *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/requested_reviewers") {
			var body struct {
				Reviewers     []string `json:"reviewers"`
				TeamReviewers []string `json:"team_reviewers"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if calls != nil {
				*calls++
			}
			if reviewers != nil {
				*reviewers = append(*reviewers, body.Reviewers...)
			}
			if teams != nil {
				*teams = append(*teams, body.TeamReviewers...)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"number":42}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

// A "request_review" request asks the named users and teams to review the PR
// and audits the change with typed repo/target (#9587).
func TestIssueRequestWatcher_RequestReviewRequestsAndAudits(t *testing.T) {
	var calls int
	var reviewers, teams []string
	srv := newReviewRequestMockServer(t, &calls, &reviewers, &teams)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	recs := captureAudit(c)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "request_review", Repo: "o/r", Number: 42, Agent: "scanner",
		Reviewers:     []string{"@alice", "alice", " bob "},
		TeamReviewers: []string{"o/core"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if calls != 1 {
		t.Fatalf("reviewer API calls = %d, want 1", calls)
	}
	if got := strings.Join(reviewers, ","); got != "alice,bob" {
		t.Errorf("reviewers requested = %q, want %q (trimmed, @ stripped, de-duplicated)", got, "alice,bob")
	}
	if got := strings.Join(teams, ","); got != "core" {
		t.Errorf("team reviewers requested = %q, want %q (org/ prefix stripped)", got, "core")
	}
	resp := readIssueResultFile(t, reqPath)
	if !resp.OK || resp.Number != 42 {
		t.Fatalf("result = %+v, want ok on #42", resp)
	}
	if strings.Join(resp.ReviewersRequested, ",") != "alice,bob" || strings.Join(resp.TeamReviewersRequested, ",") != "core" {
		t.Errorf("result does not report who was asked: %+v", resp)
	}
	rec, ok := findAudit(*recs, AuditActionAgentReviewRequested)
	if !ok {
		t.Fatalf("review request was not audited; records: %+v", *recs)
	}
	if rec.Repo != "o/r" || rec.Target != 42 || rec.Agent != "scanner" {
		t.Errorf("audit typed fields = %+v, want repo=o/r target=42 agent=scanner", rec)
	}
	if !strings.Contains(rec.Detail, "reviewers=alice bob") || !strings.Contains(rec.Detail, "team_reviewers=core") {
		t.Errorf("audit detail = %q, want the requested reviewers and teams", rec.Detail)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Error("a fulfilled request_review request should be consumed")
	}
}

// The lane allowlist governs the new operation like every other one.
func TestIssueRequestWatcher_RequestReviewRefusedOutsideLaneAllowlist(t *testing.T) {
	var calls int
	srv := newReviewRequestMockServer(t, &calls, nil, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetWriteAllowlistFunc(allowOnly("scanner", WriteOpComment, WriteOpLabel))
	recs := captureAudit(c)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "request_review", Repo: "o/r", Number: 11, Agent: "scanner", Reviewers: []string{"alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if calls != 0 {
		t.Fatalf("%d reviewer requests made by a lane not allowed to request review, want 0", calls)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("refused request was not quarantined: %v", err)
	}
	if resp := readIssueResultFile(t, reqPath); resp.OK || !strings.Contains(resp.Error, WriteOpRequestReview) {
		t.Errorf("result does not name the refused operation: %+v", resp)
	}
	assertRefusalAudited(t, *recs, "scanner", WriteOpRequestReview, "o/r", 11)
	if _, ok := findAudit(*recs, AuditActionAgentReviewRequested); ok {
		t.Error("a refused request was audited as a review request")
	}
}

// A request_review request that can never succeed is quarantined rather than
// retried for a day, and never reaches GitHub.
func TestIssueRequestWatcher_RequestReviewMalformed(t *testing.T) {
	tooMany := make([]string, 0, maxReviewRequestReviewers+1)
	for i := 0; i <= maxReviewRequestReviewers; i++ {
		tooMany = append(tooMany, "user"+string(rune('a'+i)))
	}
	for name, req := range map[string]IssueRequest{
		"no reviewers": {Kind: "request_review", Repo: "o/r", Number: 3, Agent: "scanner"},
		"no number":    {Kind: "request_review", Repo: "o/r", Agent: "scanner", Reviewers: []string{"alice"}},
		"no agent":     {Kind: "request_review", Repo: "o/r", Number: 3, Reviewers: []string{"alice"}},
		"empty list": {Kind: "request_review", Repo: "o/r", Number: 3, Agent: "scanner",
			Reviewers: []string{" ", ","}},
		"bad login": {Kind: "request_review", Repo: "o/r", Number: 3, Agent: "scanner",
			Reviewers: []string{"alice; rm -rf"}},
		"bad team": {Kind: "request_review", Repo: "o/r", Number: 3, Agent: "scanner",
			TeamReviewers: []string{"core team"}},
		"too many": {Kind: "request_review", Repo: "o/r", Number: 3, Agent: "scanner",
			Reviewers: tooMany},
	} {
		t.Run(name, func(t *testing.T) {
			var calls int
			srv := newReviewRequestMockServer(t, &calls, nil, nil)
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			dir := withIssueDir(t)
			reqPath, err := WriteIssueRequest(dir, req)
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())
			if _, err := os.Stat(reqPath + ".bad"); err != nil {
				t.Errorf("malformed request_review request should be quarantined as .bad")
			}
			if calls != 0 {
				t.Errorf("malformed request reached GitHub %d times", calls)
			}
		})
	}
}

func TestReviewRequestShapeError(t *testing.T) {
	for _, login := range []string{"alice", "a", "bob-smith", "dependabot[bot]", strings.Repeat("a", 39)} {
		if msg := reviewRequestShapeError([]string{login}, nil); msg != "" {
			t.Errorf("login %q rejected: %s", login, msg)
		}
	}
	for _, login := range []string{"-alice", "alice-", "al--ice", "al ice", "al/ice", strings.Repeat("a", 40)} {
		if msg := reviewRequestShapeError([]string{login}, nil); msg == "" {
			t.Errorf("login %q accepted, want rejected", login)
		}
	}
	for _, slug := range []string{"core", "core-maintainers", "team_1.x"} {
		if msg := reviewRequestShapeError(nil, []string{slug}); msg != "" {
			t.Errorf("team %q rejected: %s", slug, msg)
		}
	}
	for _, slug := range []string{"-core", "core team", "a/b"} {
		if msg := reviewRequestShapeError(nil, []string{slug}); msg == "" {
			t.Errorf("team %q accepted, want rejected", slug)
		}
	}
}

func TestNormalizeReviewers(t *testing.T) {
	if got := normalizeReviewers([]string{" @alice , ALICE", "", "bob"}); strings.Join(got, ",") != "alice,bob" {
		t.Errorf("normalizeReviewers = %v, want [alice bob]", got)
	}
	if got := normalizeTeamReviewers([]string{"org/core", "@org/core", "docs"}); strings.Join(got, ",") != "core,docs" {
		t.Errorf("normalizeTeamReviewers = %v, want [core docs]", got)
	}
	if normalizeReviewers(nil) != nil || normalizeReviewers([]string{" ", ","}) != nil {
		t.Error("an empty list must normalize to nil")
	}
}

func TestRequestReviewersNoop(t *testing.T) {
	var nilClient *Client
	if err := nilClient.RequestReviewers(context.Background(), "o/r", 1, []string{"a"}, nil); err == nil {
		t.Error("nil client should return an error")
	}
	c := issueTestClient(t, "http://127.0.0.1:0")
	if err := c.RequestReviewers(context.Background(), "o/r", 1, nil, nil); err != nil {
		t.Errorf("empty request should be a no-op, got %v", err)
	}
}
