package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hub/marketplace/curated"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// curatedTechdocs compiles the curated cncf-techdocs-assessment model a
// hive binds cncf/techdocs to (curated-2026.11.2).
func curatedTechdocs(t *testing.T) *workflowmodel.Profile {
	t.Helper()
	entries, errs := curated.Load()
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, e := range entries {
		if e.Model.Metadata.Name == "cncf-techdocs-assessment" && e.Model.Spec.Meta.Version == "curated-2026.11.2" {
			p, _, err := workflowmodel.Compile(e.Model)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
	t.Fatal("curated techdocs model missing")
	return nil
}

func techdocsDispatchClient(t *testing.T) *Client {
	t.Helper()
	p := curatedTechdocs(t)
	c := &Client{org: "cncf"}
	c.SetStageWorkflowFor(func(owner, repo string) *workflowmodel.Profile {
		if owner == "cncf" && repo == "techdocs" {
			return p
		}
		return nil
	})
	return c
}

func TestWorkflowDispatchForIssue(t *testing.T) {
	c := techdocsDispatchClient(t)
	accepted := Issue{Repo: "cncf/techdocs", Number: 395, Title: "Assess X", Labels: []string{"Docs analysis", "phase/assessment", "triage/accepted"}}
	d, ok := c.WorkflowDispatchForIssue(accepted)
	if !ok || d.State != "accepted" || d.Target.Profile != "techdocs-drafter" || d.Kind != workflowmodel.KindIssue {
		t.Fatalf("accepted issue dispatch = %+v %v", d, ok)
	}
	if d.Ref() != "cncf/techdocs#395" || d.Model != "cncf-techdocs-assessment@curated-2026.11.2" {
		t.Fatalf("ref %q model %q", d.Ref(), d.Model)
	}
	// Not yet accepted: the requested state is a person's, not a profile's.
	if _, ok := c.WorkflowDispatchForIssue(Issue{Repo: "cncf/techdocs", Number: 396, Labels: []string{"Docs analysis", "needs-triage"}}); ok {
		t.Fatal("a requested issue must not dispatch")
	}
	// On hold: escalated, an administrator's.
	if _, ok := c.WorkflowDispatchForIssue(Issue{Repo: "cncf/techdocs", Number: 397, Labels: []string{"triage/accepted", "hold"}}); ok {
		t.Fatal("a held issue must not dispatch")
	}
	// An unbound repository is untouched.
	if _, ok := c.WorkflowDispatchForIssue(Issue{Repo: "cncf/other", Number: 1, Labels: []string{"triage/accepted"}}); ok {
		t.Fatal("an unbound repository dispatched")
	}
	var nilClient *Client
	if _, ok := nilClient.WorkflowDispatchForIssue(accepted); ok {
		t.Fatal("nil client dispatched")
	}
}

func TestWorkflowDispatchForPR(t *testing.T) {
	c := techdocsDispatchClient(t)
	d, ok := c.WorkflowDispatchForPR(PullRequest{Repo: "cncf/techdocs", Number: 400, Draft: true, Labels: []string{"Docs analysis", "phase/assessment"}})
	if !ok || d.State != "drafting" || d.Target.Profile != "techdocs-verifier" || d.Target.Reply != "/verify" {
		t.Fatalf("draft PR dispatch = %+v %v", d, ok)
	}
	// Verified: in review, the reviewer's.
	if _, ok := c.WorkflowDispatchForPR(PullRequest{Repo: "cncf/techdocs", Number: 401, Draft: true, Labels: []string{"Docs analysis", "verified"}}); ok {
		t.Fatal("a verified PR must not dispatch")
	}
	// Not a deliverable: no model state.
	if _, ok := c.WorkflowDispatchForPR(PullRequest{Repo: "cncf/techdocs", Number: 402, Draft: true}); ok {
		t.Fatal("an unlabeled PR must not dispatch")
	}
}

func TestIsDispatchReply(t *testing.T) {
	cases := map[string]bool{
		"/verify":              true,
		"/VERIFY please":       true,
		"\n\n/verify.\nthanks": true,
		"/hive verify":         false,
		"> /verify":            false,
		"please /verify":       false,
		"/verifying":           false,
		"":                     false,
	}
	for body, want := range cases {
		if got := IsDispatchReply(body, "/verify"); got != want {
			t.Errorf("IsDispatchReply(%q) = %v; want %v", body, got, want)
		}
	}
}

// LatestDispatchReply takes the newest unedited /verify from a human with
// write access; a reader's /verify and a bot's are passed over.
func TestLatestDispatchReply(t *testing.T) {
	created := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	type wireComment struct {
		ID        int64     `json:"id"`
		Body      string    `json:"body"`
		User      wireUser  `json:"user"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
		HTMLURL   string    `json:"html_url"`
	}
	comments := []wireComment{
		{ID: 1, Body: "/verify", User: wireUser{Login: "writer"}, CreatedAt: created, UpdatedAt: created, HTMLURL: "u1"},
		{ID: 2, Body: "/verify", User: wireUser{Login: "reader"}, CreatedAt: created.Add(time.Minute), UpdatedAt: created.Add(time.Minute)},
		{ID: 3, Body: "/verify", User: wireUser{Login: "ci[bot]"}, CreatedAt: created.Add(2 * time.Minute), UpdatedAt: created.Add(2 * time.Minute)},
		{ID: 4, Body: "looks good", User: wireUser{Login: "writer"}, CreatedAt: created.Add(3 * time.Minute), UpdatedAt: created.Add(3 * time.Minute)},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/cncf/techdocs/issues/400/comments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(comments)
	})
	mux.HandleFunc("/repos/cncf/techdocs/collaborators/writer/permission", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"permission":"write","role_name":"write"}`))
	})
	mux.HandleFunc("/repos/cncf/techdocs/collaborators/reader/permission", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"permission":"read","role_name":"read"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := newTestClient(t, server, "cncf", []string{"techdocs"})
	got, ok, err := c.LatestDispatchReply(context.Background(), "cncf/techdocs", 400, "/verify")
	if err != nil || !ok {
		t.Fatalf("LatestDispatchReply = %+v %v %v", got, ok, err)
	}
	if got.CommentID != 1 || got.Author != "writer" || got.URL != "u1" {
		t.Fatalf("reply %+v; want comment 1 by writer", got)
	}
	if _, ok, _ := c.LatestDispatchReply(context.Background(), "cncf/techdocs", 400, ""); ok {
		t.Fatal("empty reply matched")
	}
}

