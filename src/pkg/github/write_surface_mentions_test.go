package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// neutralizeOnly returns a mention predicate covering only the listed lanes,
// the shape config.WriteSurfaceNeutralizesMentions has for a hive that lists
// them under write_surface.neutralize_mentions.
func neutralizeOnly(lanes ...string) func(string) bool {
	return func(agent string) bool {
		for _, l := range lanes {
			if l == agent {
				return true
			}
		}
		return false
	}
}

func TestRelayBody_DefaultPostsBodyAsWritten(t *testing.T) {
	var nilClient *Client
	if got := nilClient.relayBody("scanner", "cc @alice"); got != "cc @alice" {
		t.Errorf("nil client rewrote the body: %q", got)
	}
	c := testClient(t, "http://127.0.0.1:1")
	if got := c.relayBody("scanner", "cc @alice"); got != "cc @alice" {
		t.Errorf("no predicate configured, but the body was rewritten: %q", got)
	}
}

func TestRelayBody_ListedLaneOnly(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")
	c.SetMentionNeutralizeFunc(neutralizeOnly("scanner"))

	got := c.relayBody("scanner", "cc @alice and @bob-2, mail ops@example.com")
	if strings.Contains(got, "@alice") || strings.Contains(got, "@bob-2") {
		t.Errorf("listed lane kept a live mention: %q", got)
	}
	if !strings.Contains(got, "`alice`") || !strings.Contains(got, "`bob-2`") {
		t.Errorf("mention was not rewritten to a code span: %q", got)
	}
	if !strings.Contains(got, "ops@example.com") {
		t.Errorf("an email address was rewritten: %q", got)
	}

	if got := c.relayBody("reviewer", "cc @alice"); got != "cc @alice" {
		t.Errorf("an unlisted lane's body was rewritten: %q", got)
	}
	if got := c.relayBody("", "cc @alice"); got != "cc @alice" {
		t.Errorf("an unnamed agent's body was rewritten: %q", got)
	}

	c.SetMentionNeutralizeFunc(nil)
	if got := c.relayBody("scanner", "cc @alice"); got != "cc @alice" {
		t.Errorf("clearing the predicate must restore bodies as written: %q", got)
	}
}

// The comment relay posts the neutralized body for a listed lane and the
// original body for every other lane.
func TestIssueRequestWatcher_CommentNeutralizesMentionsForListedLane(t *testing.T) {
	var mu sync.Mutex
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments") {
			var payload struct {
				Body string `json:"body"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &payload)
			mu.Lock()
			posted = append(posted, payload.Body)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":1}`)
			return
		}
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/issues") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetMentionNeutralizeFunc(neutralizeOnly("scanner"))
	dir := withIssueDir(t)

	if _, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 41, Body: "ping @alice about this", Agent: "scanner",
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if _, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 42, Body: "ping @alice about this", Agent: "reviewer",
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 2 {
		t.Fatalf("expected 2 comments, got %d: %q", len(posted), posted)
	}
	if strings.Contains(posted[0], "@alice") || !strings.Contains(posted[0], "`alice`") {
		t.Errorf("listed lane's comment kept a live mention: %q", posted[0])
	}
	if !strings.Contains(posted[1], "@alice") {
		t.Errorf("unlisted lane's comment was rewritten: %q", posted[1])
	}
}
