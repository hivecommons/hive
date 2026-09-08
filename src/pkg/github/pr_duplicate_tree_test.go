package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// dupTreeServer fakes the four endpoints the content-identity guard touches.
//
//	branches: branch name        -> head commit SHA
//	trees:    commit SHA         -> tree SHA
//	openPRs:  base branch        -> open PRs on that base
//
// It counts POST /pulls (created) and GET /git/commits/* (commitCalls) so a test
// can assert both the OUTCOME (no duplicate PR) and the COST (the cache works).
type dupTreeServer struct {
	branches map[string]string
	trees    map[string]string
	openPRs  map[string][]dupTreePR

	mu          sync.Mutex
	created     int
	commitCalls int
	labels      int
	listedBases []string
	// failRef, failList and failCommit make the corresponding endpoint 500, so
	// the fail-open behaviour can be exercised one dependency at a time.
	failRef, failList, failCommit bool
}

type dupTreePR struct {
	Number  int
	HeadRef string
	HeadSHA string
}

func (d *dupTreeServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == "GET" && strings.HasSuffix(path, "/repos/o/r"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"r","default_branch":"main"}`)

		case r.Method == "GET" && strings.Contains(path, "/git/ref/heads/"):
			if d.failRef {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			branch := path[strings.Index(path, "/git/ref/heads/")+len("/git/ref/heads/"):]
			sha, ok := d.branches[branch]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"ref":"refs/heads/%s","object":{"sha":%q}}`, branch, sha)

		case r.Method == "GET" && strings.Contains(path, "/git/commits/"):
			d.mu.Lock()
			d.commitCalls++
			d.mu.Unlock()
			if d.failCommit {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			sha := path[strings.Index(path, "/git/commits/")+len("/git/commits/"):]
			tree, ok := d.trees[sha]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"sha":%q,"tree":{"sha":%q}}`, sha, tree)

		case r.Method == "GET" && strings.Contains(path, "/compare/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"files":[]}`)

		case r.Method == "GET" && strings.HasSuffix(path, "/pulls"):
			if d.failList {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			// The head-branch dedupe query is a different question from the
			// guard's base query; only the latter carries no head filter.
			if head := r.URL.Query().Get("head"); head != "" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			base := r.URL.Query().Get("base")
			d.mu.Lock()
			d.listedBases = append(d.listedBases, base)
			d.mu.Unlock()
			out := make([]map[string]any, 0, len(d.openPRs[base]))
			for _, pr := range d.openPRs[base] {
				out = append(out, map[string]any{
					"number":   pr.Number,
					"html_url": fmt.Sprintf("https://github.com/o/r/pull/%d", pr.Number),
					"head":     map[string]any{"ref": pr.HeadRef, "sha": pr.HeadSHA},
					"user":     map[string]any{"login": "kubestellar-hive[bot]"},
				})
			}
			_ = json.NewEncoder(w).Encode(out)

		// #5117's self-authorisation gate reads the cited issue. Counted (and
		// then 404'd, as an unconfigured issue) so a test can assert the
		// duplicate path never asks.
		case r.Method == "GET" && strings.Contains(path, "/issues/") && strings.HasSuffix(path, "/comments"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)

		case r.Method == "GET" && strings.Contains(path, "/issues/"):
			// Hive-filed and untouched by any human — the shape that makes
			// #5117's gate return Held. If the duplicate path ever consults it,
			// the test's SelfAuthorized assertion fails rather than passing by
			// accident.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":581,"user":{"login":"kubestellar-hive[bot]","type":"Bot"},"labels":[],"assignees":[],"comments":0}`)

		case r.Method == "POST" && strings.HasSuffix(path, "/labels"):
			d.mu.Lock()
			d.labels++
			d.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)

		case r.Method == "POST" && strings.HasSuffix(path, "/pulls"):
			d.mu.Lock()
			d.created++
			d.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":99,"html_url":"https://github.com/o/r/pull/99"}`)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (d *dupTreeServer) counts() (created, commitCalls int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.created, d.commitCalls
}

func (d *dupTreeServer) labelCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.labels
}

// readPRResult decodes the .result.json the watcher writes next to a consumed
// request — the artifact the requesting agent actually reads.
func readPRResult(t *testing.T, reqPath string) PRResponse {
	t.Helper()
	raw, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("reading PR result: %v", err)
	}
	var resp PRResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decoding PR result: %v", err)
	}
	return resp
}

