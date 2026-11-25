package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsForgejoVersion(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"7.0.0+gitea-1.22.0", true},
		{"13.0.2+gitea-1.22.0", true},
		{"9.0.0-dev-1a2b3c4+gitea-1.22.0", true},
		{"1.21.7+0", true},
		{"1.18.0+1", true},
		{"1.22.3", false},
		{"1.23.0-rc1", false},
		{"1.23.0+dev-456-g1234567", false},
		{"1.21.0+12-gabcdef0", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsForgejoVersion(c.version); got != c.want {
			t.Errorf("IsForgejoVersion(%q) = %v, want %v", c.version, got, c.want)
		}
	}
}

func TestGiteaFamilyVersion(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"version":"11.0.1+gitea-1.22.0"}`))
	}))
	defer srv.Close()

	v, err := GiteaFamilyVersion(context.Background(), "tok", Options{BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatalf("GiteaFamilyVersion: %v", err)
	}
	if v != "11.0.1+gitea-1.22.0" {
		t.Errorf("version = %q", v)
	}
	if gotPath != "/api/v1/version" {
		t.Errorf("path = %q, want /api/v1/version", gotPath)
	}
	if gotAuth != "token tok" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "token tok")
	}
}

func TestGiteaFamilyVersion_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sign in required", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := GiteaFamilyVersion(context.Background(), "", Options{BaseURL: srv.URL}); err == nil {
		t.Error("401: want error")
	}
	if _, err := GiteaFamilyVersion(context.Background(), "", Options{}); err == nil {
		t.Error("empty BaseURL: want error")
	}
}
