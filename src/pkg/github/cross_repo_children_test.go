package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// crossRepoIssueServer records which repo each issue create and sub-issue
// link went to, and the created body (hivecommons/hive#18680).
type crossRepoIssueServer struct {
	mu        sync.Mutex
	creates   []string // request paths of POST .../issues
	bodies    []string
	subIssues []string // request paths of POST .../sub_issues
}

func newCrossRepoIssueServer(t *testing.T) (*crossRepoIssueServer, *httptest.Server) {
	t.Helper()
	s := &crossRepoIssueServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sub_issues"):
			s.mu.Lock()
			s.subIssues = append(s.subIssues, r.URL.Path)
			s.mu.Unlock()
			_, _ = io.WriteString(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			s.mu.Lock()
			s.creates = append(s.creates, r.URL.Path)
			s.bodies = append(s.bodies, payload.Body)
			s.mu.Unlock()
			_, _ = io.WriteString(w, `{"id":990099,"number":99,"html_url":"https://github.example/o/x/issues/99"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

// A plan whose work lives in another managed repo files the child THERE,
// links it to the parent in full cross-repo form, and records the
// cross-repo relation in the split-parent ledger (hivecommons/hive#18680).
func TestIssueRequestWatcher_CrossRepoChildFiledInManagedRepo(t *testing.T) {
	fake, srv := newCrossRepoIssueServer(t)
	c := issueTestClient(t, srv.URL)
	c.SetRepos([]string{"r", "companion"})
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/companion", Title: "[scanner] Windows capture", Agent: "scanner",
		Body:   "Part of #18596\n\nAdd the Windows capture module; keeps the #18596 checklist actionable.",
		Parent: 18596, ParentRepo: "o/r",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.creates) != 1 || fake.creates[0] != "/repos/o/companion/issues" {
		t.Fatalf("child must be created in the managed repo its work lives in: %v", fake.creates)
	}
	body := fake.bodies[0]
	if !strings.Contains(body, "Part of o/r#18596") || strings.Contains(body, "Part of #18596") {
		t.Fatalf("parent link must be the full cross-repo form: %q", body)
	}
	if !strings.Contains(body, "the o/r#18596 checklist") {
		t.Fatalf("bare mentions of the parent must be qualified: %q", body)
	}
	if !strings.Contains(body, ParentMarkerFor("o/r", 18596)) || strings.Contains(body, ParentMarker(18596)) {
		t.Fatalf("marker must carry owner/repo#N: %q", body)
	}
	if repo, n := ParseParentMarkerRef(body); repo != "o/r" || n != 18596 {
		t.Fatalf("ParseParentMarkerRef = %q %d", repo, n)
	}
	if ParseParentMarker(body) != 0 {
		t.Fatal("a cross-repo marker must not read as a same-repo parent")
	}
	if len(fake.subIssues) != 1 || fake.subIssues[0] != "/repos/o/r/issues/18596/sub_issues" {
		t.Fatalf("sub-issue link must target the parent's repo: %v", fake.subIssues)
	}
	resp := readIssueResult(t, reqPath)
	if !resp.OK || !resp.ParentLinked || resp.RepoFallbackFrom != "" {
		t.Fatalf("result = %+v", resp)
	}
	ledger, err := LoadSplitParents(SplitParentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := splitChildrenOf(ledger, "o/r", 18596); len(got) != 1 || got[0] != (ChildRef{Repo: "o/companion", Number: 99}) {
		t.Fatalf("ledger children of o/r#18596 = %+v (%+v)", got, ledger)
	}
	if splitParentOf(ledger, "o/companion", 99) != 0 {
		t.Fatal("a cross-repo parent must not be resolved inside the child's repo")
	}
}

// A child whose requested repo the hive does not manage falls back to the
// parent's repo — never filed in an unmanaged repo — with the plain
// same-repo link.
func TestIssueRequestWatcher_CrossRepoChildUnmanagedRepoFallsBack(t *testing.T) {
	fake, srv := newCrossRepoIssueServer(t)
	c := issueTestClient(t, srv.URL)
	c.SetRepos([]string{"r"})
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "elsewhere/unmanaged", Title: "[scanner] Linux capture", Agent: "scanner",
		Body: "Part of #18596\n\nLinux capture.", Parent: 18596, ParentRepo: "o/r",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.creates) != 1 || fake.creates[0] != "/repos/o/r/issues" {
		t.Fatalf("child must fall back to the parent's repo: %v", fake.creates)
	}
	body := fake.bodies[0]
	if !strings.Contains(body, "Part of #18596") || !strings.Contains(body, ParentMarker(18596)) {
		t.Fatalf("same-repo child keeps the bare link: %q", body)
	}
	if len(fake.subIssues) != 1 || fake.subIssues[0] != "/repos/o/r/issues/18596/sub_issues" {
		t.Fatalf("sub-issue link = %v", fake.subIssues)
	}
	resp := readIssueResult(t, reqPath)
	if !resp.OK || resp.RepoFallbackFrom != "elsewhere/unmanaged" {
		t.Fatalf("result must report the fallback: %+v", resp)
	}
}

// A parent in an unmanaged repo is refused outright: the relay never links
// a child under an issue the hive does not manage.
func TestIssueRequestWatcher_CrossRepoChildUnmanagedParentRefused(t *testing.T) {
	fake, srv := newCrossRepoIssueServer(t)
	c := issueTestClient(t, srv.URL)
	c.SetRepos([]string{"r"})
	dir := withIssueDir(t)

	if _, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[scanner] child", Agent: "scanner", Body: "work",
		Parent: 5, ParentRepo: "elsewhere/unmanaged",
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.creates) != 0 || len(fake.subIssues) != 0 {
		t.Fatalf("nothing may be written for an unmanaged parent: creates=%v links=%v", fake.creates, fake.subIssues)
	}
}

func TestWithCrossRepoParentReference(t *testing.T) {
	if got := withCrossRepoParentReference("Part of #7", "", 7); got != "Part of #7" {
		t.Fatalf("same-repo body must be unchanged: %q", got)
	}
	got := withCrossRepoParentReference("Work.\n\nSee #70 and #7, also o/r#7.", "o/r", 7)
	if !strings.HasPrefix(got, "Part of o/r#7\n\n") || !strings.Contains(got, "See #70 and o/r#7, also o/r#7.") {
		t.Fatalf("got %q", got)
	}
	if again := withCrossRepoParentReference(got, "o/r", 7); again != got {
		t.Fatalf("must be idempotent: %q", again)
	}
	// A bare marker survives untouched for withParentMarker to replace.
	body := withCrossRepoParentReference("x\n\n"+ParentMarker(7), "o/r", 7)
	if !strings.Contains(body, ParentMarker(7)) {
		t.Fatalf("marker rewritten here: %q", body)
	}
	if out := withParentMarker(body, "o/r", 7); strings.Contains(out, ParentMarker(7)) || !strings.Contains(out, ParentMarkerFor("o/r", 7)) {
		t.Fatalf("bare marker must become the full form: %q", out)
	}
}

// The parent's tracker progress counts children in any managed repo: a
// closed cross-repo child is Completed, an open one is not, and one in a
// repo not yet enumerated counts as open (hivecommons/hive#18680).
func TestChildrenOf_CountsCrossRepoChildren(t *testing.T) {
	ledger := map[string]SplitParent{
		splitParentKey("o/companion", 3): {Parent: 1, ParentRepo: "o/r"},
		splitParentKey("o/companion", 4): {Parent: 1, ParentRepo: "o/r"},
		splitParentKey("o/r", 5):         {Parent: 1},
		splitParentKey("o/other", 6):     {Parent: 1, ParentRepo: "o/r"},
	}
	open := map[int]*gh.Issue{1: {Number: gh.Ptr(1)}, 5: {Number: gh.Ptr(5)}}
	var snaps openIssueSnapshots
	snaps.store("o/companion", map[int]struct{}{4: {}})

	got := childrenOf("o/r", 1, nil, ledger, open, snaps.isOpen)
	// #3 closed (absent from companion's snapshot), #4 open, #5 open in the
	// parent's repo, #6 in an unenumerated repo counts as open.
	if got == nil || *got != (IssueChildren{Total: 4, Completed: 1}) {
		t.Fatalf("children = %+v", got)
	}
	snaps.store("O/Other", map[int]struct{}{})
	if got := childrenOf("o/r", 1, nil, ledger, open, snaps.isOpen); got == nil || got.Completed != 2 {
		t.Fatalf("children after o/other enumerated = %+v", got)
	}
	// The child's repo never mistakes the cross-repo parent number for one
	// of its own issues.
	if childrenOf("o/companion", 1, nil, ledger, map[int]*gh.Issue{}, snaps.isOpen) != nil {
		t.Fatal("o/companion#1 has no children")
	}
	// A cross-repo marker in an open issue of the parent's repo that names
	// another repo is not a child of this parent.
	marked := map[int]*gh.Issue{
		1: {Number: gh.Ptr(1)},
		8: {Number: gh.Ptr(8), Body: gh.Ptr(ParentMarkerFor("o/elsewhere", 1))},
		9: {Number: gh.Ptr(9), Body: gh.Ptr(ParentMarkerFor("O/R", 1))},
	}
	if got := childrenOf("o/r", 1, nil, nil, marked, nil); got == nil || got.Total != 1 {
		t.Fatalf("marker children = %+v", got)
	}
}

// Closing a decomposed parent re-reads cross-repo sub-issues from their own
// repo and names them, and the PR that closed them, in full form.
func TestDecomposedChildren_CrossRepoSubIssues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/sub_issues" {
			_, _ = io.WriteString(w, `[{"number":3,"state":"closed","repository_url":"https://api.github.com/repos/o/r"},`+
				`{"number":3,"state":"closed","repository_url":"https://api.github.com/repos/o/companion"}]`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r", "companion"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	children, open, ok, err := c.decomposedChildren(context.Background(), "o", "r", 7)
	if err != nil || !ok || open != 0 {
		t.Fatalf("children=%v open=%d ok=%v err=%v", children, open, ok, err)
	}
	want := []ChildRef{{Number: 3}, {Repo: "o/companion", Number: 3}}
	if len(children) != 2 || children[0] != want[0] || children[1] != want[1] {
		t.Fatalf("children = %+v, want %+v", children, want)
	}
	note := decomposedParentCloseComment(children, map[ChildRef]int{want[1]: 12})
	if !strings.Contains(note, "All children delivered: #3, o/companion#3 → PR o/companion#12.") {
		t.Fatalf("note = %q", note)
	}
}
