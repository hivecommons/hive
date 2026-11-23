package forge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGitLabUserProfileReadsPublicFields(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get(gitLabTokenHeader) != "glpat" {
			t.Errorf("token header = %q", r.Header.Get(gitLabTokenHeader))
		}
		switch r.URL.Path {
		case "/api/v4/users":
			if r.URL.Query().Get("username") != "ada" {
				t.Errorf("username query = %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`[{"id": 42, "username": "ada"}]`))
		case "/api/v4/users/42":
			_, _ = w.Write([]byte(`{"username":"ada","name":"Ada Lovelace","organization":"Analytical Engines","location":"London, UK","website_url":"https://ada.example","avatar_url":"https://gitlab.example/a.png","bio":"never read"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	f, err := NewForge(KindGitLab, "glpat", Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	up, ok := f.(UserProfiler)
	if !ok {
		t.Fatal("gitlab adapter does not implement UserProfiler")
	}
	got, err := up.UserProfile(context.Background(), "@ada")
	if err != nil {
		t.Fatal(err)
	}
	want := UserProfile{Login: "ada", Name: "Ada Lovelace", Company: "Analytical Engines", Location: "London, UK",
		Website: "https://ada.example", AvatarURL: "https://gitlab.example/a.png"}
	if got != want {
		t.Fatalf("profile = %+v, want %+v", got, want)
	}
	if n := calls.Load(); n != UserProfileMaxRequests {
		t.Fatalf("requests = %d, want %d", n, UserProfileMaxRequests)
	}
}

func TestGitLabUserProfileUnknownUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	f, _ := newGitLabForge("", Options{BaseURL: srv.URL})
	if _, err := f.UserProfile(context.Background(), "nobody"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

func TestGiteaUserProfileReadsPublicFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/users/ada" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get(giteaAuthHeader) != giteaTokenScheme+"tok" {
			t.Errorf("auth = %q", r.Header.Get(giteaAuthHeader))
		}
		_, _ = w.Write([]byte(`{"login":"ada","full_name":"Ada Lovelace","location":"Berlin, Germany","website":"https://ada.example","avatar_url":"https://codeberg.example/a.png","email":"never@read.example"}`))
	}))
	defer srv.Close()
	f := newTestGitea(t, srv.URL, "")
	f.token = "tok"
	got, err := f.UserProfile(context.Background(), "ada")
	if err != nil {
		t.Fatal(err)
	}
	want := UserProfile{Login: "ada", Name: "Ada Lovelace", Location: "Berlin, Germany", Website: "https://ada.example", AvatarURL: "https://codeberg.example/a.png"}
	if got != want {
		t.Fatalf("profile = %+v, want %+v", got, want)
	}
}

func TestUserProfileClassifiesStatusesWithoutEchoingTheLogin(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header string
		want   error
	}{
		{"not found", http.StatusNotFound, "", ErrUserNotFound},
		{"too many", http.StatusTooManyRequests, "", ErrRateLimited},
		{"forbidden spent", http.StatusForbidden, "0", ErrRateLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.header != "" {
					w.Header().Set(rateLimitRemainingHeader, tc.header)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`secret-login-echo`))
			}))
			defer srv.Close()
			f := newTestGitea(t, srv.URL, "")
			_, err := f.UserProfile(context.Background(), "secret-login")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`secret-login-echo`))
	}))
	defer srv.Close()
	f := newTestGitea(t, srv.URL, "")
	_, err := f.UserProfile(context.Background(), "secret-login")
	if err == nil || strings.Contains(err.Error(), "secret-login") {
		t.Fatalf("err = %v: must fail without naming the login", err)
	}
}

func TestUserProfileRefusesUnsafeLogins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a request was made for an unsafe login: %s", r.URL)
	}))
	defer srv.Close()
	f := newTestGitea(t, srv.URL, "")
	for _, login := range []string{"", "a/b", "a b", "../x", "a?b"} {
		if _, err := f.UserProfile(context.Background(), login); err == nil {
			t.Errorf("login %q accepted", login)
		}
	}
}