// TestCreatePR_ReusesOpenPRWithIdenticalTree is the incident from
// kubestellar/hive#5111: a change copied forward onto a NEW branch, whose tree
// is identical to an open PR's. `git diff` between the tips is empty, so the
// second PR proposes nothing. The head-branch dedupe cannot see it — the
// branches differ — so before this guard a duplicate PR was opened.
func TestCreatePR_ReusesOpenPRWithIdenticalTree(t *testing.T) {
	d := &dupTreeServer{
		branches: map[string]string{"autotune-again": "cafe1111"},
		trees: map[string]string{
			"cafe1111": "eb0b85ab", // the new branch
			"beef2222": "eb0b85ab", // the open PR's head — SAME tree
		},
		openPRs: map[string][]dupTreePR{
			"main": {{Number: 212, HeadRef: "autotune", HeadSHA: "beef2222"}},
		},
	}
	srv := d.start(t)
	c := NewClientForTest(srv.URL, "o", nil, prTestLogger())

	res, err := c.CreatePR(context.Background(), "o/r", "autotune-again", "main", "autotune, again", "body")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if created, _ := d.counts(); created != 0 {
		t.Fatalf("opened %d PRs, want 0 — an identical tree is a no-op, not a contribution", created)
	}
	if res.Number != 212 {
		t.Errorf("Number = %d, want the existing PR 212", res.Number)
	}
	if !res.AlreadyExisted {
		t.Error("AlreadyExisted = false, want true — the caller must not treat this as a fresh PR")
	}
	if !res.DuplicateTree {
		t.Error("DuplicateTree = false, want true — the reuse was decided by content, not by head branch")
	}
	if res.URL != "https://github.com/o/r/pull/212" {
		t.Errorf("URL = %q, want the existing PR's URL", res.URL)
	}
}

// TestCreatePR_OpensWhenTreesDiffer is the control. Without it the test above
// would also pass if the guard simply refused everything.
func TestCreatePR_OpensWhenTreesDiffer(t *testing.T) {
	d := &dupTreeServer{
		branches: map[string]string{"real-work": "cafe1111"},
		trees: map[string]string{
			"cafe1111": "aaaaaaaa",
			"beef2222": "bbbbbbbb",
		},
		openPRs: map[string][]dupTreePR{
			"main": {{Number: 212, HeadRef: "other", HeadSHA: "beef2222"}},
		},
	}
	srv := d.start(t)
	c := NewClientForTest(srv.URL, "o", nil, prTestLogger())

	res, err := c.CreatePR(context.Background(), "o/r", "real-work", "main", "real work", "body")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if created, _ := d.counts(); created != 1 {
		t.Fatalf("opened %d PRs, want 1 — a genuinely different tree must still be filed", created)
	}
	if res.DuplicateTree || res.AlreadyExisted {
		t.Errorf("DuplicateTree=%v AlreadyExisted=%v, want both false", res.DuplicateTree, res.AlreadyExisted)
	}
	if res.Number != 99 {
		t.Errorf("Number = %d, want the newly created 99", res.Number)
	}
}

// TestCreatePR_DuplicateTreeOnlyComparesTheSameBase pins the correctness
// boundary. An identical tree on a DIFFERENT base is a different diff, so it is
// not a duplicate. The guard must scope its query by base rather than scanning
// every open PR.
func TestCreatePR_DuplicateTreeOnlyComparesTheSameBase(t *testing.T) {
	d := &dupTreeServer{
		branches: map[string]string{"feature": "cafe1111"},
		trees: map[string]string{
			"cafe1111": "eb0b85ab",
			"beef2222": "eb0b85ab", // same tree, but the PR targets "release"
		},
		openPRs: map[string][]dupTreePR{
			"release": {{Number: 7, HeadRef: "other", HeadSHA: "beef2222"}},
		},
	}
	srv := d.start(t)
	c := NewClientForTest(srv.URL, "o", nil, prTestLogger())

	res, err := c.CreatePR(context.Background(), "o/r", "feature", "main", "feature", "body")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if created, _ := d.counts(); created != 1 {
		t.Fatalf("opened %d PRs, want 1 — the identical tree targets another base", created)
	}
	if res.DuplicateTree {
		t.Error("DuplicateTree = true; an identical tree on a different base is a different change")
	}
	d.mu.Lock()
	bases := append([]string(nil), d.listedBases...)
	d.mu.Unlock()
	if len(bases) != 1 || bases[0] != "main" {
		t.Errorf("guard listed bases %v, want exactly [main] — an unscoped scan would compare unrelated diffs", bases)
	}
}

