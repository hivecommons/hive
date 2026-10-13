package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type artifactMock struct {
	t          *testing.T
	headSHA    string
	headExists bool
	private    bool
	graphql    map[string]any
	createdRef map[string]any
	failGraph  bool
}

func newArtifactMockServer(t *testing.T, m *artifactMock) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == "GET" && strings.Contains(p, "/git/ref/heads/"):
			if !m.headExists {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/x", "object": map[string]any{"sha": m.headSHA, "type": "commit"}})
		case r.Method == "POST" && strings.HasSuffix(p, "/git/refs"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.createdRef = body
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": body["ref"], "object": map[string]any{"sha": body["sha"]}})
		case r.Method == "POST" && strings.HasSuffix(p, "/graphql"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.graphql = body
			if m.failGraph {
				_, _ = io.WriteString(w, `{"errors":[{"message":"Expected branch to point to abc"}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"createCommitOnBranch":{"commit":{"oid":"c0ffee0000000000000000000000000000000000"}}}}`)
		case r.Method == "GET" && strings.HasSuffix(p, "/repos/o/r"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "r", "private": m.private, "default_branch": "main"})
		default:
			m.t.Logf("unexpected %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestBranchHead(t *testing.T) {
	m := &artifactMock{t: t, headSHA: "abc123", headExists: true}
	srv := newArtifactMockServer(t, m)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})

	sha, exists, err := c.BranchHead(context.Background(), "o/r", "main")
	if err != nil || !exists || sha != "abc123" {
		t.Fatalf("BranchHead = %q %v %v", sha, exists, err)
	}
	m.headExists = false
	sha, exists, err = c.BranchHead(context.Background(), "o/r", "main")
	if err != nil || exists || sha != "" {
		t.Fatalf("missing branch must report exists=false without error, got %q %v %v", sha, exists, err)
	}
	var nilClient *Client
	if _, _, err := nilClient.BranchHead(context.Background(), "o/r", "main"); err != ErrNoGitHubClient {
		t.Fatalf("nil client: %v", err)
	}
}

func TestCreateBranch(t *testing.T) {
	m := &artifactMock{t: t}
	srv := newArtifactMockServer(t, m)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})

	if err := c.CreateBranch(context.Background(), "o/r", "nous/artifacts/run-1", "base-sha"); err != nil {
		t.Fatal(err)
	}
	if m.createdRef["ref"] != "refs/heads/nous/artifacts/run-1" || m.createdRef["sha"] != "base-sha" {
		t.Fatalf("ref payload: %v", m.createdRef)
	}
	var nilClient *Client
	if err := nilClient.CreateBranch(context.Background(), "o/r", "b", "s"); err != ErrNoGitHubClient {
		t.Fatalf("nil client: %v", err)
	}
}

func TestCommitFilesOnBranch(t *testing.T) {
	m := &artifactMock{t: t}
	srv := newArtifactMockServer(t, m)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})

	oid, err := c.CommitFilesOnBranch(context.Background(), "o/r", "nous/artifacts/run-1", "head-sha", "nous: artifacts", "body text", []ArtifactFile{
		{Path: "/campaigns/run-1/ledger.json", Content: []byte(`{"iterations":[]}`)},
		{Path: "campaigns/run-1/report.md", Content: []byte("# report")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if oid != "c0ffee0000000000000000000000000000000000" {
		t.Fatalf("oid = %q", oid)
	}
	input := m.graphql["variables"].(map[string]any)["input"].(map[string]any)
	if input["expectedHeadOid"] != "head-sha" {
		t.Fatalf("expectedHeadOid = %v", input["expectedHeadOid"])
	}
	branch := input["branch"].(map[string]any)
	if branch["repositoryNameWithOwner"] != "o/r" || branch["branchName"] != "nous/artifacts/run-1" {
		t.Fatalf("branch = %v", branch)
	}
	msg := input["message"].(map[string]any)
	if msg["headline"] != "nous: artifacts" || msg["body"] != "body text" {
		t.Fatalf("message = %v", msg)
	}
	adds := input["fileChanges"].(map[string]any)["additions"].([]any)
	if len(adds) != 2 {
		t.Fatalf("additions = %v", adds)
	}
	first := adds[0].(map[string]any)
	if first["path"] != "campaigns/run-1/ledger.json" {
		t.Fatalf("leading slash must be stripped: %v", first["path"])
	}
	if first["contents"] != base64.StdEncoding.EncodeToString([]byte(`{"iterations":[]}`)) {
		t.Fatalf("contents must be base64: %v", first["contents"])
	}

	if _, err := c.CommitFilesOnBranch(context.Background(), "o/r", "b", "h", "x", "", nil); err == nil || !strings.Contains(err.Error(), "no files") {
		t.Fatalf("empty file list must be refused, got %v", err)
	}
	if _, err := c.CommitFilesOnBranch(context.Background(), "o/r", "b", "h", "x", "", []ArtifactFile{{Path: " / "}}); err == nil || !strings.Contains(err.Error(), "empty file path") {
		t.Fatalf("empty path must be refused, got %v", err)
	}
	m.failGraph = true
	if _, err := c.CommitFilesOnBranch(context.Background(), "o/r", "b", "h", "x", "", []ArtifactFile{{Path: "a", Content: []byte("1")}}); err == nil || !strings.Contains(err.Error(), "Expected branch to point") {
		t.Fatalf("graphql error must surface, got %v", err)
	}
	var nilClient *Client
	if _, err := nilClient.CommitFilesOnBranch(context.Background(), "o/r", "b", "h", "x", "", nil); err != ErrNoGitHubClient {
		t.Fatalf("nil client: %v", err)
	}
}

func TestRepoIsPrivate(t *testing.T) {
	m := &artifactMock{t: t, private: true}
	srv := newArtifactMockServer(t, m)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})

	private, err := c.RepoIsPrivate(context.Background(), "o/r")
	if err != nil || !private {
		t.Fatalf("RepoIsPrivate = %v %v", private, err)
	}
	m.private = false
	private, err = c.RepoIsPrivate(context.Background(), "o/r")
	if err != nil || private {
		t.Fatalf("public repo: %v %v", private, err)
	}
	if _, err := c.RepoIsPrivate(context.Background(), "o/missing"); err == nil {
		t.Fatal("404 must surface as an error")
	}
	var nilClient *Client
	if _, err := nilClient.RepoIsPrivate(context.Background(), "o/r"); err != ErrNoGitHubClient {
		t.Fatalf("nil client: %v", err)
	}
}
