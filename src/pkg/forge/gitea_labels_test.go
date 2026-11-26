package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// echoAppliedLabels answers an add-labels body by listing every requested
// label as applied, as Gitea and Forgejo do for labels the repo has.
func echoAppliedLabels(body []byte) []byte {
	var req struct {
		Labels []any `json:"labels"`
	}
	_ = json.Unmarshal(body, &req)
	out := make([]map[string]any, 0, len(req.Labels))
	for _, l := range req.Labels {
		out = append(out, map[string]any{"name": fmt.Sprint(l)})
	}
	b, _ := json.Marshal(out)
	return b
}

// fakeLabelRepo is a Forgejo repo as the add-labels call sees it: the call
// applies only labels the repo has (an exact name, or an id) and answers 200
// with the issue's labels, silently dropping a name the repo lacks.
type fakeLabelRepo struct {
	mu sync.Mutex
	// repoLabels maps a repo label id to its name.
	repoLabels map[int64]string
	// issue is the set of label names on the issue.
	issue []string
	// created records every create-label body.
	created []map[string]string
	// createStatus, when set, refuses label creation with that status.
	createStatus int
	// ignoreIDs drops labels sent by id too.
	ignoreIDs bool
	// adds counts add-labels calls.
	adds int
}

func (f *fakeLabelRepo) server(t *testing.T) *httptest.Server {
	t.Helper()
	const repo = "/api/v1/repos/acme/widgets"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == repo+"/issues/7/labels":
			f.adds++
			var req struct {
				Labels []any `json:"labels"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("add-labels body %q: %v", body, err)
			}
			for _, l := range req.Labels {
				switch v := l.(type) {
				case string:
					for _, name := range f.repoLabels {
						if name == v {
							f.issue = append(f.issue, name)
						}
					}
				case float64:
					if name, ok := f.repoLabels[int64(v)]; ok && !f.ignoreIDs {
						f.issue = append(f.issue, name)
					}
				}
			}
			out := make([]map[string]any, 0, len(f.issue))
			for _, name := range f.issue {
				out = append(out, map[string]any{"name": name})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && r.URL.Path == repo+"/labels":
			out := []map[string]any{}
			if r.URL.Query().Get("page") == "1" {
				for id, name := range f.repoLabels {
					out = append(out, map[string]any{"id": id, "name": name})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == repo+"/labels":
			if f.createStatus != 0 {
				w.WriteHeader(f.createStatus)
				_, _ = w.Write([]byte(`{"message":"refused"}`))
				return
			}
			var req map[string]string
			_ = json.Unmarshal(body, &req)
			f.created = append(f.created, req)
			id := int64(100 + len(f.repoLabels))
			f.repoLabels[id] = req["name"]
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": req["name"]})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newLabelTestForge(t *testing.T, f *fakeLabelRepo) *giteaForge {
	t.Helper()
	g, err := newGiteaFamilyForge(KindForgejo, "fj", Options{BaseURL: f.server(t).URL, Org: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// TestGiteaAddLabelsCreatesMissingGateLabel is #19910: the repo has no
// ready-for-review label, so the first call applies nothing. The adapter
// must create the label with the gate's colour and description and apply
// it, not report success with nothing applied.
func TestGiteaAddLabelsCreatesMissingGateLabel(t *testing.T) {
	repo := &fakeLabelRepo{repoLabels: map[int64]string{1: "bug"}}
	f := newLabelTestForge(t, repo)

	if err := f.AddLabels(context.Background(), "widgets", 7, []string{github.DefaultReadyForReviewLabel, "bug"}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if len(repo.created) != 1 {
		t.Fatalf("created = %v, want only the missing gate label", repo.created)
	}
	got := repo.created[0]
	if got["name"] != github.DefaultReadyForReviewLabel || got["color"] != "#0e8a16" || got["description"] != github.ReadyForReviewLabelDescription {
		t.Errorf("created label = %v, want the gate label's colour and description", got)
	}
	if strings.Join(repo.issue, ",") != "bug,"+github.DefaultReadyForReviewLabel {
		t.Errorf("issue labels = %v, want bug and the gate label", repo.issue)
	}
}

func TestGiteaAddLabelsCreatesOtherLabelWithDefaultColour(t *testing.T) {
	repo := &fakeLabelRepo{repoLabels: map[int64]string{}}
	f := newLabelTestForge(t, repo)
	if err := f.AddLabels(context.Background(), "widgets", 7, []string{"agent/scanner"}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if len(repo.created) != 1 || repo.created[0]["color"] != "#"+giteaDefaultLabelColor || repo.created[0]["description"] != "" {
		t.Errorf("created = %v, want agent/scanner with the default colour", repo.created)
	}
	if len(repo.issue) != 1 || repo.issue[0] != "agent/scanner" {
		t.Errorf("issue labels = %v", repo.issue)
	}
}

// A repo label that differs only in case is applied by id, never duplicated.
func TestGiteaAddLabelsUsesExistingLabelOfOtherCase(t *testing.T) {
	repo := &fakeLabelRepo{repoLabels: map[int64]string{5: "Bug"}}
	f := newLabelTestForge(t, repo)
	if err := f.AddLabels(context.Background(), "widgets", 7, []string{"bug"}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if len(repo.created) != 0 {
		t.Errorf("created = %v, want none: the repo has Bug", repo.created)
	}
	if len(repo.issue) != 1 || repo.issue[0] != "Bug" {
		t.Errorf("issue labels = %v, want Bug", repo.issue)
	}
}

// The labels the repo has are applied in one call, with no lookup.
func TestGiteaAddLabelsAllPresentIsOneCall(t *testing.T) {
	repo := &fakeLabelRepo{repoLabels: map[int64]string{1: "bug", 2: "hold"}}
	f := newLabelTestForge(t, repo)
	if err := f.AddLabels(context.Background(), "widgets", 7, []string{"bug", " hold ", "", "BUG"}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if repo.adds != 1 || len(repo.created) != 0 {
		t.Errorf("adds = %d created = %v, want one call and no create", repo.adds, repo.created)
	}
	if err := f.AddLabels(context.Background(), "widgets", 7, []string{" ", ""}); err != nil || repo.adds != 1 {
		t.Errorf("blank labels: err = %v adds = %d, want a no-op", err, repo.adds)
	}
}

// A label that still is not on the issue after the create is an error that
// names it.
func TestGiteaAddLabelsReportsLabelStillMissing(t *testing.T) {
	repo := &fakeLabelRepo{repoLabels: map[int64]string{}, ignoreIDs: true}
	f := newLabelTestForge(t, repo)
	err := f.AddLabels(context.Background(), "widgets", 7, []string{github.DefaultReadyForReviewLabel})
	if !errors.Is(err, ErrLabelsMissing) || !strings.Contains(err.Error(), github.DefaultReadyForReviewLabel) {
		t.Fatalf("err = %v, want ErrLabelsMissing naming the label", err)
	}
}

// A token that may not create labels: the error names the label and keeps
// the forge's status, so the relay classifies it as it does any forge error
// (a 4xx is final, a 5xx or 429 is retried).
func TestGiteaAddLabelsCreateRefused(t *testing.T) {
	repo := &fakeLabelRepo{repoLabels: map[int64]string{}, createStatus: http.StatusForbidden}
	f := newLabelTestForge(t, repo)
	err := f.AddLabels(context.Background(), "widgets", 7, []string{github.DefaultReadyForReviewLabel})
	if err == nil || errors.Is(err, ErrLabelsMissing) || !isGiteaStatus(err, http.StatusForbidden) || !strings.Contains(err.Error(), github.DefaultReadyForReviewLabel) {
		t.Fatalf("err = %v, want the 403 with its status (not ErrLabelsMissing) naming the label", err)
	}
}

// TestForgejoCreateChangeRequestNotHeldIsReported is the relay's view of
// #19910: when the gate label cannot be applied, CreateChangeRequest must
// return ErrLabelsNotApplied with the opened PR, not a clean open.
func TestForgejoCreateChangeRequestNotHeldIsReported(t *testing.T) {
	const repo = "/api/v1/repos/acme/widgets"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == repo+"/pulls":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == repo+"/pulls":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number": 4, "html_url": "u4"}`))
		case r.Method == http.MethodPost && r.URL.Path == repo+"/issues/4/labels":
			_, _ = w.Write([]byte(`[]`)) // Forgejo 17: 200, nothing applied
		case r.Method == http.MethodGet && r.URL.Path == repo+"/labels":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == repo+"/labels":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"forbidden"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	f, err := NewForge(KindForgejo, "fj", Options{BaseURL: srv.URL, Org: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.(ChangeRequestCreator).CreateChangeRequest(context.Background(), "widgets", ChangeRequestSpec{
		Head: "fix-1", Base: "main", Title: "t", Body: "b", Labels: []string{github.DefaultReadyForReviewLabel},
	})
	if !errors.Is(err, ErrLabelsNotApplied) || !isGiteaStatus(err, http.StatusForbidden) {
		t.Fatalf("err = %v, want ErrLabelsNotApplied wrapping the refused create", err)
	}
	if ref.Number != 4 {
		t.Errorf("ref = %+v, want the opened PR", ref)
	}
}

func TestIsGiteaStatus(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &giteaStatusError{label: "forgejo", endpoint: "/x", status: http.StatusForbidden, body: "no"})
	if !isGiteaStatus(err, http.StatusForbidden) || isGiteaStatus(err, http.StatusNotFound) || isGiteaStatus(errors.New("x"), http.StatusForbidden) {
		t.Error("isGiteaStatus misreads the status")
	}
	if got := err.Error(); got != "wrapped: forgejo API /x returned status 403: no" {
		t.Errorf("text = %q, want the adapter's usual status text", got)
	}
}

// A 5xx while listing labels keeps its status, so the relay retries it.
func TestGiteaAddLabelsLookupFailureKeepsStatus(t *testing.T) {
	const repo = "/api/v1/repos/acme/widgets"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == repo+"/issues/7/labels" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	f, err := newGiteaFamilyForge(KindForgejo, "fj", Options{BaseURL: srv.URL, Org: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	err = f.AddLabels(context.Background(), "widgets", 7, []string{"bug"})
	if !isGiteaStatus(err, http.StatusBadGateway) || errors.Is(err, ErrLabelsMissing) {
		t.Fatalf("err = %v, want the 502 with its status, not ErrLabelsMissing", err)
	}
}