// TestCreatePR_DuplicateTreeMatchesIdenticalHeadSHA covers the branch-renamed
// case: the very same commit filed under a new branch name. It must be caught
// without paying a commit lookup for the candidate.
func TestCreatePR_DuplicateTreeMatchesIdenticalHeadSHA(t *testing.T) {
	d := &dupTreeServer{
		branches: map[string]string{"renamed": "cafe1111"},
		trees:    map[string]string{"cafe1111": "eb0b85ab"},
		openPRs: map[string][]dupTreePR{
			"main": {{Number: 241, HeadRef: "original", HeadSHA: "cafe1111"}},
		},
	}
	srv := d.start(t)
	c := NewClientForTest(srv.URL, "o", nil, prTestLogger())

	res, err := c.CreatePR(context.Background(), "o/r", "renamed", "main", "renamed", "body")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	created, commitCalls := d.counts()
	if created != 0 || res.Number != 241 || !res.DuplicateTree {
		t.Fatalf("created=%d number=%d duplicate=%v, want 0/241/true", created, res.Number, res.DuplicateTree)
	}
	// One lookup for our own head; the candidate is recognised by SHA alone.
	if commitCalls != 1 {
		t.Errorf("commit lookups = %d, want 1 — an equal head SHA needs no tree fetch", commitCalls)
	}
}

// TestCreatePR_DuplicateTreeFailsOpen is the property that keeps this guard from
// becoming a new way to LOSE work. Every dependency it reads is auxiliary: if
// any of them fails, the PR must still be opened. A guard that blocked
// publication on a failed read would be a worse bug than the duplicate it
// prevents.
func TestCreatePR_DuplicateTreeFailsOpen(t *testing.T) {
	// Each case makes one dependency fail while leaving a real duplicate in
	// place, so a guard that somehow still fired would be visible as created=0.
	cases := map[string]func(d *dupTreeServer){
		"ref lookup fails":    func(d *dupTreeServer) { d.failRef = true },
		"open-PR list fails":  func(d *dupTreeServer) { d.failList = true },
		"commit lookup fails": func(d *dupTreeServer) { d.failCommit = true },
		"head branch missing": func(d *dupTreeServer) { d.branches = map[string]string{} },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			d := &dupTreeServer{
				branches: map[string]string{"dup": "cafe1111"},
				trees:    map[string]string{"cafe1111": "eb0b85ab", "beef2222": "eb0b85ab"},
				openPRs: map[string][]dupTreePR{
					"main": {{Number: 212, HeadRef: "other", HeadSHA: "beef2222"}},
				},
			}
			breakIt(d)
			srv := d.start(t)
			c := NewClientForTest(srv.URL, "o", nil, prTestLogger())

			res, err := c.CreatePR(context.Background(), "o/r", "dup", "main", "dup", "body")
			if err != nil {
				t.Fatalf("CreatePR returned an error instead of opening the PR: %v", err)
			}
			if created, _ := d.counts(); created != 1 {
				t.Fatalf("opened %d PRs, want 1 — a failed auxiliary read must never withhold the PR", created)
			}
			if res.DuplicateTree {
				t.Error("DuplicateTree = true after a failed lookup; the guard must not claim a finding it could not make")
			}
		})
	}
}

// TestCommitTreeSHA_CachesImmutableMapping pins the cache. A commit's tree can
// never change, so a second resolution must cost no request — without this the
// guard would re-fetch every open PR's tree on every PR creation.
func TestCommitTreeSHA_CachesImmutableMapping(t *testing.T) {
	d := &dupTreeServer{trees: map[string]string{"cafe1111": "eb0b85ab"}}
	srv := d.start(t)
	c := NewClientForTest(srv.URL, "o", nil, prTestLogger())

	for i := range 3 {
		tree, err := c.commitTreeSHA(context.Background(), "o", "r", "cafe1111")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if tree != "eb0b85ab" {
			t.Fatalf("call %d: tree = %q, want eb0b85ab", i, tree)
		}
	}
	if _, commitCalls := d.counts(); commitCalls != 1 {
		t.Errorf("commit lookups = %d across 3 resolutions, want 1", commitCalls)
	}
}

