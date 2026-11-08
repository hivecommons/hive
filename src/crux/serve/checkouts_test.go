package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/checkout"
	"github.com/hivecommons/hive/crux/lanes"
)

// checkoutFixtureRepo is a bare repo with one commit, as a file:// URL.
func checkoutFixtureRepo(t *testing.T) (repo, sha string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("f"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "f")
	git("commit", "-q", "-m", "c")
	sha = git("rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "fixture.git")
	git("clone", "-q", "--bare", work, bare)
	return "file://" + bare, sha
}

type checkoutFixture struct {
	t      *testing.T
	h      http.Handler
	s      *Server
	inUse  map[string]bool
	tokens map[string]string
}

// newCheckoutFixture is a token-bearing server with a checkout service
// whose allowlist admits the fixture repo's directory, and two lane
// projects (atlas, beacon) besides the operator token.
func newCheckoutFixture(t *testing.T, repo string) *checkoutFixture {
	t.Helper()
	tokens := map[string]string{"atlas": strings.Repeat("a", 32), "beacon": strings.Repeat("b", 32), "operator": "agent-admin"}
	cfg := lanes.Config{Principals: []lanes.Principal{{Project: "atlas", TokenEnv: "ATLAS"}, {Project: "beacon", TokenEnv: "BEACON"}}}
	auth, err := lanes.Bind(cfg, func(env string) string { return tokens[strings.ToLower(env)] }, tokens["operator"])
	if err != nil {
		t.Fatal(err)
	}
	f := &checkoutFixture{t: t, inUse: map[string]bool{}, tokens: tokens}
	svc, err := checkout.New(checkout.Config{
		Dir:          filepath.Join(t.TempDir(), "checkouts"),
		AllowedRepos: []string{"file://" + filepath.Dir(strings.TrimPrefix(repo, "file://"))},
		// The map is the test's own hand on the hook; a wired run service
		// (withRuns) is consulted too, the way crux serve wires it.
		InUse: func(id string) bool { return f.inUse[id] || (f.s.Runs != nil && f.s.Runs.InUse(id)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.s = New(newFakeRuntime())
	f.s.Token, f.s.LaneAuth, f.s.Checkouts = tokens["operator"], auth, svc
	f.h = f.s.Handler()
	return f
}

// call sends method path as who ("operator", "atlas", "beacon" or "") and
// decodes a 2xx body into out, returning the status and any api.Error.
func (f *checkoutFixture) call(who, method, path string, body, out any) (int, api.Error) {
	f.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			f.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if who != "" {
		req.Header.Set("Authorization", "Bearer "+f.tokens[who])
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	var e api.Error
	if rec.Code >= 300 {
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
	} else if out != nil && rec.Code != http.StatusNoContent {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			f.t.Fatalf("decode %s %s: %v: %s", method, path, err, rec.Body)
		}
	}
	return rec.Code, e
}

func TestCheckoutRoutesOwnershipAndListing(t *testing.T) {
	repo, sha := checkoutFixtureRepo(t)
	f := newCheckoutFixture(t, repo)

	// A project creates a checkout; the owner defaults to the project.
	var mine api.Checkout
	if code, e := f.call("atlas", http.MethodPost, api.PathCheckouts, api.CheckoutRequest{Repo: repo, Commit: sha, TTL: "72h"}, &mine); code != http.StatusCreated {
		t.Fatalf("atlas create: %d %+v", code, e)
	}
	if mine.Owner != "atlas" || mine.Commit != sha || mine.Path == "" || mine.TTL != "72h0m0s" || mine.ExpiresAt == nil {
		t.Fatalf("created = %+v", mine)
	}
	// A project may not name another owner; the operator may.
	if code, e := f.call("atlas", http.MethodPost, api.PathCheckouts, api.CheckoutRequest{Repo: repo, Commit: sha, Owner: "beacon"}, nil); code != http.StatusForbidden || e.Code != api.CodeForbidden {
		t.Fatalf("atlas create for beacon: %d %+v", code, e)
	}
	var theirs api.Checkout
	if code, e := f.call("operator", http.MethodPost, api.PathCheckouts, api.CheckoutRequest{Repo: repo, Commit: sha, Owner: "beacon"}, &theirs); code != http.StatusCreated || theirs.Owner != "beacon" {
		t.Fatalf("operator create for beacon: %d %+v %+v", code, e, theirs)
	}
	var ops api.Checkout
	if code, _ := f.call("operator", http.MethodPost, api.PathCheckouts, api.CheckoutRequest{Repo: repo, Commit: sha}, &ops); code != http.StatusCreated || ops.Owner != DefaultOperatorName {
		t.Fatalf("operator create for itself: %d %+v", code, ops)
	}

	// Listing: the operator sees all and can filter; a project sees only
	// its own whatever it asks for.
	var list api.CheckoutList
	if code, _ := f.call("operator", http.MethodGet, api.PathCheckouts, nil, &list); code != http.StatusOK || len(list.Checkouts) != 3 {
		t.Fatalf("operator list: %d %d", code, len(list.Checkouts))
	}
	if code, _ := f.call("operator", http.MethodGet, api.PathCheckouts+"?owner=beacon", nil, &list); code != http.StatusOK || len(list.Checkouts) != 1 || list.Checkouts[0].ID != theirs.ID {
		t.Fatalf("operator list beacon: %d %+v", code, list)
	}
	if code, _ := f.call("atlas", http.MethodGet, api.PathCheckouts+"?owner=beacon", nil, &list); code != http.StatusOK || len(list.Checkouts) != 1 || list.Checkouts[0].ID != mine.ID {
		t.Fatalf("atlas list: %d %+v", code, list)
	}

	// Get: owner and operator yes, another project no.
	var got api.Checkout
	if code, _ := f.call("atlas", http.MethodGet, api.PathCheckouts+"/"+mine.ID, nil, &got); code != http.StatusOK || got.ID != mine.ID {
		t.Fatalf("atlas get own: %d %+v", code, got)
	}
	if code, e := f.call("beacon", http.MethodGet, api.PathCheckouts+"/"+mine.ID, nil, nil); code != http.StatusForbidden || e.Code != api.CodeForbidden || !strings.Contains(e.Error, "owned by \"atlas\"") {
		t.Fatalf("beacon get atlas's: %d %+v", code, e)
	}
	if code, _ := f.call("operator", http.MethodGet, api.PathCheckouts+"/"+mine.ID, nil, &got); code != http.StatusOK {
		t.Fatalf("operator get: %d", code)
	}
	if code, e := f.call("operator", http.MethodGet, api.PathCheckouts+"/chk_000000000000", nil, nil); code != http.StatusNotFound || e.Code != api.CodeNotFound {
		t.Fatalf("get unknown: %d %+v", code, e)
	}

	// Delete: two drivers cannot delete each other's checkouts; live runs
	// block deletion with 409; the owner and the operator can delete.
	if code, e := f.call("beacon", http.MethodDelete, api.PathCheckouts+"/"+mine.ID, nil, nil); code != http.StatusForbidden || e.Code != api.CodeForbidden {
		t.Fatalf("beacon delete atlas's: %d %+v", code, e)
	}
	f.inUse[mine.ID] = true
	if code, e := f.call("atlas", http.MethodDelete, api.PathCheckouts+"/"+mine.ID, nil, nil); code != http.StatusConflict || e.Code != api.CodeConflict {
		t.Fatalf("delete in use: %d %+v", code, e)
	}
	f.inUse[mine.ID] = false
	if code, e := f.call("atlas", http.MethodDelete, api.PathCheckouts+"/"+mine.ID, nil, nil); code != http.StatusNoContent {
		t.Fatalf("atlas delete own: %d %+v", code, e)
	}
	if code, e := f.call("operator", http.MethodDelete, api.PathCheckouts+"/"+theirs.ID, nil, nil); code != http.StatusNoContent {
		t.Fatalf("operator delete beacon's: %d %+v", code, e)
	}
	if code, _ := f.call("operator", http.MethodGet, api.PathCheckouts, nil, &list); code != http.StatusOK || len(list.Checkouts) != 1 || list.Checkouts[0].ID != ops.ID {
		t.Fatalf("list after deletes: %d %+v", code, list)
	}
	// Agent RPCs still refuse the project token.
	if code, e := f.call("atlas", http.MethodGet, api.PathHealth, nil, nil); code != http.StatusUnauthorized || e.Code != api.CodeUnauthorized {
		t.Fatalf("project token on health: %d %+v", code, e)
	}
	if code, _ := f.call("", http.MethodGet, api.PathCheckouts, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("no token on checkouts: %d", code)
	}
}

func TestCheckoutRouteErrorMapping(t *testing.T) {
	repo, sha := checkoutFixtureRepo(t)
	f := newCheckoutFixture(t, repo)
	cases := []struct {
		name string
		body any
		code int
		err  string
		msg  string
	}{
		{"short sha", api.CheckoutRequest{Repo: repo, Commit: sha[:7]}, http.StatusBadRequest, api.CodeBadRequest, "full 40-hex SHA"},
		{"branch", api.CheckoutRequest{Repo: repo, Commit: "main"}, http.StatusBadRequest, api.CodeBadRequest, "tag or branch"},
		{"bad ttl", api.CheckoutRequest{Repo: repo, Commit: sha, TTL: "soon"}, http.StatusBadRequest, api.CodeBadRequest, "ttl"},
		{"off allowlist", api.CheckoutRequest{Repo: "https://github.com/org/name.git", Commit: sha}, http.StatusForbidden, api.CodeForbidden, "allowed prefixes"},
		{"over cap", api.CheckoutRequest{Repo: repo, Commit: sha, MaxBytes: 1}, http.StatusRequestEntityTooLarge, api.CodeTooLarge, "after cloning"},
		{"unknown field", map[string]any{"repo": repo, "commit": sha, "branch": "main"}, http.StatusBadRequest, api.CodeBadRequest, "unknown field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, e := f.call("operator", http.MethodPost, api.PathCheckouts, tc.body, nil)
			if code != tc.code || e.Code != tc.err || !strings.Contains(e.Error, tc.msg) {
				t.Fatalf("%d %+v, want %d %s containing %q", code, e, tc.code, tc.err, tc.msg)
			}
		})
	}
	// The remaining sentinels map through writeCheckoutError directly.
	for err, want := range map[error][2]any{
		checkout.ErrMismatch:    {http.StatusBadGateway, api.CodeUpstream},
		checkout.ErrUnsupported: {http.StatusNotImplemented, api.CodeUnsupported},
		checkout.ErrGit:         {http.StatusInternalServerError, api.CodeRuntime},
		errors.New("other"):     {http.StatusInternalServerError, api.CodeRuntime},
	} {
		rec := httptest.NewRecorder()
		f.s.writeCheckoutError(rec, err)
		var e api.Error
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if rec.Code != want[0].(int) || e.Code != want[1].(string) {
			t.Errorf("%v → %d %s, want %v", err, rec.Code, e.Code, want)
		}
	}
}

func TestCheckoutRoutesWithoutService(t *testing.T) {
	s := New(newFakeRuntime())
	s.Token = "t"
	h := s.Handler()
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, api.PathCheckouts}, {http.MethodGet, api.PathCheckouts},
		{http.MethodGet, api.PathCheckouts + "/chk_000000000000"}, {http.MethodDelete, api.PathCheckouts + "/chk_000000000000"},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer t")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var e api.Error
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if rec.Code != http.StatusNotImplemented || e.Code != api.CodeNotSupported {
			t.Fatalf("%s %s without service: %d %+v", c.method, c.path, rec.Code, e)
		}
	}
}

func TestCheckoutClientRoundTrip(t *testing.T) {
	repo, sha := checkoutFixtureRepo(t)
	f := newCheckoutFixture(t, repo)
	srv := httptest.NewServer(f.h)
	defer srv.Close()
	c, err := api.NewClient(srv.URL, f.tokens["atlas"])
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rec, err := c.CreateCheckout(ctx, api.CheckoutRequest{Repo: repo, Commit: sha})
	if err != nil || rec.Owner != "atlas" {
		t.Fatalf("CreateCheckout = %+v %v", rec, err)
	}
	got, err := c.GetCheckout(ctx, rec.ID)
	if err != nil || got.ID != rec.ID {
		t.Fatalf("GetCheckout = %+v %v", got, err)
	}
	list, err := c.ListCheckouts(ctx, "atlas")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListCheckouts = %v %v", list, err)
	}
	if err := c.DeleteCheckout(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteCheckout(ctx, rec.ID); !api.IsNotFound(err) {
		t.Fatalf("second delete = %v", err)
	}
	var se *api.StatusError
	if _, err := c.CreateCheckout(ctx, api.CheckoutRequest{Repo: repo, Commit: "main"}); !errors.As(err, &se) || se.Body.Code != api.CodeBadRequest {
		t.Fatalf("branch pin = %v", err)
	}
}
