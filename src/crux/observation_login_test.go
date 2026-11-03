package crux

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func pointGitHubUserAt(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	origURL, origClient := gitHubUserURL, gitHubUserClient
	gitHubUserURL, gitHubUserClient = srv.URL+"/user", srv.Client()
	t.Cleanup(func() {
		gitHubUserURL, gitHubUserClient = origURL, origClient
		srv.Close()
	})
	return srv
}

func TestGitHubTokenLogin_ResolvesLogin(t *testing.T) {
	var gotAuth, gotAccept string
	pointGitHubUserAt(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{"login":"  octocat \n"}`))
	})
	if got := GitHubTokenLogin("tok-123"); got != "octocat" {
		t.Fatalf("GitHubTokenLogin = %q, want octocat", got)
	}
	if gotAuth != "Bearer tok-123" {
		t.Fatalf("Authorization = %q, want Bearer tok-123", gotAuth)
	}
	if gotAccept != "application/vnd.github+json" {
		t.Fatalf("Accept = %q", gotAccept)
	}
}

func TestGitHubTokenLogin_NonOKStatusIsEmpty(t *testing.T) {
	pointGitHubUserAt(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	})
	if got := GitHubTokenLogin("bad"); got != "" {
		t.Fatalf("GitHubTokenLogin on 401 = %q, want empty", got)
	}
}

func TestGitHubTokenLogin_MalformedBodyIsEmpty(t *testing.T) {
	pointGitHubUserAt(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":`))
	})
	if got := GitHubTokenLogin("tok"); got != "" {
		t.Fatalf("GitHubTokenLogin on malformed JSON = %q, want empty", got)
	}
}

func TestGitHubTokenLogin_TransportErrorIsEmpty(t *testing.T) {
	srv := pointGitHubUserAt(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	})
	srv.Close()
	if got := GitHubTokenLogin("tok"); got != "" {
		t.Fatalf("GitHubTokenLogin against a closed server = %q, want empty", got)
	}
}

func TestGitHubTokenLogin_BadURLIsEmpty(t *testing.T) {
	orig := gitHubUserURL
	t.Cleanup(func() { gitHubUserURL = orig })
	gitHubUserURL = "://not a url"
	if got := GitHubTokenLogin("tok"); got != "" {
		t.Fatalf("GitHubTokenLogin with an unparsable URL = %q, want empty", got)
	}
}