// TestFindOpenPRWithIdenticalTree_CapsCandidates pins the bound. The duplicate
// sits past the cap on purpose: the guard is allowed to miss it, but it must
// stop looking rather than fan out over an unbounded number of open PRs.
func TestFindOpenPRWithIdenticalTree_CapsCandidates(t *testing.T) {
	d := &dupTreeServer{
		branches: map[string]string{"dup": "cafe1111"},
		trees:    map[string]string{"cafe1111": "eb0b85ab"},
	}
	var prs []dupTreePR
	for i := range maxDuplicateTreeCandidates + 10 {
		sha := fmt.Sprintf("sha%04d", i)
		d.trees[sha] = fmt.Sprintf("tree%04d", i) // distinct: nothing matches
		prs = append(prs, dupTreePR{Number: i + 1, HeadRef: fmt.Sprintf("b%d", i), HeadSHA: sha})
	}
	d.openPRs = map[string][]dupTreePR{"main": prs}
	srv := d.start(t)
	c := NewClientForTest(srv.URL, "o", nil, prTestLogger())

	found, err := c.findOpenPRWithIdenticalTree(context.Background(), "o", "r", "dup", "main")
	if err != nil {
		t.Fatalf("findOpenPRWithIdenticalTree: %v", err)
	}
	if found != nil {
		t.Fatalf("found PR #%d, want none — no candidate shares the tree", found.GetNumber())
	}
	// One lookup for our head plus at most the cap for candidates.
	if _, commitCalls := d.counts(); commitCalls > maxDuplicateTreeCandidates+1 {
		t.Errorf("commit lookups = %d, want at most %d — the candidate cap is not holding",
			commitCalls, maxDuplicateTreeCandidates+1)
	}
}

// TestCreatePR_DuplicateTreeHandlesSlashedBranchNames guards a footgun rather
// than a hypothetical: nearly every hive branch name contains a slash
// ("fix/5111-..."), and the guard resolves it through a ref lookup whose URL
// has to survive the embedded path separator.
func TestCreatePR_DuplicateTreeHandlesSlashedBranchNames(t *testing.T) {
	d := &dupTreeServer{
		branches: map[string]string{"fix/5111-duplicate-payloads": "cafe1111"},
		trees:    map[string]string{"cafe1111": "eb0b85ab", "beef2222": "eb0b85ab"},
		openPRs: map[string][]dupTreePR{
			"main": {{Number: 212, HeadRef: "fix/5111-earlier", HeadSHA: "beef2222"}},
		},
	}
	srv := d.start(t)
	c := NewClientForTest(srv.URL, "o", nil, prTestLogger())

	res, err := c.CreatePR(context.Background(), "o/r", "fix/5111-duplicate-payloads", "main", "dup", "body")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if created, _ := d.counts(); created != 0 || !res.DuplicateTree {
		t.Fatalf("created=%d duplicate=%v, want 0/true — a slashed branch name must resolve", created, res.DuplicateTree)
	}
}

