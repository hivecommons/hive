package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyInjectsBearerAndRewritesHost(t *testing.T) {
	var got http.Header
	var gotHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotHost = r.Host
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	rp, err := newProxy(upstream.URL+"/ignored/path?kiosk=1", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(rp)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/api/stage-tracker", nil)
	req.Header.Set("Authorization", "Bearer from-browser")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got.Get("Authorization") != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want the injected token to replace the browser's", got.Get("Authorization"))
	}
	if gotHost != upstream.Listener.Addr().String() {
		t.Errorf("Host = %q, want upstream host", gotHost)
	}
	if got.Get("X-Forwarded-For") != "" {
		t.Errorf("X-Forwarded-For leaked: %q", got.Get("X-Forwarded-For"))
	}
}

func TestProxyRejectsBadTarget(t *testing.T) {
	for _, target := range []string{"", "hive.example.com", "/relative"} {
		if _, err := newProxy(target, ""); err == nil {
			t.Errorf("newProxy(%q) accepted an invalid origin", target)
		}
	}
}

func TestRedactURL(t *testing.T) {
	if got := redactURL("https://h/x?token=abc"); got != "https://h/x?…" {
		t.Errorf("redactURL = %q", got)
	}
}
