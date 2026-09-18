package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests for PostRecommendations (#7570): the forge write path behind the
// "what should I merge next?" issue. The anti-spam contract is the whole
// point of the feature, so each part of it is pinned here:
//
//   - one issue, found by title, rewritten in place — never a duplicate;
//   - the skip-if-unchanged baseline is THE BODY ON THE ISSUE, not process
//     memory, so restarts cannot cause rewrites;
//   - labels are sent on creation only;
//   - no issue is opened just to say there is nothing to say;
//   - a failed lookup must NOT fall through to create (duplicate issue);
//   - raw @mentions never reach the wire.

// recServer serves the two endpoints PostRecommendations touches and records
// every write so tests can assert on exactly what reached the forge.
type recServer struct {
	*httptest.Server
	listStatus int // GET /repos/o/r/issues status; 200 by default
	existing   []map[string]any

	createCalls  int
	createBodies []map[string]any
	editCalls    int
	editBodies   []map[string]any
}

func newRecServer(t *testing.T, org, repo string) *recServer {
	t.Helper()
	s := &recServer{listStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if s.listStatus != http.StatusOK {
				w.WriteHeader(s.listStatus)
				return
			}
			json.NewEncoder(w).Encode(s.existing)
		case http.MethodPost:
			s.createCalls++
			payload := map[string]any{}
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &payload)
			s.createBodies = append(s.createBodies, payload)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"number": 77, "html_url": "https://example.test/issues/77",
			})
		}
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			s.editCalls++
			payload := map[string]any{}
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &payload)
			s.editBodies = append(s.editBodies, payload)
			json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "html_url": "https://example.test/issues/42",
			})
		}
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

const recTestTitle = "🐝 Hive recommendations — what to merge next"

func TestPostRecommendations_CreatesWithLabelsWhenWorthOpening(t *testing.T) {
	org, repo := "testorg", "testrepo"
	srv := newRecServer(t, org, repo)
	c := newTestClient(t, srv.Server, org, []string{repo})

	res, err := c.PostRecommendations(context.Background(), repo, recTestTitle,
		"**1 ready.**", []string{"hive", "digest"}, true)
	if err != nil {
		t.Fatalf("PostRecommendations: %v", err)
	}
	if !res.Created || res.Number != 77 {
		t.Fatalf("want Created number 77, got %+v", res)
	}
	if srv.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", srv.createCalls)
	}
	labels, _ := srv.createBodies[0]["labels"].([]any)
	if len(labels) != 2 {
		t.Fatalf("labels on create = %v, want the 2 configured labels", srv.createBodies[0]["labels"])
	}
}

func TestPostRecommendations_DoesNotOpenAnIssueToSayNothing(t *testing.T) {
	org, repo := "testorg", "testrepo"
	srv := newRecServer(t, org, repo)
	c := newTestClient(t, srv.Server, org, []string{repo})

	// No issue exists and nothing is actionable: the contract is "do
	// nothing at all", not "open an empty digest in someone's repo".
	res, err := c.PostRecommendations(context.Background(), repo, recTestTitle,
		"nothing is ready", nil, false)
	if err != nil {
		t.Fatalf("PostRecommendations: %v", err)
	}
	if res.Created || res.Updated || res.Unchanged {
		t.Fatalf("want a silent no-op result, got %+v", res)
	}
	if srv.createCalls != 0 {
		t.Fatalf("createCalls = %d, want 0 — worthOpening=false must not create", srv.createCalls)
	}
}

func TestPostRecommendations_UnchangedLiveBodySkipsWrite(t *testing.T) {
	org, repo := "testorg", "testrepo"
	srv := newRecServer(t, org, repo)
	// The baseline is the body currently on the issue — a fresh process
	// (this client has no memory of writing it) must still skip. This is the
	// restart-proofing the PR design leans on.
	srv.existing = []map[string]any{
		{"number": 42, "title": recTestTitle, "body": "steady state\n"},
	}
	c := newTestClient(t, srv.Server, org, []string{repo})

	res, err := c.PostRecommendations(context.Background(), repo, recTestTitle,
		"steady state", nil, true)
	if err != nil {
		t.Fatalf("PostRecommendations: %v", err)
	}
	if !res.Unchanged || res.Number != 42 {
		t.Fatalf("want Unchanged number 42, got %+v", res)
	}
	if srv.editCalls != 0 || srv.createCalls != 0 {
		t.Fatalf("unchanged body still wrote: edits=%d creates=%d", srv.editCalls, srv.createCalls)
	}
}