// TestPRRequestWatcher_DuplicateTreeDoesNotLabelTheOtherPR pins a side effect
// the guard must NOT have. On the duplicate path the watcher is pointing at a
// PR this request did not open — possibly a human's. Applying the hold label
// there would block a merge that has nothing to do with this agent.
func TestPRRequestWatcher_DuplicateTreeDoesNotLabelTheOtherPR(t *testing.T) {
	d := &dupTreeServer{
		branches: map[string]string{"copy-forward": "cafe1111"},
		trees:    map[string]string{"cafe1111": "eb0b85ab", "beef2222": "eb0b85ab"},
		openPRs: map[string][]dupTreePR{
			"main": {{Number: 212, HeadRef: "someone-elses-branch", HeadSHA: "beef2222"}},
		},
	}
	srv := d.start(t)
	c := testClient(t, srv.URL)
	c.prHoldLabel = func(agent string) bool { return true }

	dir := t.TempDir()
	prRequestDirForTest = dir
	t.Cleanup(func() { prRequestDirForTest = "" })

	// Body cites an issue ON PURPOSE: without a citation #5117's gate returns
	// before touching GitHub, and the "no issue lookups" assertion below would
	// hold no matter what the precedence did.
	reqPath, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Head: "copy-forward", Base: "main", Title: "copy forward",
		Body: "Closes #581", Agent: "scanner",
	})
	if err != nil {
		t.Fatalf("WritePRRequest: %v", err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	// Prove the request actually reached the duplicate verdict FIRST. Without
	// this, a request that failed earlier — a mock endpoint the production path
	// grew and this fake never learned — would also apply no labels, and the
	// assertion below would pass for entirely the wrong reason. It did exactly
	// that when #5198's content-metadata gate landed.
	resp := readPRResult(t, reqPath)
	if !resp.OK || !resp.DuplicateTree || resp.Number != 212 {
		t.Fatalf("result = %+v, want the duplicate verdict against PR 212", resp)
	}

	if labeled := d.labelCalls(); labeled != 0 {
		t.Fatalf("applied labels %d times to PR #212, want 0 — that PR is not this request's to gate", labeled)
	}
}

// TestPRRequestWatcher_DuplicateTreeIsNotSelfAuthorizationJudged pins the
// #5111 x #5117 precedence.
//
// The two guards answer different questions. #5117 asks "may this agent's
// change proceed to merge?", a property of the PR THIS REQUEST CREATED. A
// duplicate request created nothing — it is pointing at a PR that already
// existed, possibly a human's — so there is nothing of its own to judge, and a
// hold computed here could only land on somebody else's PR.
//
// Deliberately at a NON-hold-gated level: at a hold-gated one #5117 is already
// short-circuited by the level, so the test would pass without the precedence
// existing at all.
func TestPRRequestWatcher_DuplicateTreeIsNotSelfAuthorizationJudged(t *testing.T) {
	d := &dupTreeServer{
		branches: map[string]string{"copy-forward": "cafe1111"},
		trees:    map[string]string{"cafe1111": "eb0b85ab", "beef2222": "eb0b85ab"},
		openPRs: map[string][]dupTreePR{
			"main": {{Number: 212, HeadRef: "someone-elses-branch", HeadSHA: "beef2222"}},
		},
	}
	srv := d.start(t)
	c := testClient(t, srv.URL)
	c.SetAppBotLogin("kubestellar-hive[bot]")
	c.prHoldLabel = func(agent string) bool { return false }

	dir := t.TempDir()
	prRequestDirForTest = dir
	t.Cleanup(func() { prRequestDirForTest = "" })

	// #581 is served as hive-filed and unacknowledged — exactly the shape that
	// makes #5117 return Held. If the duplicate path consulted it, this test
	// fails.
	reqPath, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Head: "copy-forward", Base: "main", Title: "copy forward",
		Body: "Closes #581", Agent: "scanner",
	})
	if err != nil {
		t.Fatalf("WritePRRequest: %v", err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	resp := readPRResult(t, reqPath)
	if !resp.OK || !resp.DuplicateTree || resp.Number != 212 {
		t.Fatalf("result = %+v, want the duplicate verdict against PR 212", resp)
	}
	if resp.SelfAuthorized {
		t.Error("a duplicate request opened no PR of its own; there is nothing of its to have authorised")
	}
	if labeled := d.labelCalls(); labeled != 0 {
		t.Errorf("applied labels %d times, want 0 — PR 212 is not this request's to gate", labeled)
	}
}

// TestPRRequestWatcher_ReportsDuplicateTreeAndConsumesRequest is the end-to-end
// shape an agent actually sees: the request is consumed (not retried forever),
// and the result file says the content is already proposed as #212 rather than
// implying a new PR was opened.
func TestPRRequestWatcher_ReportsDuplicateTreeAndConsumesRequest(t *testing.T) {
	d := &dupTreeServer{
		branches: map[string]string{"copy-forward": "cafe1111"},
		trees:    map[string]string{"cafe1111": "eb0b85ab", "beef2222": "eb0b85ab"},
		openPRs: map[string][]dupTreePR{
			"main": {{Number: 212, HeadRef: "original", HeadSHA: "beef2222"}},
		},
	}
	srv := d.start(t)
	c := testClient(t, srv.URL)

	dir := t.TempDir()
	prRequestDirForTest = dir
	t.Cleanup(func() { prRequestDirForTest = "" })

	reqPath, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Head: "copy-forward", Base: "main", Title: "copy forward", Body: "copies the change forward", Agent: "scanner",
	})
	if err != nil {
		t.Fatalf("WritePRRequest: %v", err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if created, _ := d.counts(); created != 0 {
		t.Fatalf("opened %d PRs, want 0", created)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Error("request file survived; a duplicate is a settled outcome, not something to retry")
	}
	resultPath := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.Base(resultPath), err)
	}
	var resp PRResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if !resp.OK || resp.Number != 212 || !resp.AlreadyExisted || !resp.DuplicateTree {
		t.Fatalf("result = %+v, want ok/212/already_existed/duplicate_tree", resp)
	}
}
