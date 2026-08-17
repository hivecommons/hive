package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// routeImportFetchesToServer lets a test exercise the import handler with a
// realistic PUBLIC url while the bytes actually come from a local httptest
// server.
//
// This is required because the import preflight now rejects private/internal
// destinations, and an httptest server listens on 127.0.0.1. Pointing a test
// straight at ts.URL would be blocked — correctly. Rather than punch a
// test-only hole in the guard (which would also exist in production), the
// request keeps its public URL through validation and only the TRANSPORT is
// swapped, so the security check under test is the real one.
//
// The resolver is stubbed alongside it so the preflight never depends on live
// DNS in CI.
func routeImportFetchesToServer(t *testing.T, ts *httptest.Server) {
	t.Helper()
	target, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	stubImportResolver(t, map[string][]string{
		"raw.githubusercontent.com": {"185.199.108.133"},
		"github.com":                {"140.82.121.4"},
		"gist.github.com":           {"140.82.121.3"},
	})
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rewritten := req.Clone(req.Context())
		rewritten.URL.Scheme = target.Scheme
		rewritten.URL.Host = target.Host
		rewritten.Host = req.URL.Host
		return previous.RoundTrip(rewritten)
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
}

// Guard against the helper above being mistaken for a production bypass: the
// preflight must still reject an internal destination while it is installed.
func TestRouteImportFetchesToServerDoesNotWeakenGuard(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	routeImportFetchesToServer(t, ts)
	if err := validateImportFetchURL(context.Background(), "http://169.254.169.254/latest/meta-data/"); err == nil {
		t.Fatal("test transport helper weakened the SSRF preflight")
	}
}
