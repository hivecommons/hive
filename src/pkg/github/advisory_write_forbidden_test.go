package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// writeForbiddenMux serves an advisory issue carrying one bot-authored digest
// comment and lets each write path be forced to fail: patchStatus/postStatus
// of 0 mean success. repoFullName is what GET /repos/{owner}/{repo} reports as
// full_name — set it to a different owner to simulate a transferred repo.
func writeForbiddenMux(org, repo string, patchStatus, postStatus int, repoFullName string, calls map[string]int) *http.ServeMux {
	mux := http.NewServeMux()
	forbid := func(w http.ResponseWriter, status int) {
		w.WriteHeader(status)
		msg := writeForbiddenMessage
		if status == http.StatusNotFound {
			msg = "Not Found"
		}
		json.NewEncoder(w).Encode(map[string]any{"message": msg})
	}
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s", org, repo), func(w http.ResponseWriter, r *http.Request) {
		calls["repo-get"]++
		json.NewEncoder(w).Encode(map[string]any{"id": 1266532887, "full_name": repoFullName, "has_issues": true})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 5482991646, "body": advisoryDigestPrefix + " — existing", "user": map[string]any{"login": "hive-app[bot]", "type": "Bot"}},
			})
		case "POST":
			calls["post"]++
			if postStatus != 0 {
				forbid(w, postStatus)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": 9999, "user": map[string]any{"login": "hive-app[bot]"}})
		}
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/comments/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" {
			return
		}
		calls["patch"]++
		if patchStatus != 0 {
			forbid(w, patchStatus)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 5482991646, "user": map[string]any{"login": "hive-app[bot]"}})
	})
	return mux
}

// A 403 on the PATCH of an existing digest comment can never succeed on retry
// — GitHub refuses this identity for that comment. The post path must fall
// back to CREATING a fresh comment under the hive's own identity instead of
// returning the error and re-PATCHing every cycle (#11791).
func TestPostAdvisoryDigest_PatchForbiddenFallsBackToCreate(t *testing.T) {
	org, repo := "IBM", "docling-pipelines"
	calls := map[string]int{}
	server := httptest.NewServer(writeForbiddenMux(org, repo, http.StatusForbidden, 0, org+"/"+repo, calls))
	defer server.Close()

	c := newAppTestClient(t, server, org, repo, "hive-app[bot]")
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+" — new"); err != nil {
		t.Fatalf("PostAdvisoryDigest: %v", err)
	}
	if calls["patch"] != 1 || calls["post"] != 1 {
		t.Errorf("expected 1 PATCH then 1 POST, got patch=%d post=%d", calls["patch"], calls["post"])
	}
	if calls["repo-get"] != 0 {
		t.Errorf("repo probe should not run on a successful fallback, ran %d times", calls["repo-get"])
	}
}

// A 404 on the PATCH means the comment was deleted out from under us; same
// remedy as the 403 — create a new one.
func TestPostAdvisoryDigest_PatchNotFoundFallsBackToCreate(t *testing.T) {
	org, repo := "testorg", "testrepo"
	calls := map[string]int{}
	server := httptest.NewServer(writeForbiddenMux(org, repo, http.StatusNotFound, 0, org+"/"+repo, calls))
	defer server.Close()

	c := newAppTestClient(t, server, org, repo, "hive-app[bot]")
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+" — new"); err != nil {
		t.Fatalf("PostAdvisoryDigest: %v", err)
	}
	if calls["post"] != 1 {
		t.Errorf("expected the digest to be re-created, post=%d", calls["post"])
	}
}

// A transient PATCH failure (5xx) is NOT a reason to create a duplicate: the
// error is returned and the next cycle retries the edit.
func TestPostAdvisoryDigest_PatchServerErrorDoesNotCreate(t *testing.T) {
	org, repo := "testorg", "testrepo"
	calls := map[string]int{}
	server := httptest.NewServer(writeForbiddenMux(org, repo, http.StatusBadGateway, 0, org+"/"+repo, calls))
	defer server.Close()

	c := newAppTestClient(t, server, org, repo, "hive-app[bot]")
	err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+" — new")
	if err == nil {
		t.Fatal("expected the 502 to be returned")
	}
	if calls["post"] != 0 {
		t.Errorf("a transient PATCH failure must not create a duplicate comment, post=%d", calls["post"])
	}
}

