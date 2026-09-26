package worksource

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/forge"
)

func TestGiteaSourceListFiltersAndMutates(t *testing.T) {
	var seenAuth string
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/repo/issues":
			if r.URL.Query().Get("state") != "open" || r.URL.Query().Get("type") != "issues" {
				t.Fatalf("bad list query: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 1, "title": "take me", "state": "open", "labels": []map[string]string{{"name": "bug"}, {"name": "ready"}}, "user": map[string]string{"login": "ada"}, "assignees": []map[string]string{{"login": "bot"}}, "html_url": "https://gitea/acme/repo/issues/1"},
				{"number": 2, "title": "held", "state": "open", "labels": []map[string]string{{"name": "hold"}}, "user": map[string]string{"login": "ada"}},
				{"number": 3, "title": "wrong label", "state": "open", "labels": []map[string]string{{"name": "docs"}}, "user": map[string]string{"login": "ada"}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/repo/issues/1/labels":
			assertJSONField(t, r, "labels", []any{"design"})
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/repo/labels":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 44, "name": "design"}})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/repos/acme/repo/issues/1/labels/44":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/repo/issues/1/comments":
			assertJSONField(t, r, "body", "hello")
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/repos/acme/repo/issues/1":
			assertJSONField(t, r, "state", "closed")
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()

	ws, err := newGiteaSource(forgeWorkSourceConfig{
		BaseURL: srv.URL,
		Token:   "tok",
		Repos:   []forgeRepoConfig{{SourceRepo: "acme/repo", WorkRepo: "work/repo"}},
		Labels:  []string{"bug"},
	})
	if err != nil {
		t.Fatalf("newGiteaSource: %v", err)
	}
	items, err := ws.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(items) != 1 || items[0].ExternalID != "acme/repo#1" || items[0].Number != 0 || items[0].Repo != "work/repo" || TaskKey(items[0]) != "work/repo!acme/repo#1" {
		t.Fatalf("items = %+v", items)
	}
	if seenAuth != "token tok" {
		t.Fatalf("auth header = %q", seenAuth)
	}
	ref := RefFromIssue(items[0])
	if err := ws.(LabelMutator).AddLabel(context.Background(), ref, "design"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	if err := ws.(LabelMutator).RemoveLabel(context.Background(), ref, "design"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if err := ws.(Commenter).AddComment(context.Background(), ref, "hello"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if err := ws.(StatusTransitioner).TransitionStatus(context.Background(), ref, "closed"); err != nil {
		t.Fatalf("TransitionStatus: %v", err)
	}
	if len(calls) < 5 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestGitLabSourceListEscapesAndMutates(t *testing.T) {
	var seenToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenToken = r.Header.Get("PRIVATE-TOKEN")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.RequestURI, "/api/v4/projects/acme%2Fsub%2Frepo/issues"):
			if r.URL.Query().Get("state") != "opened" {
				t.Fatalf("bad state query: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"iid": 7, "title": "gitlab", "state": "opened", "labels": []string{"ready"}, "author": map[string]string{"username": "grace"}, "assignees": []map[string]string{{"username": "bot"}}, "web_url": "https://gitlab/acme/sub/repo/-/issues/7",
			}})
		case r.Method == http.MethodPost && strings.Contains(r.RequestURI, "/api/v4/projects/acme%2Fsub%2Frepo/issues/7/notes"):
			assertJSONField(t, r, "body", "note")
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && strings.Contains(r.RequestURI, "/api/v4/projects/acme%2Fsub%2Frepo/issues/7"):
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			switch {
			case body["add_labels"] == "design", body["remove_labels"] == "design", body["state_event"] == "reopen":
				w.WriteHeader(http.StatusOK)
			default:
				t.Fatalf("unexpected gitlab body: %#v", body)
			}
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.RequestURI)
		}
	}))
	defer srv.Close()

	ws, err := newGitLabSource(forgeWorkSourceConfig{
		BaseURL:  srv.URL,
		Token:    "gl",
		Repos:    []forgeRepoConfig{{SourceRepo: "acme/sub/repo"}},
		States:   []string{"opened"},
		Assignee: "bot",
	})
	if err != nil {
		t.Fatalf("newGitLabSource: %v", err)
	}
	items, err := ws.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(items) != 1 || items[0].ExternalID != "acme/sub/repo#7" || items[0].Number != 0 || TaskKey(items[0]) != "acme/sub/repo!acme/sub/repo#7" {
		t.Fatalf("items = %+v", items)
	}
	if seenToken != "gl" {
		t.Fatalf("PRIVATE-TOKEN = %q", seenToken)
	}
	ref := RefFromIssue(items[0])
	if err := ws.(LabelMutator).AddLabel(context.Background(), ref, "design"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	if err := ws.(LabelMutator).RemoveLabel(context.Background(), ref, "design"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if err := ws.(Commenter).AddComment(context.Background(), ref, "note"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if err := ws.(StatusTransitioner).TransitionStatus(context.Background(), ref, "open"); err != nil {
		t.Fatalf("TransitionStatus: %v", err)
	}
}

func TestForgeSourceErrorsAndUnsupportedTransition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ws, err := newGitLabSource(forgeWorkSourceConfig{BaseURL: srv.URL, Repos: []forgeRepoConfig{{SourceRepo: "acme/repo"}}})
	if err != nil {
		t.Fatalf("newGitLabSource: %v", err)
	}
	if _, err := ws.ListIssues(context.Background()); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("ListIssues error = %v, want 429", err)
	}
	err = ws.(StatusTransitioner).TransitionStatus(context.Background(), Ref{Repo: "acme/repo", ExternalID: "1"}, "wontfix")
	if !errors.Is(err, ErrStatusTransitionUnsupported) {
		t.Fatalf("TransitionStatus error = %v, want unsupported", err)
	}
	if _, err := newGiteaSource(forgeWorkSourceConfig{Repos: []forgeRepoConfig{{SourceRepo: "acme/repo"}}}); err == nil {
		t.Fatal("newGiteaSource without base_url succeeded")
	}
	if _, err := newGitLabSource(forgeWorkSourceConfig{BaseURL: srv.URL}); err == nil {
		t.Fatal("newGitLabSource without repos succeeded")
	}
}

func TestForgeSourceExternalIDDisambiguatesSharedWorkRepo(t *testing.T) {
	var commented string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v4/projects/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"iid": 1, "title": "issue", "state": "opened", "labels": []string{"ready"}, "web_url": "https://gitlab/issue",
			}})
		case r.Method == http.MethodPost && strings.Contains(r.RequestURI, "/api/v4/projects/b%2Frepo/issues/1/notes"):
			commented = "b/repo"
			w.WriteHeader(http.StatusCreated)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.RequestURI)
		}
	}))
	defer srv.Close()

	ws, err := newGitLabSource(forgeWorkSourceConfig{
		BaseURL: srv.URL,
		Repos: []forgeRepoConfig{
			{SourceRepo: "a/repo", WorkRepo: "work/repo"},
			{SourceRepo: "b/repo", WorkRepo: "work/repo"},
		},
	})
	if err != nil {
		t.Fatalf("newGitLabSource: %v", err)
	}
	items, err := ws.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	if TaskKey(items[0]) == TaskKey(items[1]) {
		t.Fatalf("duplicate keys for shared work repo: %+v", items)
	}
	second := RefFromIssue(items[1])
	if err := ws.(Commenter).AddComment(context.Background(), second, "note"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if commented != "b/repo" {
		t.Fatalf("commented repo = %q, want b/repo", commented)
	}
}

