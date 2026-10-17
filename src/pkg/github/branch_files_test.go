package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type branchFilesMock struct {
	mu        sync.Mutex
	refs      map[string]string
	pulls     string
	created   map[string]any
	patched   map[string]any
	graphql   map[string]any
	graphResp string
	failRef   bool
	failWrite bool
}

func (m *branchFilesMock) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/pulls"):
			if r.URL.Query().Get("head") != "o:pub" || r.URL.Query().Get("state") != "open" {
				t.Errorf("unexpected PR list query %q", r.URL.RawQuery)
			}
			if m.pulls == "" {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			_, _ = io.WriteString(w, m.pulls)
		case r.Method == http.MethodGet && strings.Contains(p, "/git/ref/heads/"):
			name := p[strings.Index(p, "/git/ref/heads/")+len("/git/ref/heads/"):]
			if m.failRef {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			sha, ok := m.refs[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/" + name, "object": map[string]any{"sha": sha}})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/git/refs"):
			if m.failWrite {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			_ = json.NewDecoder(r.Body).Decode(&m.created)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": m.created["ref"], "object": map[string]any{"sha": m.created["sha"]}})
		case r.Method == http.MethodPatch && strings.Contains(p, "/git/refs/heads/"):
			if m.failWrite {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			_ = json.NewDecoder(r.Body).Decode(&m.patched)
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/pub", "object": map[string]any{"sha": m.patched["sha"]}})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/graphql"):
			_ = json.NewDecoder(r.Body).Decode(&m.graphql)
			_, _ = io.WriteString(w, m.graphResp)
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestOpenPRForHead(t *testing.T) {
	m := &branchFilesMock{pulls: `[{"number":9,"html_url":"https://github.com/o/r/pull/9"}]`}
	srv := m.server(t)
	defer srv.Close()
	c := testClient(t, srv.URL)

	n, url, ok, err := c.OpenPRForHead(context.Background(), "o/r", " pub ")
	if err != nil || !ok || n != 9 || url != "https://github.com/o/r/pull/9" {
		t.Fatalf("got %d %q %v %v", n, url, ok, err)
	}
	m.pulls = `[]`
	if _, _, ok, err := c.OpenPRForHead(context.Background(), "r", "pub"); err != nil || ok {
		t.Fatalf("no open PR: ok=%v err=%v", ok, err)
	}
	m.pulls = ""
	if _, _, _, err := c.OpenPRForHead(context.Background(), "o/r", "pub"); err == nil {
		t.Fatal("list failure must surface")
	}
}

func TestResetBranch_CreatesMissingBranchAtBase(t *testing.T) {
	m := &branchFilesMock{refs: map[string]string{"main": "base-sha"}}
	srv := m.server(t)
	defer srv.Close()
	c := testClient(t, srv.URL)

	sha, err := c.ResetBranch(context.Background(), "o/r", "pub", "main")
	if err != nil || sha != "base-sha" {
		t.Fatalf("got %q %v", sha, err)
	}
	if m.created["ref"] != "refs/heads/pub" || m.created["sha"] != "base-sha" || m.patched != nil {
		t.Fatalf("created=%v patched=%v", m.created, m.patched)
	}
}

func TestResetBranch_ForceUpdatesExistingBranch(t *testing.T) {
	m := &branchFilesMock{refs: map[string]string{"main": "base-sha", "pub": "stale"}}
	srv := m.server(t)
	defer srv.Close()
	c := testClient(t, srv.URL)

	sha, err := c.ResetBranch(context.Background(), "o/r", "pub", "main")
	if err != nil || sha != "base-sha" {
		t.Fatalf("got %q %v", sha, err)
	}
	if m.patched["sha"] != "base-sha" || m.patched["force"] != true || m.created != nil {
		t.Fatalf("created=%v patched=%v", m.created, m.patched)
	}
}

func TestResetBranch_Errors(t *testing.T) {
	ctx := context.Background()
	var nilClient *Client
	if _, err := nilClient.ResetBranch(ctx, "o/r", "pub", "main"); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client: %v", err)
	}
	m := &branchFilesMock{refs: map[string]string{"main": "base-sha"}}
	srv := m.server(t)
	defer srv.Close()
	c := testClient(t, srv.URL)
	if _, err := c.ResetBranch(ctx, "o/r", "", "main"); err == nil {
		t.Error("empty branch must fail")
	}
	if _, err := c.ResetBranch(ctx, "o/r", "main", "main"); err == nil {
		t.Error("resetting base onto itself must fail")
	}
	if _, err := c.ResetBranch(ctx, "o/r", "pub", "missing"); err == nil {
		t.Error("missing base must fail")
	}
	m.refs["empty"] = ""
	if _, err := c.ResetBranch(ctx, "o/r", "pub", "empty"); err == nil {
		t.Error("empty base sha must fail")
	}
	m.failWrite = true
	if _, err := c.ResetBranch(ctx, "o/r", "pub", "main"); err == nil {
		t.Error("create failure must surface")
	}
	m.refs["pub"] = "stale"
	if _, err := c.ResetBranch(ctx, "o/r", "pub", "main"); err == nil {
		t.Error("update failure must surface")
	}
	m.failRef = true
	if _, err := c.ResetBranch(ctx, "o/r", "pub", "main"); err == nil {
		t.Error("ref read failure must surface")
	}
}

func TestResetBranch_BranchLookupFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/git/ref/heads/main") {
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"base-sha"}}`)
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	if _, err := testClient(t, srv.URL).ResetBranch(context.Background(), "o/r", "pub", "main"); err == nil {
		t.Fatal("a non-404 branch lookup failure must not create or move the branch")
	}
}

func TestCommitFilesOnBranch(t *testing.T) {
	m := &branchFilesMock{graphResp: `{"data":{"createCommitOnBranch":{"commit":{"oid":"new-oid"}}}}`}
	srv := m.server(t)
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.SetAppBotLogin("hive-app[bot]")

	oid, err := c.CommitFilesOnBranch(context.Background(), "o/r", "pub", "head-sha", "docs: update", "- b.md\n- a.md",
		map[string]string{"docs/b.md": "B", "docs/a.md": "A"})
	if err != nil || oid != "new-oid" {
		t.Fatalf("got %q %v", oid, err)
	}
	input := m.graphql["variables"].(map[string]any)["input"].(map[string]any)
	branch := input["branch"].(map[string]any)
	if branch["repositoryNameWithOwner"] != "o/r" || branch["branchName"] != "pub" || input["expectedHeadOid"] != "head-sha" {
		t.Fatalf("input: %v", input)
	}
	msg := input["message"].(map[string]any)
	if msg["headline"] != "docs: update" || !strings.HasSuffix(msg["body"].(string), "\n\n"+BotSignedOffByTrailer("hive-app[bot]")) {
		t.Fatalf("message: %v", msg)
	}
	adds := input["fileChanges"].(map[string]any)["additions"].([]any)
	if len(adds) != 2 {
		t.Fatalf("additions: %v", adds)
	}
	first := adds[0].(map[string]any)
	raw, _ := base64.StdEncoding.DecodeString(first["contents"].(string))
	if first["path"] != "docs/a.md" || string(raw) != "A" {
		t.Fatalf("additions must be sorted and base64: %v", adds)
	}

	c.SetAppBotLogin("")
	if _, err := c.CommitFilesOnBranch(context.Background(), "o/r", "pub", "head-sha", "h", "", map[string]string{"x.md": "x"}); err != nil {
		t.Fatal(err)
	}
	if _, has := m.graphql["variables"].(map[string]any)["input"].(map[string]any)["message"].(map[string]any)["body"]; has {
		t.Fatal("no body and no login must send no message body")
	}
}

func TestCommitFilesOnBranch_Errors(t *testing.T) {
	ctx := context.Background()
	files := map[string]string{"a.md": "a"}
	var nilClient *Client
	if _, err := nilClient.CommitFilesOnBranch(ctx, "o/r", "pub", "h", "x", "", files); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client: %v", err)
	}
	m := &branchFilesMock{graphResp: `{"errors":[{"message":"expected head oid mismatch"}]}`}
	srv := m.server(t)
	defer srv.Close()
	c := testClient(t, srv.URL)
	for name, call := range map[string]func() error{
		"no branch":   func() error { _, err := c.CommitFilesOnBranch(ctx, "o/r", "", "h", "x", "", files); return err },
		"no head":     func() error { _, err := c.CommitFilesOnBranch(ctx, "o/r", "pub", "", "x", "", files); return err },
		"no headline": func() error { _, err := c.CommitFilesOnBranch(ctx, "o/r", "pub", "h", " ", "", files); return err },
		"no files":    func() error { _, err := c.CommitFilesOnBranch(ctx, "o/r", "pub", "h", "x", "", nil); return err },
		"too large": func() error {
			_, err := c.CommitFilesOnBranch(ctx, "o/r", "pub", "h", "x", "", map[string]string{"big": strings.Repeat("x", signedCommitMaxBytes+1)})
			return err
		},
		"graphql error": func() error { _, err := c.CommitFilesOnBranch(ctx, "o/r", "pub", "h", "x", "", files); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if m.graphql == nil {
		t.Error("the graphql case must reach the mutation")
	}
}

func TestOpenPRForHead_NilClient(t *testing.T) {
	var c *Client
	if _, _, _, err := c.OpenPRForHead(context.Background(), "o/r", "pub"); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("got %v", err)
	}
}
