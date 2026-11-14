package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func derivedReadOnlyFixture() []config.ReferenceRepo {
	return []config.ReferenceRepo{
		{Repo: "acme/docs", DerivedFrom: "o/r#7", CredentialState: config.ReferenceCredentialReadOnly, CredentialReason: "the hive's GitHub App is not installed on acme"},
		{Repo: "acme/site", DerivedFrom: "o/r#7", CredentialState: config.ReferenceCredentialReadOnly, CredentialReason: "the hive's GitHub App is not installed on acme"},
	}
}

func TestDerivedReadOnlyMarkerIsASetKey(t *testing.T) {
	a := DerivedReadOnlyMarker([]string{"Acme/Site", "acme/docs", " acme/docs "})
	b := DerivedReadOnlyMarker([]string{"acme/docs", "acme/site"})
	if a != b {
		t.Fatalf("marker must not depend on order, case or duplicates:\n%s\n%s", a, b)
	}
	if !strings.HasPrefix(a, DerivedReadOnlyMarkerPrefix) || !strings.Contains(a, "repos=acme/docs,acme/site") || !strings.Contains(a, "state=read-only") || !strings.HasSuffix(a, "-->") {
		t.Fatalf("marker = %q", a)
	}
	if DerivedReadOnlyMarker([]string{"acme/docs"}) == a {
		t.Fatal("a different repo set must yield a different marker")
	}
}

func TestDerivedReadOnlyCommentBody(t *testing.T) {
	body := DerivedReadOnlyComment(derivedReadOnlyFixture())
	for _, want := range []string{DerivedReadOnlyMarkerPrefix, "`acme/docs`", "`acme/site`", "not installed on acme", "will not open issues or pull requests", AttributionTrailerPrefix} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "@") {
		t.Fatalf("comment must not mention anyone:\n%s", body)
	}
	if DerivedReadOnlyComment(nil) != "" {
		t.Fatal("no read-only entries must render nothing")
	}
}

// hivecommons/hive#18158: exactly one notice per (request issue, repo set),
// however many reloads happen; a wider set posts a second one.
func TestCommentDerivedReadOnlyPostsOnce(t *testing.T) {
	s := &statusCommentServer{}
	srv := s.start(t)
	c := testClient(t, srv.URL)
	c.SetAppBotLogin("hive-app[bot]")
	ctx := context.Background()
	readOnly := derivedReadOnlyFixture()

	posted, err := c.CommentDerivedReadOnly(ctx, "o/r", 7, readOnly)
	if err != nil || !posted {
		t.Fatalf("first notice posted=%v err=%v", posted, err)
	}
	for i := 0; i < 3; i++ {
		posted, err = c.CommentDerivedReadOnly(ctx, "o/r", 7, readOnly)
		if err != nil || posted {
			t.Fatalf("reload %d reposted: posted=%v err=%v", i, posted, err)
		}
	}
	if s.creates != 1 || s.edits != 0 || len(s.comments) != 1 {
		t.Fatalf("creates=%d edits=%d comments=%d, want 1/0/1", s.creates, s.edits, len(s.comments))
	}
	body, _ := s.comments[0]["body"].(string)
	if !strings.Contains(body, DerivedReadOnlyMarker([]string{"acme/docs", "acme/site"})) {
		t.Fatalf("comment body lacks the marker:\n%s", body)
	}

	// Same set in another order is the same notice.
	posted, err = c.CommentDerivedReadOnly(ctx, "o/r", 7, []config.ReferenceRepo{readOnly[1], readOnly[0]})
	if err != nil || posted || s.creates != 1 {
		t.Fatalf("reordered set reposted: posted=%v err=%v creates=%d", posted, err, s.creates)
	}
	// A later grant that adds a repo is a new repo set and a new notice.
	wider := append(readOnly, config.ReferenceRepo{Repo: "acme/legacy", DerivedFrom: "o/r#7", CredentialState: config.ReferenceCredentialReadOnly})
	posted, err = c.CommentDerivedReadOnly(ctx, "o/r", 7, wider)
	if err != nil || !posted || s.creates != 2 {
		t.Fatalf("wider set: posted=%v err=%v creates=%d", posted, err, s.creates)
	}
	// Nothing read-only, nothing posted.
	posted, err = c.CommentDerivedReadOnly(ctx, "o/r", 7, nil)
	if err != nil || posted || s.creates != 2 {
		t.Fatalf("empty set: posted=%v err=%v creates=%d", posted, err, s.creates)
	}
	if _, err := (*Client)(nil).CommentDerivedReadOnly(ctx, "o/r", 7, readOnly); err == nil {
		t.Fatal("nil client must refuse")
	}
}

