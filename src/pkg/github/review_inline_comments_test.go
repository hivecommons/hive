package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// A two-hunk patch: lines 10-12 and 40-41 of the head are in the diff, as
// are base lines 10-11 and 40.
const inlineTestPatch = "@@ -10,2 +10,3 @@ func a() {\n context\n-old\n+new\n+added\n@@ -40 +41,1 @@\n-gone\n+here\n\\ No newline at end of file"

func TestParseHunkHeader(t *testing.T) {
	for _, tc := range []struct {
		in       string
		old, new int
		ok       bool
	}{
		{"@@ -10,2 +10,3 @@ func a() {", 10, 10, true},
		{"@@ -1 +1 @@", 1, 1, true},
		{"@@ -0,0 +1,4 @@", 0, 1, true},
		{"@@ garbage @@", 0, 0, false},
		{"@@ -x,1 +1 @@", 0, 0, false},
		{"@@", 0, 0, false},
	} {
		o, n, ok := parseHunkHeader(tc.in)
		if o != tc.old || n != tc.new || ok != tc.ok {
			t.Errorf("parseHunkHeader(%q) = %d,%d,%v want %d,%d,%v", tc.in, o, n, ok, tc.old, tc.new, tc.ok)
		}
	}
}

func TestParsePatchAnchors(t *testing.T) {
	a := parsePatchAnchors(inlineTestPatch)
	right, left := a[reviewSideRight], a[reviewSideLeft]
	for _, l := range []int{10, 11, 12, 41} {
		if _, ok := right[l]; !ok {
			t.Errorf("RIGHT line %d should be anchorable; got %v", l, right)
		}
	}
	for _, l := range []int{10, 11, 40} {
		if _, ok := left[l]; !ok {
			t.Errorf("LEFT line %d should be anchorable; got %v", l, left)
		}
	}
	if _, ok := right[13]; ok {
		t.Errorf("RIGHT line 13 is outside every hunk")
	}
	if right[10] != 0 || right[41] != 1 {
		t.Errorf("hunk indexes wrong: %v", right)
	}
	if got := parsePatchAnchors(""); len(got[reviewSideRight]) != 0 || len(got[reviewSideLeft]) != 0 {
		t.Errorf("empty patch must anchor nothing: %v", got)
	}
	// Lines before any valid hunk header carry no anchor.
	if got := parsePatchAnchors("+orphan\n@@ bad @@\n+also"); len(got[reviewSideRight]) != 0 {
		t.Errorf("lines outside a valid hunk must not anchor: %v", got)
	}
}