func TestPostRecommendations_ChangedBodyEditsInPlaceWithoutLabels(t *testing.T) {
	org, repo := "testorg", "testrepo"
	srv := newRecServer(t, org, repo)
	srv.existing = []map[string]any{
		{"number": 42, "title": recTestTitle, "body": "old content"},
	}
	c := newTestClient(t, srv.Server, org, []string{repo})

	res, err := c.PostRecommendations(context.Background(), repo, recTestTitle,
		"new content", []string{"hive"}, true)
	if err != nil {
		t.Fatalf("PostRecommendations: %v", err)
	}
	if !res.Updated || res.Number != 42 {
		t.Fatalf("want Updated number 42, got %+v", res)
	}
	if srv.createCalls != 0 {
		t.Fatalf("an existing issue must be edited, not duplicated: createCalls=%d", srv.createCalls)
	}
	if srv.editCalls != 1 {
		t.Fatalf("editCalls = %d, want 1", srv.editCalls)
	}
	// Labels are applied on creation only — re-sending them on an edit would
	// resurrect a label the maintainer deliberately removed.
	if _, sent := srv.editBodies[0]["labels"]; sent {
		t.Fatalf("edit payload carried labels: %v", srv.editBodies[0])
	}
}

func TestPostRecommendations_LookupFailureNeverFallsThroughToCreate(t *testing.T) {
	org, repo := "testorg", "testrepo"
	srv := newRecServer(t, org, repo)
	srv.listStatus = http.StatusInternalServerError
	c := newTestClient(t, srv.Server, org, []string{repo})

	_, err := c.PostRecommendations(context.Background(), repo, recTestTitle,
		"body", nil, true)
	if err == nil {
		t.Fatal("want an error when the lookup fails")
	}
	// A missed cycle costs nothing; a duplicate recommendations issue costs
	// the maintainer's trust in the digest.
	if srv.createCalls != 0 {
		t.Fatalf("lookup failure fell through to create: createCalls=%d", srv.createCalls)
	}
}

func TestPostRecommendations_RawMentionsNeverReachTheWire(t *testing.T) {
	org, repo := "testorg", "testrepo"
	srv := newRecServer(t, org, repo)
	c := newTestClient(t, srv.Server, org, []string{repo})

	_, err := c.PostRecommendations(context.Background(), repo, recTestTitle,
		"#12 fix things by @alice — ready", nil, true)
	if err != nil {
		t.Fatalf("PostRecommendations: %v", err)
	}
	body, _ := srv.createBodies[0]["body"].(string)
	// The issue body is rewritten on a cadence; one raw mention would
	// re-notify that person on every refresh.
	if strings.Contains(body, "@alice") {
		t.Fatalf("raw @mention reached the forge: %q", body)
	}
	if !strings.Contains(body, "alice") {
		t.Fatalf("neutralization must keep the name greppable, got: %q", body)
	}
}

func TestPostRecommendations_EmptyTitleRefused(t *testing.T) {
	org, repo := "testorg", "testrepo"
	srv := newRecServer(t, org, repo)
	c := newTestClient(t, srv.Server, org, []string{repo})

	if _, err := c.PostRecommendations(context.Background(), repo, "  ", "body", nil, true); err == nil {
		t.Fatal("want an error for an empty title — it is the issue's identity key")
	}
	if srv.createCalls != 0 {
		t.Fatalf("empty title still created: createCalls=%d", srv.createCalls)
	}
}

func TestPostRecommendations_NilClient(t *testing.T) {
	var c *Client
	_, err := c.PostRecommendations(context.Background(), "r", recTestTitle, "b", nil, true)
	if !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("want ErrNoGitHubClient, got %v", err)
	}
}
