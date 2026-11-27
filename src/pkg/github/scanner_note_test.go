package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const testScannerFooter = "\n\n---\n🐝 **Hive Agent**: `scanner` | **Instance:** `test-hive` | **SHA:** `unknown`\n<!-- hive:origin hive=test-hive at=2026-11-15T03:14:00Z -->\n\n-- hive: agent=scanner backend=claude"

// scannerNoteServer fakes the GitHub surface the scanner note reads and
// writes: one PR (head, author, mergeable state), its checks, and its
// comments.
type scannerNoteServer struct {
	mu        sync.Mutex
	prNumber  int
	head      string
	author    string
	mergeable string
	red       []string
	checksErr bool
	prErr     int
	listErr   bool
	comments  []map[string]any
	creates   int
	edits     int
}

var (
	scannerNotePullRE   = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/(\d+)$`)
	scannerNoteChecksRE = regexp.MustCompile(`^/repos/[^/]+/[^/]+/commits/[^/]+/check-runs$`)
	scannerNoteStatusRE = regexp.MustCompile(`^/repos/[^/]+/[^/]+/commits/[^/]+/status$`)
)

func (s *scannerNoteServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && scannerNotePullRE.MatchString(r.URL.Path):
			n, _ := strconv.Atoi(scannerNotePullRE.FindStringSubmatch(r.URL.Path)[1])
			if s.prErr != 0 {
				w.WriteHeader(s.prErr)
				return
			}
			if n != s.prNumber {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": n, "state": "open", "mergeable_state": s.mergeable,
				"head": map[string]any{"sha": s.head},
				"user": map[string]any{"login": s.author, "type": "User"},
			})
		case r.Method == http.MethodGet && scannerNoteStatusRE.MatchString(r.URL.Path):
			if s.checksErr {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"statuses": []any{}})
		case r.Method == http.MethodGet && scannerNoteChecksRE.MatchString(r.URL.Path):
			runs := []any{}
			for i, name := range s.red {
				runs = append(runs, map[string]any{"id": i + 1, "name": name, "status": "completed", "conclusion": "failure", "app": map[string]any{"id": 1}})
			}
			runs = append(runs, map[string]any{"id": 99, "name": "lint", "status": "completed", "conclusion": "success", "app": map[string]any{"id": 1}})
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			if s.listErr {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(s.comments)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.creates++
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
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *scannerNoteServer) markedComments() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, cm := range s.comments {
		if body, _ := cm["body"].(string); strings.Contains(body, "<!-- "+ScannerNoteMarkerName) {
			out = append(out, body)
		}
	}
	return out
}

func scannerNoteClient(t *testing.T, s *scannerNoteServer) *Client {
	t.Helper()
	c := testClient(t, s.start(t).URL)
	c.SetAppBotLogin("hive-app[bot]")
	return c
}

func relayScanner(t *testing.T, c *Client, number int, agent, note string) (bool, IssueResponse) {
	t.Helper()
	var resp IssueResponse
	handled, err := c.relayScannerNote(context.Background(), IssueRequest{Kind: "comment", Repo: "o/r", Number: number, Agent: agent, Body: note}, note+testScannerFooter, &resp)
	if err != nil {
		t.Fatal(err)
	}
	return handled, resp
}

// The #18254 shape: a hive-authored PR re-triaged pass after pass. The first
// note posts, a repeat on the same head and failing set writes nothing, and a
// new head (or a new failing set) edits the one note in place.
func TestScannerNoteHiveAuthoredPR(t *testing.T) {
	s := &scannerNoteServer{prNumber: 7, head: "50eef7e", author: "hive-app[bot]", red: []string{"runtime-tests", "coverage"}}
	c := scannerNoteClient(t, s)

	if handled, _ := relayScanner(t, c, 7, "scanner", "runtime-tests fails: KB_AUTH_TYPES is not defined."); !handled {
		t.Fatal("first note not handled")
	}
	if s.creates != 1 || s.edits != 0 {
		t.Fatalf("first note: creates=%d edits=%d, want 1/0", s.creates, s.edits)
	}

	for i := 0; i < 3; i++ {
		handled, resp := relayScanner(t, c, 7, "scanner-2", "Scanner triage again, reworded "+strconv.Itoa(i))
		if !handled || !resp.AlreadyExisted {
			t.Fatalf("repeat %d: handled=%v already_existed=%v", i, handled, resp.AlreadyExisted)
		}
	}
	if s.creates != 1 || s.edits != 0 {
		t.Fatalf("repeats on the same head wrote: creates=%d edits=%d", s.creates, s.edits)
	}

	s.head = "bb69f41"
	if handled, resp := relayScanner(t, c, 7, "scanner", "New head, still red."); !handled || resp.AlreadyExisted {
		t.Fatalf("changed head: handled=%v already_existed=%v", handled, resp.AlreadyExisted)
	}
	s.red = []string{"coverage"}
	relayScanner(t, c, 7, "scanner", "Only coverage left.")
	if s.creates != 1 || s.edits != 2 {
		t.Fatalf("changes: creates=%d edits=%d, want 1/2", s.creates, s.edits)
	}
	marked := s.markedComments()
	if len(marked) != 1 {
		t.Fatalf("marked comments = %d, want 1", len(marked))
	}
	st, ok := parseScannerNoteState(marked[0])
	if !ok || st.Head != "bb69f41" || st.Checks != scannerNoteFingerprint([]string{"coverage"}) || st.Told != "" {
		t.Fatalf("note state = %+v, %v", st, ok)
	}
	if !strings.Contains(marked[0], "Only coverage left.") || strings.Contains(marked[0], "New head, still red.") {
		t.Fatalf("note body not replaced:\n%s", marked[0])
	}
}

// The #20074 shape: the agent copies the scanner-note marker into its text,
// bare or with forged attributes. The note is still the one note per PR: a
// repeat at an unchanged head and failing set is dropped whatever the marker
// says, and a new head edits the existing note instead of posting another.
func TestScannerNoteAgentSuppliedMarker(t *testing.T) {
	s := &scannerNoteServer{prNumber: 11, head: "1cc89a7", author: "hive-app[bot]", red: []string{"test (hub 4/4)"}}
	c := scannerNoteClient(t, s)

	bare := "<!-- " + ScannerNoteMarkerName + " -->\n"
	if handled, _ := relayScanner(t, c, 11, "scanner", bare+"test (hub 4/4) fails."); !handled {
		t.Fatal("bare-marker note not handled")
	}
	if s.creates != 1 || s.edits != 0 {
		t.Fatalf("first note: creates=%d edits=%d, want 1/0", s.creates, s.edits)
	}

	for i, note := range []string{
		bare + "test (hub 4/4) still fails.",
		"<!-- " + ScannerNoteMarkerName + " head=0000000 checks=forged told=0000000 -->\nStill red.",
	} {
		handled, resp := relayScanner(t, c, 11, "scanner", note)
		if !handled || !resp.AlreadyExisted {
			t.Fatalf("repeat %d: handled=%v already_existed=%v", i, handled, resp.AlreadyExisted)
		}
	}
	if s.creates != 1 || s.edits != 0 {
		t.Fatalf("repeats at an unchanged head wrote: creates=%d edits=%d", s.creates, s.edits)
	}

	s.head = "2dd90b8"
	if handled, resp := relayScanner(t, c, 11, "scanner", bare+"New head, still red."); !handled || resp.AlreadyExisted {
		t.Fatalf("new head: handled=%v already_existed=%v", handled, resp.AlreadyExisted)
	}
	if s.creates != 1 || s.edits != 1 {
		t.Fatalf("new head: creates=%d edits=%d, want 1/1", s.creates, s.edits)
	}
	marked := s.markedComments()
	if len(marked) != 1 || strings.Count(marked[0], ScannerNoteMarkerName) != 1 {
		t.Fatalf("want one note with one marker, got %d:\n%s", len(marked), strings.Join(marked, "\n----\n"))
	}
	if st, ok := parseScannerNoteState(marked[0]); !ok || st.Head != "2dd90b8" || st.Checks != scannerNoteFingerprint([]string{"test (hub 4/4)"}) {
		t.Fatalf("note state = %+v, %v", st, ok)
	}
}

// A human-authored PR with red checks: the author must act, so a new head
// they have not been told about gets a new comment, which takes over as the
// one marked note; repeats and failing-set changes on that head stay quiet.
func TestScannerNoteAuthorMustActPostsOncePerHead(t *testing.T) {
	s := &scannerNoteServer{prNumber: 9, head: "aaa111", author: "alice", red: []string{"build-and-test"}}
	c := scannerNoteClient(t, s)

	relayScanner(t, c, 9, "scanner", "Please merge v5 in.")
	relayScanner(t, c, 9, "scanner", "Please merge v5 in (again).")
	s.red = []string{"build-and-test", "coverage"}
	relayScanner(t, c, 9, "scanner", "coverage is red too.")
	if s.creates != 1 || s.edits != 1 {
		t.Fatalf("same head: creates=%d edits=%d, want 1/1", s.creates, s.edits)
	}
	if st, _ := parseScannerNoteState(s.markedComments()[0]); st.Told != "aaa111" {
		t.Fatalf("told = %q, want aaa111", st.Told)
	}

	s.head = "bbb222"
	relayScanner(t, c, 9, "scanner", "Still red on the new head.")
	if s.creates != 2 {
		t.Fatalf("author not told about the new head: creates=%d", s.creates)
	}
	marked := s.markedComments()
	if len(marked) != 1 || !strings.Contains(marked[0], "Still red on the new head.") {
		t.Fatalf("want exactly the new note marked, got %d:\n%s", len(marked), strings.Join(marked, "\n----\n"))
	}
	if body, _ := s.comments[0]["body"].(string); !strings.Contains(body, "coverage is red too.") {
		t.Fatalf("superseded note lost its text:\n%s", body)
	}

	// All green on a later head: nothing for the author to do, so edit.
	s.head, s.red = "ccc333", nil
	relayScanner(t, c, 9, "scanner", "Green now.")
	if s.creates != 2 {
		t.Fatalf("green head posted a new comment: creates=%d", s.creates)
	}
}

// A merge conflict and a failed DCO are part of the failing set, so a rebase
// note is keyed like a red check.
func TestScannerNoteFailingSetIncludesConflictAndDCO(t *testing.T) {
	s := &scannerNoteServer{prNumber: 3, head: "abc", author: "hive-app[bot]", mergeable: "dirty", red: []string{scannerNoteDCOCheck}}
	c := scannerNoteClient(t, s)
	relayScanner(t, c, 3, "scanner", "Needs a rebase.")
	st, _ := parseScannerNoteState(s.markedComments()[0])
	if want := scannerNoteFingerprint([]string{scannerNoteConflictCheck, scannerNoteDCOCheck}); st.Checks != want {
		t.Fatalf("checks = %q, want %q", st.Checks, want)
	}
	s.mergeable = "clean"
	relayScanner(t, c, 3, "scanner", "Conflict resolved, DCO still red.")
	if s.edits != 1 {
		t.Fatalf("resolved conflict should edit: edits=%d", s.edits)
	}
}

// Unreadable checks are never a reason to drop: the note is edited instead.
func TestScannerNoteUnknownChecksNeverDrop(t *testing.T) {
	s := &scannerNoteServer{prNumber: 4, head: "abc", author: "hive-app[bot]", checksErr: true}
	c := scannerNoteClient(t, s)
	relayScanner(t, c, 4, "scanner", "one")
	if handled, resp := relayScanner(t, c, 4, "scanner", "two"); !handled || resp.AlreadyExisted {
		t.Fatalf("handled=%v already_existed=%v", handled, resp.AlreadyExisted)
	}
	if s.creates != 1 || s.edits != 1 {
		t.Fatalf("creates=%d edits=%d, want 1/1", s.creates, s.edits)
	}
}

// Requests the upsert does not own are left to the usual path.
func TestScannerNotePassThrough(t *testing.T) {
	s := &scannerNoteServer{prNumber: 7, head: "abc", author: "hive-app[bot]"}
	c := scannerNoteClient(t, s)
	ctx := context.Background()
	cases := []struct {
		name string
		req  IssueRequest
		body string
	}{
		{"other lane", IssueRequest{Repo: "o/r", Number: 7, Agent: "reviewer"}, "note" + testScannerFooter},
		{"issue, not a PR", IssueRequest{Repo: "o/r", Number: 8, Agent: "scanner"}, "note" + testScannerFooter},
		{"blocked-on-PR park", IssueRequest{Repo: "o/r", Number: 7, Agent: "scanner", BlockedOnPR: 5}, "note" + testScannerFooter},
		{"shared-CI incident reference", IssueRequest{Repo: "o/r", Number: 7, Agent: "scanner"}, "see #10397\n<!-- hive-shared-ci-10397 -->" + testScannerFooter},
		{"verification", IssueRequest{Repo: "o/r", Number: 7, Agent: "scanner"}, "verified\n<!-- hive-verified -->" + testScannerFooter},
		{"dedicated hive marker", IssueRequest{Repo: "o/r", Number: 7, Agent: "scanner"}, "needs a secret\n<!-- hive:blocker kind=secret -->" + testScannerFooter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp IssueResponse
			handled, err := c.relayScannerNote(ctx, tc.req, tc.body, &resp)
			if handled || err != nil {
				t.Fatalf("handled=%v err=%v, want pass-through", handled, err)
			}
		})
	}
	if s.creates != 0 || s.edits != 0 {
		t.Fatalf("pass-through wrote: creates=%d edits=%d", s.creates, s.edits)
	}

	t.Run("toggle off", func(t *testing.T) {
		t.Setenv(scannerNoteEnv, "off")
		var resp IssueResponse
		if handled, _ := c.relayScannerNote(ctx, IssueRequest{Repo: "o/r", Number: 7, Agent: "scanner"}, "note"+testScannerFooter, &resp); handled {
			t.Fatal("disabled upsert still handled the note")
		}
	})
	t.Run("PR lookup error", func(t *testing.T) {
		s.prErr = http.StatusInternalServerError
		defer func() { s.prErr = 0 }()
		var resp IssueResponse
		if handled, err := c.relayScannerNote(ctx, IssueRequest{Repo: "o/r", Number: 7, Agent: "scanner"}, "note"+testScannerFooter, &resp); handled || err != nil {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
	})
	t.Run("comment list error retries", func(t *testing.T) {
		s.listErr = true
		defer func() { s.listErr = false }()
		var resp IssueResponse
		if handled, err := c.relayScannerNote(ctx, IssueRequest{Repo: "o/r", Number: 7, Agent: "scanner"}, "note"+testScannerFooter, &resp); !handled || err == nil {
			t.Fatalf("handled=%v err=%v, want the retry path", handled, err)
		}
	})
}

// End to end through the relay: the repeat request completes with
// already_existed and posts nothing.
func TestScannerNoteThroughIssueRequestWatcher(t *testing.T) {
	s := &scannerNoteServer{prNumber: 41, head: "50eef7e", author: "hive-app[bot]", red: []string{"runtime-tests"}}
	c := scannerNoteClient(t, s)
	c.issueAuthz = func(agent string, uid int, kind string) error { return nil }
	dir := withIssueDir(t)
	for i := 0; i < 2; i++ {
		if _, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 41, Body: "triage pass " + strconv.Itoa(i) + testScannerFooter, Agent: "scanner"}); err != nil {
			t.Fatal(err)
		}
		c.ProcessIssueRequestsOnce(context.Background())
	}
	if s.creates != 1 || s.edits != 0 {
		t.Fatalf("creates=%d edits=%d, want 1/0", s.creates, s.edits)
	}
}

func TestScannerNoteHelpers(t *testing.T) {
	for agent, want := range map[string]bool{"scanner": true, "Scanner-3": true, "scanner-automerge": false, "reviewer": false, "": false} {
		if got := isScannerAgent(agent); got != want {
			t.Errorf("isScannerAgent(%q) = %v, want %v", agent, got, want)
		}
	}
	for v, want := range map[string]bool{"": true, "1": true, "on": true, "0": false, "false": false, "OFF": false, "no": false} {
		t.Setenv(scannerNoteEnv, v)
		if got := scannerNoteEnabled(); got != want {
			t.Errorf("%s=%q: enabled=%v, want %v", scannerNoteEnv, v, got, want)
		}
	}
	if scannerNoteFingerprint([]string{"b", "a", "a", " "}) != scannerNoteFingerprint([]string{"a", "b"}) {
		t.Error("fingerprint depends on order or duplicates")
	}
	if scannerNoteFingerprint(nil) == scannerNoteFingerprint([]string{"a"}) {
		t.Error("empty and non-empty sets collide")
	}
	st := scannerNoteState{Head: "h", Checks: "c", Told: "t"}
	got, ok := parseScannerNoteState(renderScannerNote(st, "body\n"+st.marker()))
	if !ok || got != st {
		t.Fatalf("round trip = %+v, %v", got, ok)
	}
	if n := strings.Count(renderScannerNote(st, "body\n"+st.marker()), ScannerNoteMarkerName); n != 1 {
		t.Fatalf("forged marker kept: %d markers", n)
	}
	if _, ok := parseScannerNoteState("no marker"); ok {
		t.Fatal("parsed a marker from plain text")
	}
	if !scannerNoteCandidate("note\n<!-- hive-scanner-triage -->" + testScannerFooter) {
		t.Fatal("ad-hoc marker should not block the upsert")
	}
	if !scannerNoteCandidate("<!-- " + ScannerNoteMarkerName + " -->\nnote" + testScannerFooter) {
		t.Fatal("a copied scanner-note marker should not block the upsert")
	}
}
