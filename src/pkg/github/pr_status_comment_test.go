package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/review"
)

// statusCommentServer is a GitHub stand-in that keeps issue comments: POST
// appends, PATCH rewrites, GET lists. It counts creates and edits so a test
// can prove the second write edited in place instead of posting again.
type statusCommentServer struct {
	mu       sync.Mutex
	comments []map[string]any
	creates  int
	edits    int
	deletes  []string
}

func (s *statusCommentServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode(s.comments)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.creates++
			// GitHub stamps the authenticated App as the comment's user.
			cm := map[string]any{"id": 100 + s.creates, "body": body["body"], "user": map[string]any{"login": "hive-app[bot]", "type": "Bot"}}
			s.comments = append(s.comments, cm)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(cm)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/issues/comments/"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.edits++
			for _, cm := range s.comments {
				if strings.HasSuffix(r.URL.Path, "/"+jsonNumber(cm["id"])) {
					cm["body"] = body["body"]
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/issues/comments/"):
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			s.deletes = append(s.deletes, id)
			kept := s.comments[:0]
			for _, cm := range s.comments {
				if jsonNumber(cm["id"]) != id {
					kept = append(kept, cm)
				}
			}
			s.comments = kept
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonNumber(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The first section creates the status comment; every later section, and
// every change to an existing one, edits that comment. Nothing is posted
// twice, and an unchanged section is not even an edit.
func TestUpsertPRStatusSectionCreatesOnceThenEdits(t *testing.T) {
	s := &statusCommentServer{}
	srv := s.start(t)
	c := testClient(t, srv.URL)
	c.SetAppBotLogin("hive-app[bot]")
	ctx := context.Background()

	created, err := c.UpsertPRStatusSection(ctx, "o/r", 5, prstatus.KeyStageStalled, "stalled at 080")
	if err != nil || !created {
		t.Fatalf("first upsert: created=%v err=%v", created, err)
	}
	created, err = c.UpsertPRStatusSection(ctx, "o/r", 5, prstatus.KeyStageRegressed, "080 → 071")
	if err != nil || created {
		t.Fatalf("second section must edit, not create: created=%v err=%v", created, err)
	}
	created, err = c.UpsertPRStatusSection(ctx, "o/r", 5, prstatus.KeyStageStalled, "stalled at 080")
	if err != nil || created {
		t.Fatalf("unchanged section: created=%v err=%v", created, err)
	}
	if s.creates != 1 || s.edits != 1 {
		t.Fatalf("creates=%d edits=%d, want 1 create and 1 edit", s.creates, s.edits)
	}
	body, _ := s.comments[0]["body"].(string)
	if !prstatus.IsStatusComment(body) {
		t.Fatalf("not a status comment: %q", body)
	}
	for _, key := range []string{prstatus.KeyStageStalled, prstatus.KeyStageRegressed} {
		if _, ok := prstatus.Get(body, key); !ok {
			t.Fatalf("section %q missing from %q", key, body)
		}
	}
	if created, err := c.UpsertPRStatusSection(ctx, "o/r", 5, prstatus.KeyStageStalled, ""); err != nil || created || s.edits != 2 {
		t.Fatalf("clearing a section must edit: created=%v err=%v edits=%d", created, err, s.edits)
	}
	body, _ = s.comments[0]["body"].(string)
	if _, ok := prstatus.Get(body, prstatus.KeyStageStalled); ok {
		t.Fatalf("cleared section still present: %q", body)
	}
}

// A PR carrying two hive status comments (two writers both created one)
// is reconciled on the next write: every section lands in the oldest, the
// newer hive-authored copy is deleted, and the edit is tallied as an edit,
// not a created comment.
func TestUpsertPRStatusSectionMergesDuplicateStatusComments(t *testing.T) {
	bot := map[string]any{"login": "hive-app[bot]", "type": "Bot"}
	s := &statusCommentServer{comments: []map[string]any{
		{"id": 11, "body": prstatus.Upsert("", prstatus.KeyStageStalled, "stalled"), "user": bot},
		{"id": 12, "body": prstatus.Upsert("", prstatus.KeyStageRegressed, "regressed"), "user": bot},
	}}
	c := testClient(t, s.start(t).URL)
	c.SetAppBotLogin("hive-app[bot]")
	created, err := c.UpsertPRStatusSection(context.Background(), "o/r", 5, prstatus.KeyEvidence, "evidence")
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if s.creates != 0 || len(s.deletes) != 1 || s.deletes[0] != "12" || len(s.comments) != 1 {
		t.Fatalf("creates=%d deletes=%v comments=%d, want the newer status comment deleted", s.creates, s.deletes, len(s.comments))
	}
	body, _ := s.comments[0]["body"].(string)
	for _, key := range []string{prstatus.KeyStageStalled, prstatus.KeyStageRegressed, prstatus.KeyEvidence} {
		n := 0
		for _, sec := range prstatus.Parse(body) {
			if sec.Key == key {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("section %q appears %d times in %q", key, n, body)
		}
	}
	if created, edited, _ := c.NotificationCounts(); created != 0 || edited != 2 {
		t.Fatalf("tally created=%d edited=%d, want 0 created and 2 edits", created, edited)
	}
}

// Own is this hive's App login, not "any bot": another App's status comment
// is never edited or deleted, and an unknown App login fails closed.
func TestUpsertPRStatusSectionIgnoresOtherBotsComments(t *testing.T) {
	for _, tc := range []struct{ name, appLogin string }{
		{"other bot", "hive-app[bot]"},
		{"unknown own login", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := map[string]any{"login": "other-app[bot]", "type": "Bot"}
			s := &statusCommentServer{comments: []map[string]any{
				{"id": 11, "body": prstatus.Upsert("", prstatus.KeyStageStalled, "stalled"), "user": other},
			}}
			c := testClient(t, s.start(t).URL)
			c.SetAppBotLogin(tc.appLogin)
			created, err := c.UpsertPRStatusSection(context.Background(), "o/r", 5, prstatus.KeyEvidence, "evidence")
			if err != nil || !created {
				t.Fatalf("created=%v err=%v, want a fresh comment", created, err)
			}
			if s.edits != 0 || len(s.deletes) != 0 {
				t.Fatalf("edits=%d deletes=%v, want the other bot's comment untouched", s.edits, s.deletes)
			}
		})
	}
}

// Quiet mode: the duplicate-guard redirect lands in the status comment of
// the PR it points at and a retry edits it rather than stacking comments.
func TestCreatePR_DuplicateGuardQuietModeUsesStatusComment(t *testing.T) {
	g := &guardServer{open: []guardPR{{Number: 7, Ref: "a", Body: "Fixes #42", User: "human"}}}
	srv := g.start(t)
	c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
	c.SetAppBotLogin("hive-app[bot]")
	c.SetNotificationsQuiet(func() bool { return true })
	res, err := c.CreatePR(context.Background(), "o/r", "new-branch", "main", "title", "Closes #42")
	if err != nil || res.Number != 7 || res.DuplicateReason != DuplicateReasonClosesIssue {
		t.Fatalf("CreatePR = %+v, %v", res, err)
	}
	if g.comments != 1 || g.edits != 0 {
		t.Fatalf("first redirect must create the status comment: comments=%d edits=%d", g.comments, g.edits)
	}
	// Retry: the status comment exists now, so the guard edits it.
	g.existingComments = []string{prstatus.Upsert("", prstatus.KeyDuplicateGuard, "stale")}
	if _, err := c.CreatePR(context.Background(), "o/r", "new-branch", "main", "title", "Closes #42"); err != nil {
		t.Fatal(err)
	}
	if g.comments != 1 || g.edits != 1 {
		t.Fatalf("retry must edit the status comment, not post: comments=%d edits=%d", g.comments, g.edits)
	}
}

// Quiet mode: a review whose body is only the clean acknowledgement is
// recorded as a verdict and never submitted to GitHub; a body with findings
// is still a review.
func TestReviewRequestWatcher_QuietModeSuppressesNoFindingsReview(t *testing.T) {
	reviewed := 0
	srv := newReviewMockServer(t, &reviewed, nil)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.SetNotificationsQuiet(func() bool { return true })
	dir := withReviewDir(t)
	oldLinks := ReviewLinksPath
	ReviewLinksPath = filepath.Join(t.TempDir(), "review-links.json")
	t.Cleanup(func() { ReviewLinksPath = oldLinks })
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
		{Repo: "o/r", Number: 5, HeadSHA: "abc", Perspective: review.PerspectiveCorrectness, Agent: "reviewer"},
	}})

	submit := func(body string) ReviewResponse {
		t.Helper()
		path, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 5, Event: "comment", Agent: "reviewer", Body: body, Report: validVerdictJSON(t, "o/r", 5, "correctness", "approve")})
		if err != nil {
			t.Fatal(err)
		}
		c.ProcessReviewRequestsOnce(context.Background())
		raw, err := os.ReadFile(strings.TrimSuffix(path, ".json") + ".result.json")
		if err != nil {
			t.Fatal(err)
		}
		var resp ReviewResponse
		_ = json.Unmarshal(raw, &resp)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("request must be consumed: %v", err)
		}
		return resp
	}

	resp := submit("**Reviewed** — no findings from this perspective.")
	if !resp.OK || resp.State != ReviewEventRecordVerdict || resp.Note != reviewNoFindingsSuppressedNote {
		t.Fatalf("no-findings ack must be recorded, not posted: %+v", resp)
	}
	if reviewed != 0 {
		t.Fatalf("no-findings ack submitted %d reviews", reviewed)
	}
	if resp := submit("`main.go:12` the error is dropped on the floor"); !resp.OK || reviewed != 1 {
		t.Fatalf("a body with findings must still post: %+v reviewed=%d", resp, reviewed)
	}
}

func TestMaxReviewsPerHeadQuietMode(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:0")
	c.SetReviewCadenceLimits(func() bool { return false }, func() int { return 0 })
	normal := c.maxReviewsPerHead()
	c.SetNotificationsQuiet(func() bool { return true })
	if got := c.maxReviewsPerHead(); got != 1 {
		t.Fatalf("quiet mode must allow one review per head, got %d (normal %d)", got, normal)
	}
	c.SetReviewCadenceLimits(func() bool { return false }, func() int { return 3 })
	if got := c.maxReviewsPerHead(); got != 3 {
		t.Fatalf("an explicit max_reviews_per_head must win: %d", got)
	}
}
