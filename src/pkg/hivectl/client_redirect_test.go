package hivectl

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A 307 from the Hive server must not carry a POST body (here a PAT) to
// another host: the client stops at the redirect instead of following it.
func TestClientDoesNotResendBodyOnRedirect(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	var leaked atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		data, _ := io.ReadAll(r.Body)
		leaked.Store(string(data))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/github-token", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client, err := NewClient(origin.URL, "secret", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(context.Background(), http.MethodPut, "/api/github-token", nil, map[string]string{"token": "ghp_redirect"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("err = %v, want APIError 307", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("redirect target received %d request(s), body %q", n, leaked.Load())
	}
}

// A GET still follows a redirect, and a redirect loop stops after 10 hops.
func TestClientFollowsGetRedirectsWithLimit(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/new", http.StatusFound)
	})
	mux.HandleFunc("/api/new", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	var loops atomic.Int32
	mux.HandleFunc("/api/loop", func(w http.ResponseWriter, r *http.Request) {
		loops.Add(1)
		http.Redirect(w, r, "/api/loop", http.StatusFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := NewClient(server.URL, "secret", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(context.Background(), http.MethodGet, "/api/old", nil, nil); err != nil {
		t.Fatalf("GET redirect: %v", err)
	}
	if _, err := client.Do(context.Background(), http.MethodGet, "/api/loop", nil, nil); err == nil {
		t.Fatal("redirect loop: expected an error")
	}
	if n := loops.Load(); n != 10 {
		t.Fatalf("redirect loop made %d requests, want 10", n)
	}
}

// A credential is sent over https, or over plain http only to loopback.
func TestRequireSecureTransport(t *testing.T) {
	t.Parallel()
	for server, wantOK := range map[string]bool{
		"https://hive.example.com":    true,
		"https://10.0.0.5:3001":       true,
		"http://127.0.0.1:3001":       true,
		"http://127.8.9.10":           true,
		"http://[::1]:3001":           true,
		"http://localhost:3001":       true,
		"http://LocalHost":            true,
		"http://hive.example.com":     false,
		"http://10.0.0.5:3001":        false,
		"http://localhost.evil.test":  false,
		"http://127.0.0.1.evil.test/": false,
	} {
		client, err := NewClient(server, "", 5*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", server, err)
		}
		if err := client.RequireSecureTransport(); (err == nil) != wantOK {
			t.Errorf("%s: RequireSecureTransport() = %v, want ok=%v", server, err, wantOK)
		}
	}
}

// No redirect status carries a PUT body onward, including 301/302/303 (which
// net/http would turn into a GET) and 308, and the target is never contacted.
func TestClientRefusesEveryRedirectForBodyRequests(t *testing.T) {
	t.Parallel()
	for _, status := range []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	} {
		var hits atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/api/github-token", status)
		}))
		client, err := NewClient(origin.URL, "", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Do(context.Background(), http.MethodPut, "/api/github-token", nil, map[string]string{"token": "ghp_redirect"})
		origin.Close()
		target.Close()
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != status {
			t.Fatalf("status %d: err = %v, want APIError %d", status, err, status)
		}
		if strings.Contains(err.Error(), "ghp_redirect") {
			t.Fatalf("status %d: error echoes the body: %v", status, err)
		}
		if n := hits.Load(); n != 0 {
			t.Fatalf("status %d: redirect target received %d request(s)", status, n)
		}
	}
}

// The host is lowercased so net/http's case-sensitive "localhost" proxy
// bypass applies: http://LOCALHOST must not go through HTTP_PROXY.
func TestNewClientLowercasesHost(t *testing.T) {
	t.Parallel()
	client, err := NewClient("http://LocalHost:3001/Base", "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if client.baseURL.Host != "localhost:3001" || client.baseURL.Path != "/Base" {
		t.Fatalf("baseURL = %s", client.baseURL)
	}
}

// A GET that is redirected from https to plain http stops, so the
// Authorization header and session cookie are never sent in clear text.
func TestRefuseBodyRedirectsRefusesHTTPSDowngrade(t *testing.T) {
	t.Parallel()
	mustReq := func(method, raw string) *http.Request {
		req, err := http.NewRequest(method, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	via := []*http.Request{mustReq(http.MethodGet, "https://hive.example/api/status")}
	if err := refuseBodyRedirects(mustReq(http.MethodGet, "http://hive.example/api/status"), via); err == nil {
		t.Fatal("https -> http redirect was allowed")
	}
	if err := refuseBodyRedirects(mustReq(http.MethodGet, "https://hive.example/api/other"), via); err != nil {
		t.Fatalf("https -> https redirect refused: %v", err)
	}
	plain := []*http.Request{mustReq(http.MethodGet, "http://127.0.0.1/api/status")}
	if err := refuseBodyRedirects(mustReq(http.MethodGet, "http://127.0.0.1/api/new"), plain); err != nil {
		t.Fatalf("http -> http redirect refused: %v", err)
	}
}
