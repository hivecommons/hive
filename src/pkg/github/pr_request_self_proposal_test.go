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

// selfProposalServer serves a single issue (by number) plus its comments,
// matching the routes validateSelfProposalPRRequest actually calls:
// GET /repos/{owner}/{repo}/issues/{n} and GET .../issues/{n}/comments.
func selfProposalServer(t *testing.T, issue map[string]any, comments []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode(comments)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues/"):
			_ = json.NewEncoder(w).Encode(issue)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func selfProposalClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func botUser(login string) map[string]any {
	return map[string]any{"login": login, "type": "Bot"}
}

func humanUser(login string) map[string]any {
	return map[string]any{"login": login, "type": "User"}
}

// The reported incident's exact shape: a bot-filed proposal issue, zero
// comments, then a PR citing it with the non-closing "Refs #N" form architect
// policy itself instructs for multi-phase work. Must be rejected.
func TestValidateSelfProposalPRRequest_RejectsUnacknowledgedBotProposal_RefsForm(t *testing.T) {
	issue := map[string]any{"number": 581, "title": "[architect] decompose build-chain.sh", "user": botUser("hanthor-hive-agent[bot]")}
	srv := selfProposalServer(t, issue, nil)
	defer srv.Close()
	c := selfProposalClient(t, srv)

	reason, err := c.validateSelfProposalPRRequest(context.Background(), PRRequest{
		Repo: "o/r", Title: "[architect] refactor: extract native build backend",
		Body: "Extracts the native rpmbuild executor.\n\nRefs #581",
	})
	if err != nil {
		t.Fatalf("validateSelfProposalPRRequest: %v", err)
	}
	if reason == "" || !strings.Contains(reason, "581") {
		t.Fatalf("reason = %q, want a rejection naming issue 581", reason)
	}
}

// The same shape but with a closing keyword ("Fixes") must be caught too —
// the gate is not limited to the non-closing form.
func TestValidateSelfProposalPRRequest_RejectsUnacknowledgedBotProposal_FixesForm(t *testing.T) {
	issue := map[string]any{"number": 42, "title": "proposal", "user": botUser("hive[bot]")}
	srv := selfProposalServer(t, issue, nil)
	defer srv.Close()
	c := selfProposalClient(t, srv)

	reason, err := c.validateSelfProposalPRRequest(context.Background(), PRRequest{
		Repo: "o/r", Title: "implement the proposal", Body: "Fixes #42",
	})
	if err != nil {
		t.Fatalf("validateSelfProposalPRRequest: %v", err)
	}
	if reason == "" {
		t.Fatal("want a rejection, got none")
	}
}

// A human comment of ANY kind — not just an explicit approval — satisfies the
// gate: it is evidence a human looked at the proposal, which is the
// precondition being checked, not an approval classifier.
func TestValidateSelfProposalPRRequest_AllowsAfterAnyHumanComment(t *testing.T) {
	issue := map[string]any{"number": 581, "title": "proposal", "user": botUser("hive[bot]")}
	comments := []map[string]any{
		{"id": 1, "body": "hmm, not sure about this", "user": humanUser("a-maintainer")},
	}
	srv := selfProposalServer(t, issue, comments)
	defer srv.Close()
	c := selfProposalClient(t, srv)

	reason, err := c.validateSelfProposalPRRequest(context.Background(), PRRequest{
		Repo: "o/r", Title: "implement the proposal", Body: "Refs #581",
	})
	if err != nil {
		t.Fatalf("validateSelfProposalPRRequest: %v", err)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want no rejection once a human commented", reason)
	}
}

// A bot reply to its own proposal (e.g. a status update from another agent)
// must not count as human acknowledgement — that would make the gate
// trivially satisfiable by the hive commenting on its own issue.
func TestValidateSelfProposalPRRequest_BotCommentsDoNotCount(t *testing.T) {
	issue := map[string]any{"number": 581, "title": "proposal", "user": botUser("hive[bot]")}
	comments := []map[string]any{
		{"id": 1, "body": "starting phase 1", "user": botUser("hive[bot]")},
		{"id": 2, "body": "still working", "user": map[string]any{"login": "other-automation[bot]"}},
	}
	srv := selfProposalServer(t, issue, comments)
	defer srv.Close()
	c := selfProposalClient(t, srv)

	reason, err := c.validateSelfProposalPRRequest(context.Background(), PRRequest{
		Repo: "o/r", Title: "implement the proposal", Body: "Refs #581",
	})
	if err != nil {
		t.Fatalf("validateSelfProposalPRRequest: %v", err)
	}
	if reason == "" {
		t.Fatal("want a rejection, bot-only comments must not satisfy the gate")
	}
}

// A human-filed proposal issue is exactly the case this gate exists to leave
// alone: the precondition ("tracked rationale") is already met by a human
// having written the issue in the first place.
func TestValidateSelfProposalPRRequest_AllowsHumanFiledProposal(t *testing.T) {
	issue := map[string]any{"number": 12, "title": "please decompose this", "user": humanUser("a-maintainer")}
	srv := selfProposalServer(t, issue, nil)
	defer srv.Close()
	c := selfProposalClient(t, srv)

	reason, err := c.validateSelfProposalPRRequest(context.Background(), PRRequest{
		Repo: "o/r", Title: "implement the decomposition", Body: "Fixes #12",
	})
	if err != nil {
		t.Fatalf("validateSelfProposalPRRequest: %v", err)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want no rejection for a human-filed issue", reason)
	}
}

// A PR body with no issue reference at all triggers no API call and no
// rejection — this gate only fires on the shape it targets (a citation).
func TestValidateSelfProposalPRRequest_NoReferenceIsANoOp(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := selfProposalClient(t, srv)

	reason, err := c.validateSelfProposalPRRequest(context.Background(), PRRequest{
		Repo: "o/r", Title: "unrelated cleanup", Body: "no issue mentioned here",
	})
	if err != nil {
		t.Fatalf("validateSelfProposalPRRequest: %v", err)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want no rejection", reason)
	}
	if calls != 0 {
		t.Fatalf("calls = %d, want 0 — a PR with no issue reference must not hit the API", calls)
	}
}

// End-to-end: the watcher actually quarantines the request and records a
// result, exercising the same wiring the other three PR-request gates use.
func TestPRRequestWatcher_QuarantinesSelfProposal(t *testing.T) {
	created := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/581", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 581, "title": "proposal", "user": botUser("hive[bot]")})
	})
	mux.HandleFunc("/repos/o/r/issues/581/comments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc("/repos/o/r/compare/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ahead","files":[{"filename":"scripts/lib/build-chain/native.sh","patch":"@@ -0,0 +1 @@\n+echo hi"}]}`)
	})
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			created++
			_, _ = io.WriteString(w, `{"number":99}`)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := selfProposalClient(t, srv)
	c.prAuthz = func(string, int) error { return nil }

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()
	reqPath, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Base: "main", Head: "agent/change", Title: "[architect] refactor: extract module",
		Body: "Refs #581", Agent: "architect",
	})
	if err != nil {
		t.Fatal(err)
	}

	c.ProcessPRRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("self-proposal request created %d PRs, want 0", created)
	}
	if _, err := os.Stat(reqPath + ".rejected"); err != nil {
		t.Fatalf("rejected request was not quarantined: %v", err)
	}
}