func TestForgeSourceNativeRefErrors(t *testing.T) {
	src := &forgeWorkSource{cfg: forgeWorkSourceConfig{Kind: forge.KindGitLab}}
	if _, _, err := src.nativeRef(Ref{Repo: "acme/repo", ExternalID: "not-a-number"}); err == nil {
		t.Fatal("nativeRef accepted non-numeric external id")
	}
	if _, _, err := src.nativeRef(Ref{ExternalID: "1"}); err == nil {
		t.Fatal("nativeRef accepted empty repo")
	}
	if err := src.AddLabel(context.Background(), Ref{Repo: "acme/repo", ExternalID: "1"}, "x"); err == nil {
		t.Fatal("AddLabel without client succeeded")
	}
	if got := forgeExternalID("", 4); got != "4" {
		t.Fatalf("forgeExternalID without repo = %q", got)
	}
}

func TestForgeSourceFilterAndTransitionBranches(t *testing.T) {
	src := &forgeWorkSource{cfg: forgeWorkSourceConfig{
		Kind:       forge.KindGitea,
		States:     []string{"open"},
		Labels:     []string{"ready", "bug"},
		Assignee:   "hive",
		HoldLabels: []string{"blocked"},
	}}
	if src.includeIssue(forge.Issue{State: "closed", Labels: []string{"ready", "bug"}, Assignees: []string{"hive"}}) {
		t.Fatal("closed issue passed state filter")
	}
	if src.includeIssue(forge.Issue{State: "open", Labels: []string{"ready"}, Assignees: []string{"hive"}}) {
		t.Fatal("issue missing required label passed")
	}
	if src.includeIssue(forge.Issue{State: "open", Labels: []string{"ready", "bug", "blocked"}, Assignees: []string{"hive"}}) {
		t.Fatal("held issue passed")
	}
	if src.includeIssue(forge.Issue{State: "open", Labels: []string{"ready", "bug"}, Assignees: []string{"other"}}) {
		t.Fatal("wrong assignee passed")
	}
	if !src.includeIssue(forge.Issue{State: "open", Labels: []string{"ready", "bug"}, Assignees: []string{"hive"}}) {
		t.Fatal("matching issue did not pass")
	}
	if got, ok := src.nativeTransition("reopen"); !ok || got != "open" {
		t.Fatalf("gitea reopen transition = %q/%t", got, ok)
	}
	src.cfg.Kind = forge.KindGitLab
	if got, ok := src.nativeTransition("done"); !ok || got != "close" {
		t.Fatalf("gitlab done transition = %q/%t", got, ok)
	}
}

func assertJSONField(t *testing.T, r *http.Request, key string, want any) {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body[key]; !equalJSONValue(got, want) {
		t.Fatalf("body[%s] = %#v, want %#v", key, got, want)
	}
}

func equalJSONValue(got, want any) bool {
	switch w := want.(type) {
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	default:
		return got == want
	}
}
