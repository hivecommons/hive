package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestAskDefersTo(t *testing.T) {
	for _, tc := range []struct {
		text string
		n    int
		want bool
	}{
		// Danathar/tududi#512 and #528, verbatim.
		{"**What I need from you:** Answer on #511; the same choice applies here.", 511, true},
		{"This is the same choice you were already asked about on the 2.0 preview (#511), and nothing has been merged for it.", 511, true},
		{"Parking this with #511 instead of syncing, because that question has no answer yet.", 511, true},
		{"An answer here or on #511 settles all three.", 511, true},
		{"Whatever the owner says on #511 settles this one too.", 511, true},
		{"Pick on #172.", 172, true},
		{"This defers to #40.", 40, true},
		// Mentions that are not deferrals.
		{"PR #172, the fix for stalled downloads, fails the same way.", 172, false},
		{"Answer on #511; the same choice applies here.", 51, false},
		{"Unlike #511, this needs its own answer. Pick A or B here.", 511, false},
	} {
		if got := askDefersTo(tc.text, tc.n); got != tc.want {
			t.Errorf("askDefersTo(%q, %d) = %v, want %v", tc.text, tc.n, got, tc.want)
		}
	}
}

func TestAskFoldCandidates(t *testing.T) {
	text := "See #511, #511 again, #600, #12 and #9, #8, #7; owner/repo#5 and a heading anchor x#3 are not refs."
	if got := fmt.Sprint(askFoldCandidates(text, 512)); got != "[511 12 9 8]" {
		t.Fatalf("candidates = %s, want same-repo refs below #512, deduped, at most 4", got)
	}
	if got := fmt.Sprint(askFoldCandidates("Answer on #511.", 0)); got != "[511]" {
		t.Fatalf("candidates for a new issue = %s, want [511]", got)
	}
}

type foldFake struct {
	mu     sync.Mutex
	issues map[int]string // number -> JSON
	notes  map[int]string // number -> comments JSON
	writes []string
	reads  []string
}