// When the fallback CREATE is refused too and the repo's canonical full_name
// has a different owner, the error names the transfer and the exact remedy —
// not the generic "tick the repo in the installation" story. This is the live
// IBM/docling-pipelines → docling-project/docling-pipelines case (#11791).
func TestPostAdvisoryDigest_WriteForbiddenNamesRepoTransfer(t *testing.T) {
	org, repo := "IBM", "docling-pipelines"
	calls := map[string]int{}
	server := httptest.NewServer(writeForbiddenMux(org, repo, http.StatusForbidden, http.StatusForbidden, "docling-project/docling-pipelines", calls))
	defer server.Close()

	c := newAppTestClient(t, server, org, repo, "hive-app[bot]")
	err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+" — new")
	if err == nil {
		t.Fatal("expected an error when both PATCH and POST are forbidden")
	}
	var moved *RepoMovedError
	if !errors.As(err, &moved) {
		t.Fatalf("expected *RepoMovedError in chain, got %T: %v", err, err)
	}
	if moved.Configured != "IBM/docling-pipelines" || moved.Current != "docling-project/docling-pipelines" {
		t.Errorf("unexpected move %+v", *moved)
	}
	for _, want := range []string{"transferred to docling-project/docling-pipelines", "Install the hive App on 'docling-project'", writeForbiddenMessage} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	if calls["repo-get"] != 1 {
		t.Errorf("expected exactly one repo probe, got %d", calls["repo-get"])
	}
}

// Both writes forbidden but the repo has NOT moved: the original 403 is
// returned unchanged (the caller's write-forbidden classifier owns it).
func TestPostAdvisoryDigest_WriteForbiddenWithoutMoveIsPlain403(t *testing.T) {
	org, repo := "testorg", "testrepo"
	calls := map[string]int{}
	server := httptest.NewServer(writeForbiddenMux(org, repo, http.StatusForbidden, http.StatusForbidden, org+"/"+repo, calls))
	defer server.Close()

	c := newAppTestClient(t, server, org, repo, "hive-app[bot]")
	err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+" — new")
	if err == nil {
		t.Fatal("expected an error")
	}
	var moved *RepoMovedError
	if errors.As(err, &moved) {
		t.Errorf("unmoved repo must not be reported as moved: %v", err)
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), writeForbiddenMessage) {
		t.Errorf("expected the raw 403 to surface, got %v", err)
	}
}

func TestProbeRepoMoved(t *testing.T) {
	cases := []struct {
		name     string
		fullName string
		moved    bool
	}{
		{"same owner", "IBM/docling-pipelines", false},
		{"same owner different case", "ibm/Docling-Pipelines", false},
		{"transferred", "docling-project/docling-pipelines", true},
		{"empty full_name", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := map[string]int{}
			server := httptest.NewServer(writeForbiddenMux("IBM", "docling-pipelines", 0, 0, tc.fullName, calls))
			defer server.Close()
			c := newTestClient(t, server, "IBM", []string{"docling-pipelines"})
			moved, err := c.ProbeRepoMoved(context.Background(), "IBM", "docling-pipelines")
			if err != nil {
				t.Fatalf("ProbeRepoMoved: %v", err)
			}
			if (moved != nil) != tc.moved {
				t.Errorf("moved=%v, want %v", moved != nil, tc.moved)
			}
		})
	}
	var nilClient *Client
	if _, err := nilClient.ProbeRepoMoved(context.Background(), "a", "b"); !errors.Is(err, ErrNoGitHubClient) {
		t.Errorf("nil client: got %v, want ErrNoGitHubClient", err)
	}
}

func TestIsWriteForbiddenError(t *testing.T) {
	if isWriteForbiddenError(nil) {
		t.Error("nil is not forbidden")
	}
	if !isWriteForbiddenError(errors.New("PATCH https://api.github.com/x: 403 Resource not accessible by integration []")) {
		t.Error("text form should match")
	}
	if isWriteForbiddenError(errors.New("403 API rate limit exceeded")) {
		t.Error("a rate-limit 403 is not write-forbidden")
	}
	if isDigestEditUnrecoverable(errors.New("502 Bad Gateway")) {
		t.Error("5xx must stay retryable")
	}
}