// requestIssueServer fakes the three GitHub endpoints request-issue
// discovery reads: the labelled issue list, an issue's timeline and a
// collaborator's permission.
func requestIssueServer(t *testing.T, issuesJSON, timelineJSON, permissionJSON string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("labels"); got != "triage/accepted" {
			t.Errorf("labels filter = %q, want triage/accepted", got)
		}
		if got := r.URL.Query().Get("state"); got != "open" {
			t.Errorf("state filter = %q, want open", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(issuesJSON))
	})
	mux.HandleFunc("/repos/o/r/issues/395/timeline", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(timelineJSON))
	})
	mux.HandleFunc("/repos/o/r/collaborators/maintainer/permission", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(permissionJSON))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const requestIssueListJSON = `[
 {"number":395,"title":"Assessment request","body":"### Website or documentation repository\nhttps://github.com/acme/docs\n","state":"open",
  "labels":[{"name":"triage/accepted"},{"name":"kind/assessment"}],"updated_at":"2026-11-01T10:00:00Z"},
 {"number":400,"title":"a PR","body":"","state":"open","labels":[{"name":"triage/accepted"}],"updated_at":"2026-11-02T10:00:00Z",
  "pull_request":{"url":"https://api.github.com/repos/o/r/pulls/400"}}
]`

const requestIssueTimelineJSON = `[
 {"event":"labeled","created_at":"2026-10-01T00:00:00Z","label":{"name":"triage/accepted"},"actor":{"login":"hive-app[bot]","type":"Bot"}},
 {"event":"unlabeled","created_at":"2026-10-02T00:00:00Z","label":{"name":"triage/accepted"},"actor":{"login":"maintainer","type":"User"}},
 {"event":"labeled","created_at":"2026-10-03T00:00:00Z","label":{"name":"kind/assessment"},"actor":{"login":"someone","type":"User"}},
 {"event":"labeled","created_at":"2026-10-04T00:00:00Z","label":{"name":"triage/accepted"},"actor":{"login":"maintainer","type":"User"}}
]`

func TestListRequestIssueCandidates(t *testing.T) {
	srv := requestIssueServer(t, requestIssueListJSON, "[]", "{}")
	c := testClient(t, srv.URL)
	got, err := c.ListRequestIssueCandidates(context.Background(), "o/r", "triage/accepted", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("pull requests must be skipped; got %d candidates: %+v", len(got), got)
	}
	cand := got[0]
	if cand.Repo != "o/r" || cand.Number != 395 || cand.Closed || !strings.Contains(cand.Body, "acme/docs") {
		t.Errorf("candidate = %+v", cand)
	}
	if len(cand.Labels) != 2 || cand.Labels[0] != "triage/accepted" {
		t.Errorf("labels = %v", cand.Labels)
	}
	if cand.UpdatedAt.IsZero() {
		t.Error("updated_at must be carried: it is the cache key")
	}
}

// The latest "labeled" event for the grant label names the accepter, not an
// earlier application by a bot and not a labeled event for another label.
func TestRequestIssueAccepterForLatestLabeledEvent(t *testing.T) {
	srv := requestIssueServer(t, "[]", requestIssueTimelineJSON, `{"permission":"maintain","user":{"login":"maintainer"}}`)
	c := testClient(t, srv.URL)
	acc, err := c.RequestIssueAccepterFor(context.Background(), "o/r", 395, "triage/accepted")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Login != "maintainer" || acc.IsBot || acc.Permission != "maintain" {
		t.Fatalf("accepter = %+v, want maintainer/maintain/human", acc)
	}
}

func TestRequestIssueAccepterForNoLabeledEvent(t *testing.T) {
	srv := requestIssueServer(t, "[]", `[{"event":"commented","created_at":"2026-10-01T00:00:00Z","actor":{"login":"x"}}]`, "{}")
	c := testClient(t, srv.URL)
	acc, err := c.RequestIssueAccepterFor(context.Background(), "o/r", 395, "triage/accepted")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Login != "" || acc.Permission != "" {
		t.Fatalf("an unattributed grant must come back empty, got %+v", acc)
	}
}