func (f *foldFake) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	var n int
	switch {
	case r.Method == "GET" && strings.HasSuffix(p, "/comments"):
		_, _ = fmt.Sscanf(p[strings.Index(p, "/issues/")+len("/issues/"):], "%d", &n)
		f.reads = append(f.reads, fmt.Sprintf("comments:%d", n))
		if c, ok := f.notes[n]; ok {
			_, _ = io.WriteString(w, c)
			return
		}
		_, _ = io.WriteString(w, "[]")
	case r.Method == "GET" && strings.Contains(p, "/issues/"):
		_, _ = fmt.Sscanf(p[strings.LastIndex(p, "/")+1:], "%d", &n)
		f.reads = append(f.reads, fmt.Sprintf("issue:%d", n))
		if j, ok := f.issues[n]; ok {
			_, _ = io.WriteString(w, j)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == "POST" && strings.HasSuffix(p, "/labels"):
		_, _ = io.WriteString(w, `[{"name":"x"}]`)
	case r.Method == "POST" && strings.HasSuffix(p, "/dependencies/blocked_by"):
		body, _ := io.ReadAll(r.Body)
		f.writes = append(f.writes, "blocked_by:"+strings.TrimSpace(string(body)))
		_, _ = io.WriteString(w, `{}`)
	case r.Method == "POST" && strings.HasSuffix(p, "/comments"):
		var body struct{ Body string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.writes = append(f.writes, "comment:"+body.Body)
		_, _ = io.WriteString(w, `{"id":1}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func parkedIssueJSON(n, id int, extra string) string {
	return fmt.Sprintf(`{"id":%d,"number":%d,"state":"open","labels":[{"name":"owner-call"}]%s}`, id, n, extra)
}

func runFoldComment(t *testing.T, f *foldFake, number int, body string, blockedBy []int) (IssueResponse, []string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = "hive-app[bot]"
	c.SetAgentIssueDecisionPolicy("escalate")
	c.SetIssueFilter(needsDecisionFilter{label: "owner-call"})
	dir := withIssueDir(t)
	reqPath, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: number, Agent: "scanner", NeedsDecision: true, Body: body, BlockedBy: blockedBy})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	f.mu.Lock()
	defer f.mu.Unlock()
	return readIssueResultFile(t, reqPath), append([]string(nil), f.writes...)
}

// Danathar/tududi#528 parks "the same choice you were already asked about
// ... (#511)" without --blocked-by: the relay links it to #511 itself and
// says where to answer.
func TestCommentParkFoldsIntoDeferredAsk(t *testing.T) {
	f := &foldFake{issues: map[int]string{
		528: `{"id":5280,"number":528,"state":"open"}`,
		511: parkedIssueJSON(511, 5110, ""),
	}}
	resp, writes := runFoldComment(t, f, 528, "This is the same choice you were already asked about on the 2.0 preview (#511).", nil)
	if !resp.OK || fmt.Sprint(resp.BlockedByLinked) != "[511]" {
		t.Fatalf("result = %+v, want ok with #511 linked", resp)
	}
	if len(writes) != 2 || !strings.Contains(writes[0], askFoldNote(511)) || writes[1] != `blocked_by:{"issue_id":5110}` {
		t.Fatalf("writes = %q, want the comment with the fold note, then the link to #511", writes)
	}
}

// Two parks that cite the same failing Actions run ask one question.
func TestCommentParkFoldsOnSameRun(t *testing.T) {
	run := "https://github.com/o/r/actions/runs/41967867530"
	f := &foldFake{
		issues: map[int]string{
			177: `{"id":1770,"number":177,"state":"open"}`,
			173: parkedIssueJSON(173, 1730, `,"body":"E2E fails."`),
		},
		notes: map[int]string{173: `[{"id":2,"user":{"login":"hive-app[bot]"},"body":"Failing run: ` + run + `"}]`},
	}
	resp, writes := runFoldComment(t, f, 177, "Coverage reads 14% in "+run+"; see #173 for E2E.", nil)
	if !resp.OK || fmt.Sprint(resp.BlockedByLinked) != "[173]" || len(writes) != 2 {
		t.Fatalf("result = %+v writes = %q, want #173 linked on the shared run", resp, writes)
	}
}

func TestCommentParkDoesNotFold(t *testing.T) {
	run := "https://github.com/o/r/actions/runs/1"
	for _, tc := range []struct {
		name   string
		issues map[int]string
		notes  map[int]string
		body   string
	}{
		{"named issue is not parked", map[int]string{511: `{"id":5110,"number":511,"state":"open","labels":[]}`}, nil, "Answer on #511."},
		{"named issue is closed", map[int]string{511: `{"id":5110,"number":511,"state":"closed","labels":[{"name":"owner-call"}]}`}, nil, "Answer on #511."},
		{"named item is a pull request", map[int]string{511: parkedIssueJSON(511, 5110, `,"pull_request":{"url":"x"}`)}, nil, "Answer on #511."},
		{"a mention, not a deferral", map[int]string{511: parkedIssueJSON(511, 5110, "")}, nil, "Unlike #511, pick A or B here."},
		{"a different run", map[int]string{511: parkedIssueJSON(511, 5110, `,"body":"`+run+`"`)}, nil, "See #511; this failed in https://github.com/o/r/actions/runs/2."},
		{"a run cited only by a person", map[int]string{511: parkedIssueJSON(511, 5110, "")},
			map[int]string{511: `[{"id":2,"user":{"login":"someone"},"body":"` + run + `"}]`}, "See #511; this failed in " + run + "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.issues[600] = `{"id":6000,"number":600,"state":"open"}`
			f := &foldFake{issues: tc.issues, notes: tc.notes}
			resp, writes := runFoldComment(t, f, 600, tc.body, nil)
			if !resp.OK || len(resp.BlockedByLinked) != 0 || len(writes) != 1 || strings.Contains(writes[0], "blocked by #") {
				t.Fatalf("result = %+v writes = %q, want a plain park", resp, writes)
			}
		})
	}
}

// A later issue is never a fold target, so two asks cannot wait on each
// other (openaudible #172 said "answer on #174", #174 said "pick on #172");
// an explicit --blocked-by is the agent's own call and is not second-guessed.
func TestCommentParkFoldOrderAndExplicitLink(t *testing.T) {
	f := &foldFake{issues: map[int]string{
		511: `{"id":5110,"number":511,"state":"open"}`,
		528: parkedIssueJSON(528, 5280, ""),
	}}
	if resp, writes := runFoldComment(t, f, 511, "Answer on #528.", nil); !resp.OK || len(writes) != 1 {
		t.Fatalf("result = %+v writes = %q, want no fold into a later issue", resp, writes)
	}
	f = &foldFake{issues: map[int]string{
		528: `{"id":5280,"number":528,"state":"open"}`,
		511: parkedIssueJSON(511, 5110, ""),
		300: parkedIssueJSON(300, 3000, ""),
	}}
	resp, writes := runFoldComment(t, f, 528, "Answer on #511.", []int{300})
	if !resp.OK || fmt.Sprint(resp.BlockedByLinked) != "[300]" || strings.Contains(writes[0], askFoldNote(511)) {
		t.Fatalf("result = %+v writes = %q, want only the explicit #300 link", resp, writes)
	}
	for _, r := range f.reads {
		if r == "issue:511" {
			t.Fatalf("reads = %q, want no fold lookup when --blocked-by is given", f.reads)
		}
	}
}

// A new issue filed with --needs-decision folds the same way.
func TestCreateParkFoldsIntoDeferredAsk(t *testing.T) {
	var mu sync.Mutex
	var created string
	f := &foldFake{issues: map[int]string{511: parkedIssueJSON(511, 5110, "")}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/repos/o/r/issues") {
			var body struct{ Body string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			created = body.Body
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":5120,"number":512,"html_url":"https://github.com/o/r/issues/512"}`)
			return
		}
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/repos/o/r/issues") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
			return
		}
		f.handler(w, r)
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetAgentIssueDecisionPolicy("escalate")
	c.SetIssueFilter(needsDecisionFilter{label: "owner-call"})
	dir := withIssueDir(t)
	reqPath, _ := WriteIssueRequest(dir, IssueRequest{Kind: "issue", Repo: "o/r", Agent: "scanner", NeedsDecision: true,
		Title: "Port the v2.0.0-rc.1 tag", Body: "**What I need from you:** Answer on #511; the same choice applies here."})
	c.ProcessIssueRequestsOnce(context.Background())
	resp := readIssueResultFile(t, reqPath)
	mu.Lock()
	defer mu.Unlock()
	if !resp.OK || fmt.Sprint(resp.BlockedByLinked) != "[511]" || !strings.Contains(created, askFoldNote(511)) {
		t.Fatalf("result = %+v body = %q, want #512 filed with the fold note and linked to #511", resp, created)
	}
}
