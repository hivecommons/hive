package integrated

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

func TestEnrichRemoteInspectionUsesGitHubCanonicalRepositorySpelling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/daviddiaz0317/visual-hive-demo-site":
			_, _ = io.WriteString(writer, `{"id":1287566668,"full_name":"DavidDiaz0317/visual-hive-demo-site","default_branch":"main","permissions":{"push":true}}`)
		case "/repos/daviddiaz0317/visual-hive-demo-site/branches/main/protection":
			http.NotFound(writer, request)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := hivegithub.NewClient("token", "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), server.URL)
	inspection := RepositoryInspection{Permissions: map[string]bool{}}
	got := enrichRemoteInspection(context.Background(), client, "daviddiaz0317/visual-hive-demo-site", &inspection)

	if want := "DavidDiaz0317/visual-hive-demo-site"; got != want {
		t.Fatalf("canonical repository = %q, want %q", got, want)
	}
	if inspection.RepositoryID != "1287566668" {
		t.Fatalf("repository ID = %q, want 1287566668", inspection.RepositoryID)
	}
	if inspection.DefaultBranch != "main" {
		t.Fatalf("default branch = %q, want main", inspection.DefaultBranch)
	}
	if !inspection.Permissions["push"] {
		t.Fatal("expected push permission from canonical repository metadata")
	}
}

func TestEnrichRemoteInspectionRejectsDifferentRepositoryIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":456,"full_name":"other/repository","default_branch":"main"}`)
		case "/repos/owner/repo/branches/main/protection":
			http.NotFound(writer, request)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := hivegithub.NewClient("token", "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), server.URL)
	inspection := RepositoryInspection{Permissions: map[string]bool{}}
	got := enrichRemoteInspection(context.Background(), client, "owner/repo", &inspection)

	if got != "owner/repo" {
		t.Fatalf("different repository identity must not replace the requested repository: got %q", got)
	}
}