// One item has one Ref whichever spelling of its repository the caller
// carries: the enumeration's bare name and the owner's qualified one must
// build the same dispatch (and so the same dispatch-log key).
func TestWorkflowDispatchRepoIsCanonical(t *testing.T) {
	c := techdocsDispatchClient(t)
	labels := []string{"Docs analysis", "phase/assessment", "triage/accepted"}
	bare, ok1 := c.WorkflowDispatchForIssue(Issue{Repo: "techdocs", Number: 395, Labels: labels})
	qual, ok2 := c.WorkflowDispatchForIssue(Issue{Repo: "cncf/techdocs", Number: 395, Labels: labels})
	if !ok1 || !ok2 {
		t.Fatalf("dispatch ok = %v %v", ok1, ok2)
	}
	if bare.Ref() != "cncf/techdocs#395" || bare.Ref() != qual.Ref() || bare.Repo != "cncf/techdocs" {
		t.Fatalf("refs differ: bare %q qualified %q", bare.Ref(), qual.Ref())
	}
	pr, ok := c.WorkflowDispatchForPR(PullRequest{Repo: "techdocs", Number: 400, Draft: true, Labels: []string{"Docs analysis", "phase/assessment"}})
	if !ok || pr.Ref() != "cncf/techdocs#400" {
		t.Fatalf("bare PR ref = %q ok=%v", pr.Ref(), ok)
	}
}
