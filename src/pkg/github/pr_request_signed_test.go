package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// signedMock is a GitHub double for the re-author flow: compare, refs, tree,
// raw blobs, the GraphQL mutation, and the PR endpoints the watcher hits
// afterwards. It records every call in order so a test can assert not just
// WHAT was sent but that the branch was only rewritten after the signed
// commit existed, and that the scratch ref was cleaned up.
type signedMock struct {
	t  *testing.T
	mu sync.Mutex

	headSHA       string // sha GET git/ref/heads/<head> answers with
	headSHALater  string // when set, the SECOND GET of the head ref answers this (simulates a push under us)
	headRefReads  int
	mergeBase     string
	files         []map[string]any
	commits       []map[string]any
	treeMode      map[string]string // path → mode; default 100644
	blobs         map[string]string // sha → raw content
	treeTruncated bool

	calls    []string // "METHOD path"
	graphql  map[string]any
	refPatch map[string]any
	created  int
}

func (m *signedMock) record(r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, r.Method+" "+r.URL.Path)
}

func (m *signedMock) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case r.Method == "GET" && strings.HasSuffix(p, "/repos/o/r"):
			_, _ = io.WriteString(w, `{"name":"r","default_branch":"main"}`)
		case r.Method == "GET" && strings.Contains(p, "/compare/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"merge_base_commit": map[string]any{"sha": m.mergeBase},
				"commits":           m.commits,
				"files":             m.files,
			})
		case r.Method == "GET" && strings.HasSuffix(p, "/pulls"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET" && strings.Contains(p, "/git/ref/heads/"):
			m.mu.Lock()
			m.headRefReads++
			sha := m.headSHA
			if m.headSHALater != "" && m.headRefReads >= 2 {
				sha = m.headSHALater
			}
			m.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/x", "object": map[string]any{"sha": sha, "type": "commit"}})
		case r.Method == "GET" && strings.Contains(p, "/git/trees/"):
			var entries []map[string]any
			for _, f := range m.files {
				path := f["filename"].(string)
				if f["status"] == "removed" {
					continue
				}
				mode := "100644"
				if mm, ok := m.treeMode[path]; ok {
					mode = mm
				}
				typ := "blob"
				if mode == "160000" {
					typ = "commit"
				}
				entries = append(entries, map[string]any{"path": path, "mode": mode, "type": typ, "sha": f["sha"]})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": m.headSHA, "tree": entries, "truncated": m.treeTruncated})
		case r.Method == "GET" && strings.Contains(p, "/git/blobs/"):
			sha := p[strings.LastIndex(p, "/")+1:]
			content, ok := m.blobs[sha]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Header.Get("Accept") != "application/vnd.github.v3.raw" {
				m.t.Errorf("blob fetch must ask for raw content, got Accept=%q", r.Header.Get("Accept"))
			}
			w.Header().Set("Content-Type", "application/vnd.github.v3.raw")
			_, _ = io.WriteString(w, content)
		case r.Method == "DELETE" && strings.Contains(p, "/git/refs/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "POST" && strings.HasSuffix(p, "/git/refs"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["sha"] != m.mergeBase {
				m.t.Errorf("scratch ref must be created at the merge base %s, got %v", m.mergeBase, body["sha"])
			}
			if !strings.HasPrefix(body["ref"].(string), "refs/heads/"+signedCommitScratchPrefix) {
				m.t.Errorf("scratch ref must live under refs/heads/%s, got %v", signedCommitScratchPrefix, body["ref"])
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": body["ref"], "object": map[string]any{"sha": m.mergeBase}})
		case r.Method == "PATCH" && strings.Contains(p, "/git/refs/heads/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.refPatch = body
			m.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/x", "object": map[string]any{"sha": body["sha"]}})
		case r.Method == "POST" && strings.HasSuffix(p, "/graphql"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.graphql = body
			m.mu.Unlock()
			_, _ = io.WriteString(w, `{"data":{"createCommitOnBranch":{"commit":{"oid":"5190ed0000000000000000000000000000000000"}}}}`)
		case r.Method == "GET" && strings.Contains(p, "/git/commits/"):
			// CreatePR's duplicate-tree guard (#5111) reads the head commit's
			// tree regardless of signing; answer it so that guard stays quiet.
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": m.headSHA, "tree": map[string]any{"sha": "tree-" + m.headSHA[:6]}})
		case r.Method == "GET" && strings.Contains(p, "/issues/"):
			_, _ = io.WriteString(w, `{"number":1,"title":"ordinary issue","body":"implement the requested change","state":"open"}`)
		case r.Method == "POST" && strings.HasSuffix(p, "/pulls"):
			m.mu.Lock()
			m.created++
			m.mu.Unlock()
			_, _ = io.WriteString(w, `{"number":77,"html_url":"https://github.com/o/r/pull/77"}`)
		case r.Method == "POST" && strings.HasSuffix(p, "/labels"):
			_, _ = io.WriteString(w, `[]`)
		default:
			m.t.Logf("unexpected %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func (m *signedMock) callIndex(prefix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func newSignedMock(t *testing.T) *signedMock {
	t.Helper()
	return &signedMock{
		t:         t,
		headSHA:   "aaaa000000000000000000000000000000000001",
		mergeBase: "bbbb000000000000000000000000000000000002",
		commits: []map[string]any{
			{"sha": "c1", "commit": map[string]any{"message": "[quality] Cover routes/storage.py (ONB-2654)\n\nAdds 51 tests.\n\nSigned-off-by: onboard-ai-hive-bot[bot] <onboard-ai-hive-bot[bot]@users.noreply.github.com>"}},
			{"sha": "c2", "commit": map[string]any{"message": "fixup: no-op assertion\n\nSigned-off-by: onboard-ai-hive-bot[bot] <onboard-ai-hive-bot[bot]@users.noreply.github.com>"}},
		},
		files: []map[string]any{
			{"filename": "tests/new_test.py", "status": "added", "sha": "blob-a"},
			{"filename": "tests/old_test.py", "status": "modified", "sha": "blob-b"},
			{"filename": "tests/gone.py", "status": "removed", "sha": "blob-c"},
			{"filename": "tests/renamed_to.py", "status": "renamed", "previous_filename": "tests/renamed_from.py", "sha": "blob-d"},
		},
		blobs: map[string]string{
			"blob-a": "def test_a():\n    assert True\n",
			"blob-b": "def test_b():\n    assert 1 == 1\n",
			"blob-d": "moved\n",
		},
		treeMode: map[string]string{},
	}
}

func signedTestClient(t *testing.T, srvURL string, enabled bool) *Client {
	t.Helper()
	c := testClient(t, srvURL)
	c.SetSignedCommits(func() bool { return enabled })
	return c
}

func runOneRequest(t *testing.T, c *Client) PRResponse {
	t.Helper()
	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()
	reqPath, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "quality/onb-2654-storage", Title: "[ONB-2654]: Cover routes/storage.py", Body: "Refs ONB-2654", Agent: "quality"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())
	resBytes, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("result file missing: %v", err)
	}
	var res PRResponse
	if err := json.Unmarshal(resBytes, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// The whole flow, in order: compare → scratch ref at the merge base → one
// createCommitOnBranch carrying every addition/deletion and the agents'
// messages → head force-updated to the signed oid → scratch ref deleted →
// PR opened. The result file names the signed commit.
func TestPRRequestWatcher_SignedCommits_ReauthorsBranchBeforeOpening(t *testing.T) {
	m := newSignedMock(t)
	srv := m.server()
	defer srv.Close()
	c := signedTestClient(t, srv.URL, true)

	res := runOneRequest(t, c)

	if !res.OK || res.Number != 77 {
		t.Fatalf("PR should have opened: %+v", res)
	}
	if res.SignedCommit != "5190ed0000000000000000000000000000000000" || res.SignedSkipped != "" {
		t.Fatalf("result should name the signed commit and no skip reason: %+v", res)
	}

	// --- the mutation ---
	if m.graphql == nil {
		t.Fatal("createCommitOnBranch was never called")
	}
	input := m.graphql["variables"].(map[string]any)["input"].(map[string]any)
	branch := input["branch"].(map[string]any)
	if branch["repositoryNameWithOwner"] != "o/r" || branch["branchName"] != signedCommitScratchPrefix+"quality/onb-2654-storage" {
		t.Errorf("mutation must target the scratch branch in o/r, got %v", branch)
	}
	if input["expectedHeadOid"] != m.mergeBase {
		t.Errorf("expectedHeadOid must be the merge base (the scratch ref's tip), got %v", input["expectedHeadOid"])
	}
	msg := input["message"].(map[string]any)
	if msg["headline"] != "[quality] Cover routes/storage.py (ONB-2654)" {
		t.Errorf("headline must be the oldest commit's subject, got %q", msg["headline"])
	}
	body := msg["body"].(string)
	// The trailers arrive from the pane in the bracketed pre-#6276 form; the
	// signed commit must carry them re-addressed to the bracket-free form
	// (#6251), with the name and everything else the agent wrote intact.
	for _, want := range []string{"Adds 51 tests.", "fixup: no-op assertion", "Signed-off-by: onboard-ai-hive-bot[bot] <onboard-ai-hive-bot@hive.kubestellar.io>"} {
		if !strings.Contains(body, want) {
			t.Errorf("body must carry the agents' messages and DCO trailers; missing %q in:\n%s", want, body)
		}
	}
	assertSignoffsDCOValid(t, body)
	fc := input["fileChanges"].(map[string]any)
	adds := map[string]string{}
	for _, a := range fc["additions"].([]any) {
		am := a.(map[string]any)
		raw, err := base64.StdEncoding.DecodeString(am["contents"].(string))
		if err != nil {
			t.Fatalf("addition contents must be base64: %v", err)
		}
		adds[am["path"].(string)] = string(raw)
	}
	wantAdds := map[string]string{
		"tests/new_test.py":   m.blobs["blob-a"],
		"tests/old_test.py":   m.blobs["blob-b"],
		"tests/renamed_to.py": m.blobs["blob-d"],
	}
	for path, want := range wantAdds {
		if adds[path] != want {
			t.Errorf("addition %s: got %q want %q", path, adds[path], want)
		}
	}
	if len(adds) != len(wantAdds) {
		t.Errorf("additions: got %v", adds)
	}
	var dels []string
	for _, d := range fc["deletions"].([]any) {
		dels = append(dels, d.(map[string]any)["path"].(string))
	}
	if strings.Join(dels, ",") != "tests/gone.py,tests/renamed_from.py" {
		t.Errorf("deletions must cover the removed file and the rename source, got %v", dels)
	}

	// --- the ref update ---
	if m.refPatch == nil || m.refPatch["sha"] != "5190ed0000000000000000000000000000000000" || m.refPatch["force"] != true {
		t.Errorf("head must be force-updated to the signed oid, got %v", m.refPatch)
	}

	// --- ordering and cleanup ---
	iGraph := m.callIndex("POST /graphql")
	iPatch := m.callIndex("PATCH /repos/o/r/git/refs/heads/quality/onb-2654-storage")
	iPR := m.callIndex("POST /repos/o/r/pulls")
	if !(iGraph < iPatch && iPatch < iPR) {
		t.Errorf("order must be mutation (%d) → head update (%d) → PR open (%d):\n%s", iGraph, iPatch, iPR, strings.Join(m.calls, "\n"))
	}
	lastDelete := -1
	for i, call := range m.calls {
		if strings.HasPrefix(call, "DELETE /repos/o/r/git/refs/heads/"+signedCommitScratchPrefix) {
			lastDelete = i
		}
	}
	if lastDelete < iPatch {
		t.Errorf("scratch ref must be deleted after the head update; last delete at %d, patch at %d", lastDelete, iPatch)
	}
}

// A change the mutation cannot express — here an executable — must leave the
// branch exactly as pushed, never call the mutation, still open the PR, and
// say why in the result file.
func TestPRRequestWatcher_SignedCommits_FallsBackOnUnsupportedMode(t *testing.T) {
	m := newSignedMock(t)
	m.treeMode["tests/new_test.py"] = "100755"
	srv := m.server()
	defer srv.Close()
	c := signedTestClient(t, srv.URL, true)

	res := runOneRequest(t, c)

	if !res.OK || res.Number != 77 {
		t.Fatalf("PR must still open on the agent's own commits: %+v", res)
	}
	if res.SignedCommit != "" || !strings.Contains(res.SignedSkipped, "mode 100755") {
		t.Errorf("result must report the skip reason and no signed commit: %+v", res)
	}
	if m.graphql != nil || m.refPatch != nil {
		t.Errorf("neither the mutation nor a ref update may run on fallback (graphql=%v patch=%v)", m.graphql != nil, m.refPatch != nil)
	}
}

// The agent pushed again between compare and update: the signed commit does
// not carry those commits, so the head must NOT be replaced.
func TestPRRequestWatcher_SignedCommits_RefusesWhenHeadMoved(t *testing.T) {
	m := newSignedMock(t)
	m.headSHALater = "cccc000000000000000000000000000000000003"
	srv := m.server()
	defer srv.Close()
	c := signedTestClient(t, srv.URL, true)

	res := runOneRequest(t, c)

	if !res.OK {
		t.Fatalf("PR must still open: %+v", res)
	}
	if res.SignedCommit != "" || !strings.Contains(res.SignedSkipped, "moved") {
		t.Errorf("result must report that the head moved: %+v", res)
	}
	if m.refPatch != nil {
		t.Errorf("head must not be updated when it moved under us: %v", m.refPatch)
	}
	if m.callIndex("DELETE /repos/o/r/git/refs/heads/"+signedCommitScratchPrefix) < 0 {
		t.Errorf("scratch ref must be cleaned up on refusal:\n%s", strings.Join(m.calls, "\n"))
	}
}

// Off by default: with the toggle off (or unset) the watcher never calls the
// mutation and never creates, updates, or deletes a ref — exactly the
// pre-feature behaviour. (Reads of the head ref and commit are not asserted
// against: CreatePR's duplicate-tree guard has always made those.)
func TestPRRequestWatcher_SignedCommits_OffTouchesNothing(t *testing.T) {
	m := newSignedMock(t)
	srv := m.server()
	defer srv.Close()

	for _, c := range []*Client{signedTestClient(t, srv.URL, false), testClient(t, srv.URL)} {
		m.mu.Lock()
		m.calls, m.graphql, m.refPatch = nil, nil, nil
		m.mu.Unlock()
		res := runOneRequest(t, c)
		if !res.OK || res.SignedCommit != "" || res.SignedSkipped != "" {
			t.Errorf("unexpected result with signing off: %+v", res)
		}
		for _, call := range m.calls {
			if strings.HasSuffix(call, "/graphql") || strings.Contains(call, "/git/refs") ||
				strings.Contains(call, "/git/trees/") || strings.Contains(call, "/git/blobs/") {
				t.Errorf("signing off must not touch %s", call)
			}
		}
	}
}

// dcoValidEmailRE is the address shape probot-dco's validator accepts
// (local@domain.tld, no brackets) — the same check
// src/deploy/test_entrypoint_system_gitconfig.sh applies to the pane identity.
var dcoValidEmailRE = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}$`)

// assertSignoffsDCOValid fails unless every Signed-off-by line in msg carries
// a bracket-free, syntactically valid address (#6251). At least one trailer
// must be present, or the check would pass vacuously.
func assertSignoffsDCOValid(t *testing.T, msg string) {
	t.Helper()
	trailers := signedTrailerLineRE.FindAllStringSubmatch(msg, -1)
	if len(trailers) == 0 {
		t.Fatalf("expected at least one Signed-off-by trailer in:\n%s", msg)
	}
	for _, m := range trailers {
		email := m[3]
		if strings.ContainsAny(email, "[]") {
			t.Errorf("Signed-off-by address %q carries a bracket; probot-dco rejects it as malformed (#6251)", email)
			continue
		}
		if !dcoValidEmailRE.MatchString(email) {
			t.Errorf("Signed-off-by address %q is not a syntactically valid email address", email)
		}
	}
}

func TestSignedTrailerEmail(t *testing.T) {
	t.Setenv(signedTrailerDomainEnv, "")
	cases := map[string]string{
		// GitHub's bot address, as a pre-#6276 pane writes it.
		"onboard-ai-hive-bot[bot]@users.noreply.github.com": "onboard-ai-hive-bot@hive.kubestellar.io",
		// The same with GitHub's numeric id prefix.
		"4744647+onboard-ai-hive-bot[bot]@users.noreply.github.com": "onboard-ai-hive-bot@hive.kubestellar.io",
		// Domain case must not matter.
		"kubestellar-hive[bot]@Users.NoReply.GitHub.com": "kubestellar-hive@hive.kubestellar.io",
		// Already the #6276 form: untouched.
		"onboard-ai-hive-bot@hive.kubestellar.io": "onboard-ai-hive-bot@hive.kubestellar.io",
		// The legacy pair a hive without a usable App signs with: untouched.
		"hive-bot@kubestellar.io": "hive-bot@kubestellar.io",
		// A human's noreply address is valid and attributable: untouched.
		"some-maintainer@users.noreply.github.com": "some-maintainer@users.noreply.github.com",
		// A [bot] local-part on a foreign domain is not GitHub's shape; leave
		// the agent's own sign-off alone rather than guess.
		"x[bot]@example.org": "x[bot]@example.org",
		// Degenerate: nothing left once the suffix goes; leave it.
		"[bot]@users.noreply.github.com": "[bot]@users.noreply.github.com",
		"not-an-address":                 "not-an-address",
	}
	for in, want := range cases {
		if got := signedTrailerEmail(in); got != want {
			t.Errorf("signedTrailerEmail(%q) = %q, want %q", in, got, want)
		}
	}

	// The domain is operator-configurable, exactly as entrypoint.sh honours it
	// for the pane identity: a plain hostname is used, anything else falls
	// back so the two identities never diverge on an unsafe value.
	t.Setenv(signedTrailerDomainEnv, "bots.example.org")
	if got := signedTrailerEmail("onboard-ai-hive-bot[bot]@users.noreply.github.com"); got != "onboard-ai-hive-bot@bots.example.org" {
		t.Errorf("domain override: got %q", got)
	}
	t.Setenv(signedTrailerDomainEnv, "evil.example\n[core]\n\tsshCommand = evil")
	if got := signedTrailerEmail("onboard-ai-hive-bot[bot]@users.noreply.github.com"); got != "onboard-ai-hive-bot@hive.kubestellar.io" {
		t.Errorf("unsafe domain must fall back to the default, got %q", got)
	}
}

func TestSignedCommitMessage_TrailersBracketFree(t *testing.T) {
	t.Setenv(signedTrailerDomainEnv, "")
	mk := func(msg string) *gh.RepositoryCommit {
		return &gh.RepositoryCommit{Commit: &gh.Commit{Message: gh.Ptr(msg)}}
	}
	_, body := signedCommitMessage([]*gh.RepositoryCommit{
		mk("subject\n\nbody\n\nSigned-off-by: onboard-ai-hive-bot[bot] <onboard-ai-hive-bot[bot]@users.noreply.github.com>"),
		mk("second\n\nSigned-off-by: Gregory Hunt <greg@on-board.ai>\nSigned-off-by: kubestellar-hive[bot] <4744647+kubestellar-hive[bot]@users.noreply.github.com>  "),
	})
	want := "body\n\nSigned-off-by: onboard-ai-hive-bot[bot] <onboard-ai-hive-bot@hive.kubestellar.io>\n\nsecond\n\nSigned-off-by: Gregory Hunt <greg@on-board.ai>\nSigned-off-by: kubestellar-hive[bot] <kubestellar-hive@hive.kubestellar.io>"
	if body != want {
		t.Errorf("trailers must be re-addressed bracket-free and nothing else touched:\n got %q\nwant %q", body, want)
	}
	assertSignoffsDCOValid(t, body)
	// The name keeps its "[bot]": only the address is what DCO validates, and
	// the name is how the commit reads as the App's.
	if !strings.Contains(body, "Signed-off-by: onboard-ai-hive-bot[bot] <") {
		t.Errorf("the sign-off name must keep the bot login, got:\n%s", body)
	}
}

func TestSignedCommitMessage(t *testing.T) {
	mk := func(msg string) *gh.RepositoryCommit {
		return &gh.RepositoryCommit{Commit: &gh.Commit{Message: gh.Ptr(msg)}}
	}
	headline, body := signedCommitMessage([]*gh.RepositoryCommit{mk("subject only\n\nSigned-off-by: a <a@x>")})
	if headline != "subject only" || body != "Signed-off-by: a <a@x>" {
		t.Errorf("single commit: got %q / %q", headline, body)
	}
	headline, body = signedCommitMessage([]*gh.RepositoryCommit{mk("first\n\nbody one"), mk("second\n\nbody two")})
	if headline != "first" || body != "body one\n\nsecond\n\nbody two" {
		t.Errorf("two commits: got %q / %q", headline, body)
	}
	headline, body = signedCommitMessage(nil)
	if headline == "" || body != "" {
		t.Errorf("no commits must still yield a headline, got %q / %q", headline, body)
	}
}

func TestGraphQLEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://api.github.com/":        "https://api.github.com/graphql",
		"https://github.ibm.com/api/v3/": "https://github.ibm.com/api/graphql",
		"http://127.0.0.1:9999/":         "http://127.0.0.1:9999/graphql",
	}
	for in, want := range cases {
		u, _ := url.Parse(in)
		if got := graphQLEndpoint(u); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
	if got := graphQLEndpoint(nil); got != "https://api.github.com/graphql" {
		t.Errorf("nil base: got %s", got)
	}
}
