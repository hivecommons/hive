package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewForgeGiteaFamily(t *testing.T) {
	for _, kind := range []Kind{KindGitea, KindForgejo} {
		t.Run(string(kind), func(t *testing.T) {
			f, err := NewForge(kind, "token", Options{BaseURL: "https://git.example.com", Org: "acme"})
			if err != nil {
				t.Fatalf("NewForge(%q): %v", kind, err)
			}
			if f.Kind() != kind {
				t.Errorf("Kind() = %q, want %q", f.Kind(), kind)
			}
			if _, ok := f.(Merger); !ok {
				t.Errorf("%q adapter does not implement Merger", kind)
			}
			_, err = NewForge(kind, "token", Options{Org: "acme"})
			if err == nil || !strings.HasPrefix(err.Error(), string(kind)+": BaseURL is required") {
				t.Errorf("missing BaseURL error = %v, want a %q-prefixed BaseURL error", err, kind)
			}
		})
	}
}

func TestForgejoAdapterTalksToServer(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotMethod, gotPath = r.Header.Get("Authorization"), r.Method, r.URL.Path
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/hivecommons/hive":
			_, _ = w.Write([]byte(giteaRepoJSON))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/hivecommons/hive/issues":
			_, _ = w.Write([]byte(giteaIssuesJSON))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
		}
	}))
	defer srv.Close()

	f, err := NewForge(KindForgejo, "fj-token", Options{BaseURL: srv.URL, Org: "hivecommons"})
	if err != nil {
		t.Fatalf("NewForge: %v", err)
	}
	ctx := context.Background()

	repo, err := f.GetRepo(ctx, "hive")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if repo.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want main", repo.DefaultBranch)
	}
	if gotAuth != "token fj-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "token fj-token")
	}

	issues, err := f.ListOpenIssues(ctx, "hive")
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Errorf("issues = %d, want 2 (the PR stub filtered out)", len(issues))
	}

	if err := f.RemoveLabel(ctx, "hive", 7, "hold"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if gotMethod != http.MethodDelete || !strings.HasSuffix(gotPath, "/repos/hivecommons/hive/issues/7/labels/hold") {
		t.Errorf("RemoveLabel sent %s %s, want DELETE .../issues/7/labels/hold (label by name)", gotMethod, gotPath)
	}

	err = f.AddLabels(ctx, "hive", 7, []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "forgejo API") {
		t.Errorf("AddLabels error = %v, want a forgejo-prefixed error", err)
	}
}
