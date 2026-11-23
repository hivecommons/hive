package linearagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchMemberByEmail(t *testing.T) {
	var gotEmail, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Variables map[string]string `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotEmail = body.Variables["email"]
		if gotEmail == "nobody@example.com" {
			_, _ = w.Write([]byte(`{"data":{"users":{"nodes":[]}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"users":{"nodes":[{"name":"Ada Lovelace","displayName":"ada","avatarUrl":"https://linear.example/a.png"}]}}}`))
	}))
	defer srv.Close()
	m, err := FetchMemberByEmail(context.Background(), srv.Client(), srv.URL, "tok", " ada@example.com ")
	if err != nil {
		t.Fatal(err)
	}
	if m != (Member{Name: "Ada Lovelace", DisplayName: "ada", AvatarURL: "https://linear.example/a.png"}) {
		t.Fatalf("member = %+v", m)
	}
	if gotEmail != "ada@example.com" || gotAuth != "Bearer tok" {
		t.Fatalf("email %q auth %q", gotEmail, gotAuth)
	}
	if _, err := FetchMemberByEmail(context.Background(), srv.Client(), srv.URL, "tok", "nobody@example.com"); !errors.Is(err, ErrMemberNotFound) {
		t.Fatalf("missing member err = %v", err)
	}
}

func TestFetchMemberByEmailRateLimitAndGuards(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	if _, err := FetchMemberByEmail(context.Background(), srv.Client(), srv.URL, "tok", "ada@example.com"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429 err = %v", err)
	}
	if _, err := FetchMemberByEmail(context.Background(), srv.Client(), srv.URL, "", "ada@example.com"); err == nil {
		t.Fatal("no token accepted")
	}
	if _, err := FetchMemberByEmail(context.Background(), srv.Client(), srv.URL, "tok", "not-an-email"); err == nil {
		t.Fatal("non-email accepted")
	}
}