func TestDiffAnchorsAnchorable(t *testing.T) {
	a := diffAnchors{"f.go": parsePatchAnchors(inlineTestPatch)}
	for _, tc := range []struct {
		name string
		rc   ReviewInlineComment
		want bool
	}{
		{"right added line", ReviewInlineComment{Path: "f.go", Line: 11}, true},
		{"explicit right", ReviewInlineComment{Path: "f.go", Line: 12, Side: "right"}, true},
		{"left deleted line", ReviewInlineComment{Path: "f.go", Line: 11, Side: "LEFT"}, true},
		{"line outside diff", ReviewInlineComment{Path: "f.go", Line: 99}, false},
		{"file not in PR", ReviewInlineComment{Path: "other.go", Line: 11}, false},
		{"range in one hunk", ReviewInlineComment{Path: "f.go", StartLine: 10, Line: 12}, true},
		{"range across hunks", ReviewInlineComment{Path: "f.go", StartLine: 12, Line: 41}, false},
		{"range start outside", ReviewInlineComment{Path: "f.go", StartLine: 5, Line: 12}, false},
	} {
		if got := a.anchorable(tc.rc); got != tc.want {
			t.Errorf("%s: anchorable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestInlineCommentsShapeError(t *testing.T) {
	ok := ReviewInlineComment{Path: "a.go", Line: 3, Body: "x"}
	if msg := inlineCommentsShapeError([]ReviewInlineComment{ok}); msg != "" {
		t.Fatalf("valid comment refused: %s", msg)
	}
	tooMany := make([]ReviewInlineComment, maxReviewInlineComments+1)
	for i := range tooMany {
		tooMany[i] = ok
	}
	cases := map[string][]ReviewInlineComment{
		"at most":             tooMany,
		"requires path":       {{Line: 1, Body: "x"}},
		"repository-relative": {{Path: "/etc/x", Line: 1, Body: "x"}},
		"positive line":       {{Path: "a", Body: "x"}},
		"start_line":          {{Path: "a", Line: 3, StartLine: 3, Body: "x"}},
		"side must":           {{Path: "a", Line: 3, Side: "middle", Body: "x"}},
		"requires body":       {{Path: "a", Line: 3, Body: "  "}},
		"exceeds":             {{Path: "a", Line: 3, Body: strings.Repeat("x", maxReviewInlineCommentBytes+1)}},
	}
	for want, comments := range cases {
		if msg := inlineCommentsShapeError(comments); !strings.Contains(msg, want) {
			t.Errorf("%s: got %q", want, msg)
		}
	}
	if msg := inlineCommentsShapeError([]ReviewInlineComment{{Path: "a", Line: 3, Body: "Look at this:\n\n```go\nauth := \"[REDACTED]\"\n```"}}); msg == "" {
		t.Errorf("a comment quoting a redacted span must be refused like the body")
	}
}

func TestFoldInlineComments(t *testing.T) {
	if got := foldInlineComments("summary", nil); got != "summary" {
		t.Fatalf("no comments must leave the body alone: %q", got)
	}
	got := foldInlineComments("summary\n", []ReviewInlineComment{
		{Path: "a.go", Line: 3, Body: "first\nsecond"},
		{Path: "b.go", StartLine: 7, Line: 9, Side: "LEFT", Body: "base"},
	})
	for _, want := range []string{"summary\n\n" + reviewInlineFoldHeading, "- `a.go:3`: first\n  second", "- `b.go:7-9 (base)`: base"} {
		if !strings.Contains(got, want) {
			t.Errorf("fold missing %q:\n%s", want, got)
		}
	}
	if got := foldInlineComments("", []ReviewInlineComment{{Path: "a", Line: 1, Body: "x"}}); !strings.HasPrefix(got, reviewInlineFoldHeading) {
		t.Errorf("empty body fold should start with the heading: %q", got)
	}
}

func TestDraftReviewComments(t *testing.T) {
	if draftReviewComments(nil, nil) != nil {
		t.Fatal("no comments must produce a nil payload")
	}
	d := draftReviewComments([]ReviewInlineComment{
		{Path: " a.go ", Line: 3, Body: "x"},
		{Path: "b.go", StartLine: 1, Line: 2, Side: "left", Body: "y"},
	}, strings.ToUpper)
	if d[0].GetBody() != "X" {
		t.Errorf("body transform not applied: %q", d[0].GetBody())
	}
	if d[0].GetPath() != "a.go" || d[0].GetLine() != 3 || d[0].GetSide() != "RIGHT" || d[0].StartLine != nil {
		t.Errorf("single-line comment wrong: %v", d[0])
	}
	if d[1].GetStartLine() != 1 || d[1].GetStartSide() != "LEFT" || d[1].GetSide() != "LEFT" {
		t.Errorf("range comment wrong: %v", d[1])
	}
}

func TestInlineCommentsText(t *testing.T) {
	got := inlineCommentsText([]ReviewInlineComment{{Body: "a"}, {Body: "b"}})
	if got != "a\nb\n" {
		t.Errorf("inlineCommentsText = %q", got)
	}
}

func TestIsInlineAnchorRefusal(t *testing.T) {
	mk := func(status int, msg string) error {
		return &gh.ErrorResponse{Response: &http.Response{StatusCode: status}, Message: msg}
	}
	if !isInlineAnchorRefusal(mk(422, "Line could not be resolved")) {
		t.Error("a 422 anchor refusal must be recognised")
	}
	if isInlineAnchorRefusal(mk(500, "boom")) || isInlineAnchorRefusal(nil) || isInlineAnchorRefusal(errors.New("x")) {
		t.Error("non-422 errors are not anchor refusals")
	}
	if isInlineAnchorRefusal(mk(422, "Can not approve your own pull request")) {
		t.Error("the own-PR refusal has its own handling")
	}
}

func TestReviewFindingsSectionCarriesInlineComments(t *testing.T) {
	got := reviewFindingsSection(ReviewRequest{Agent: "reviewer", Body: "summary", Comments: []ReviewInlineComment{{Path: "a.go", Line: 3, Body: "finding"}}})
	if !strings.Contains(got, "summary") || !strings.Contains(got, "`a.go:3`: finding") {
		t.Errorf("per-head fold dropped inline comments:\n%s", got)
	}
}

func TestPartitionInlineCommentsNilClient(t *testing.T) {
	var c *Client
	comments := []ReviewInlineComment{{Path: "a", Line: 1, Body: "x"}}
	anchored, folded := c.partitionInlineComments(context.Background(), "o", "r", 1, comments)
	if len(anchored) != 1 || len(folded) != 0 {
		t.Errorf("unknown diff must keep every comment: %v %v", anchored, folded)
	}
	if a, f := c.partitionInlineComments(context.Background(), "o", "r", 1, nil); a != nil || f != nil {
		t.Errorf("no comments: %v %v", a, f)
	}
	c.logInlineFold(ReviewRequest{}, 1, "nil client is a no-op")
}

// inlineReviewServer serves the PR file list (unless filesStatus is set) and
// records every create-review payload. The first rejectFirst submissions
// carrying comments get a 422.
type inlineReviewServer struct {
	filesStatus int
	rejectFirst int
	posts       []gh.PullRequestReviewRequest
	fileGets    int
}

func (s *inlineReviewServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/9/files":
			s.fileGets++
			if s.filesStatus != 0 {
				w.WriteHeader(s.filesStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"filename": "f.go", "status": "modified", "patch": inlineTestPatch},
				{"filename": "img.png", "status": "added"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls/9/reviews":
			var body gh.PullRequestReviewRequest
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			s.posts = append(s.posts, body)
			if len(body.Comments) > 0 && s.rejectFirst > 0 {
				s.rejectFirst--
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, `{"message":"Unprocessable Entity","errors":["Line could not be resolved"]}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":7,"state":"COMMENTED","commit_id":"abc"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func inlineReviewRequest() ReviewRequest {
	return ReviewRequest{
		Repo: "o/r", Number: 9, Event: "comment", Agent: "reviewer", Body: "Overall summary",
		Comments: []ReviewInlineComment{
			{Path: "f.go", Line: 11, Body: "anchored finding"},
			{Path: "f.go", Line: 99, Body: "off-diff finding"},
			{Path: "img.png", Line: 1, Body: "binary file finding"},
		},
	}
}

// One review, carrying the anchorable comment as a thread and the rest in
// the summary.
func TestReviewRequestWatcher_InlineCommentsOneReview(t *testing.T) {
	s := &inlineReviewServer{}
	srv := s.start(t)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDir(t)
	reqPath, err := WriteReviewRequest(dir, inlineReviewRequest())
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if len(s.posts) != 1 {
		t.Fatalf("want exactly one create-review call, got %d", len(s.posts))
	}
	p := s.posts[0]
	if len(p.Comments) != 1 || p.Comments[0].GetPath() != "f.go" || p.Comments[0].GetLine() != 11 || p.Comments[0].GetSide() != "RIGHT" {
		t.Fatalf("inline comments = %v, want the one anchored on f.go:11", p.Comments)
	}
	body := p.GetBody()
	for _, want := range []string{"Overall summary", reviewInlineFoldHeading, "`f.go:99`: off-diff finding", "`img.png:1`: binary file finding"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "anchored finding") {
		t.Errorf("an anchored finding must not be duplicated into the summary")
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("request should be consumed")
	}
}

// An unreadable diff keeps every comment; GitHub's 422 then folds them all
// into one resubmitted review.
func TestReviewRequestWatcher_InlineCommentsAnchorRefusalFallsBack(t *testing.T) {
	s := &inlineReviewServer{filesStatus: http.StatusInternalServerError, rejectFirst: 1}
	srv := s.start(t)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDir(t)
	reqPath, err := WriteReviewRequest(dir, inlineReviewRequest())
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if len(s.posts) != 2 {
		t.Fatalf("want the refused review and one resubmission, got %d posts", len(s.posts))
	}
	if len(s.posts[0].Comments) != 3 {
		t.Errorf("unknown diff should send every comment first, got %d", len(s.posts[0].Comments))
	}
	if len(s.posts[1].Comments) != 0 {
		t.Errorf("resubmission must carry no inline comments")
	}
	body := s.posts[1].GetBody()
	for _, want := range []string{"Overall summary", "`f.go:11`: anchored finding", "`f.go:99`: off-diff finding"} {
		if !strings.Contains(body, want) {
			t.Errorf("fallback summary missing %q:\n%s", want, body)
		}
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("request should be consumed after the fallback lands")
	}
}

// Comments on an event that posts no PR-level review are refused before
// any API call.
func TestReviewRequestWatcher_InlineCommentsShapeRefused(t *testing.T) {
	for name, req := range map[string]ReviewRequest{
		"record_verdict": {Repo: "o/r", Number: 9, Event: ReviewEventRecordVerdict, Agent: "reviewer", Report: "{}", Comments: []ReviewInlineComment{{Path: "a", Line: 1, Body: "x"}}},
		"thread reply":   {Repo: "o/r", Number: 9, Event: "comment", Agent: "reviewer", Body: "b", ThreadID: "PRRT_x", Comments: []ReviewInlineComment{{Path: "a", Line: 1, Body: "x"}}},
		"bad comment":    {Repo: "o/r", Number: 9, Event: "comment", Agent: "reviewer", Body: "b", Comments: []ReviewInlineComment{{Path: "a", Body: "x"}}},
	} {
		t.Run(name, func(t *testing.T) {
			s := &inlineReviewServer{}
			srv := s.start(t)
			defer srv.Close()
			c := reviewTestClient(t, srv.URL)
			dir := withReviewDir(t)
			reqPath, err := WriteReviewRequest(dir, req)
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessReviewRequestsOnce(context.Background())
			if len(s.posts) != 0 || s.fileGets != 0 {
				t.Errorf("a refused request must not reach GitHub: posts=%d files=%d", len(s.posts), s.fileGets)
			}
			if _, err := os.Stat(reqPath + ".bad"); err != nil {
				t.Errorf("request should be quarantined .bad")
			}
		})
	}
}

// A request without comments never fetches the diff.
func TestReviewRequestWatcher_NoCommentsNoDiffFetch(t *testing.T) {
	s := &inlineReviewServer{}
	srv := s.start(t)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDir(t)
	if _, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 9, Event: "comment", Agent: "reviewer", Body: "plain"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if s.fileGets != 0 {
		t.Errorf("plain review fetched the diff %d times", s.fileGets)
	}
	if len(s.posts) != 1 || len(s.posts[0].Comments) != 0 {
		t.Errorf("plain review payload wrong: %+v", s.posts)
	}
}
