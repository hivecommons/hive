package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetPublicProfileReadsPublishedFieldsOnly(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/users/ada", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"login":"ada","name":"Ada Lovelace","company":"@acme","location":"London, UK","blog":"https://ada.example","avatar_url":"https://avatars.example/ada","bio":"never read","email":"never@read.example"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	p, err := c.GetPublicProfile(context.Background(), "@ada")
	if err != nil {
		t.Fatal(err)
	}
	want := PublicProfile{Login: "ada", Name: "Ada Lovelace", Company: "@acme", Location: "London, UK", Blog: "https://ada.example", AvatarURL: "https://avatars.example/ada"}
	if p != want || calls.Load() != 1 {
		t.Fatalf("profile = %+v (calls %d), want %+v", p, calls.Load(), want)
	}
}

func TestGetPublicProfileNotFoundIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	_, err := c.GetPublicProfile(context.Background(), "ghost-login")
	if !errors.Is(err, ErrProfileNotFound) || strings.Contains(err.Error(), "ghost-login") {
		t.Fatalf("err = %v", err)
	}
}

func TestGetPublicOrgNameMapsHandleToDisplayName(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/acme", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"login":"acme","name":"Acme Corporation"}`))
	})
	mux.HandleFunc("/orgs/someuser", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	if name, err := c.GetPublicOrgName(context.Background(), "@acme"); err != nil || name != "Acme Corporation" {
		t.Fatalf("org name = %q, %v", name, err)
	}
	if _, err := c.GetPublicOrgName(context.Background(), "someuser"); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("user handle err = %v", err)
	}
	if _, err := c.GetPublicOrgName(context.Background(), "a/b"); err == nil {
		t.Fatal("an unsafe handle was accepted")
	}
}

func TestOptionalRESTStandDownIgnoresSearchBucket(t *testing.T) {
	ResetRateLimitPacingForTest()
	t.Cleanup(ResetRateLimitPacingForTest)
	ResetRateLimitBudgetForTest()
	t.Cleanup(ResetRateLimitBudgetForTest)
	prev := sharedRESTAccountingPtr.Load()
	sharedRESTAccountingPtr.Store(&restAccountingStore{now: time.Now, coreRemaining: -1})
	t.Cleanup(func() { sharedRESTAccountingPtr.Store(prev) })
	var nilClient *Client
	if got := nilClient.OptionalRESTStandDown(); got == "" {
		t.Fatal("no client must stand down")
	}
	c := &Client{}
	sharedRateLimitBudget.observe("search", mentionSearchLowRemaining, 30)
	if got := c.OptionalRESTStandDown(); got != "" {
		t.Fatalf("REST gate stood down on the search bucket: %q", got)
	}
	if got := c.OptionalSearchStandDown(); got != "search_low" {
		t.Fatalf("search gate = %q", got)
	}
	sharedSlowStartState.mu.Lock()
	sharedSlowStartState.cautiousUntil = time.Now().Add(time.Minute)
	sharedSlowStartState.mu.Unlock()
	if got := c.OptionalRESTStandDown(); got != "caution_window" {
		t.Fatalf("caution: %q", got)
	}
}
