package knowledge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestFetchContext7Docs_RejectsNonSlashLibraryID verifies that a libraryID
// not beginning with "/" is rejected before any HTTP request is made.
// A libraryID like "@evil.com/path" would produce the URL
// "https://context7.com@evil.com/path/llms.txt" where evil.com is the host
// (Go's net/url treats context7.com as userinfo), enabling SSRF.
func TestFetchContext7Docs_RejectsNonSlashLibraryID(t *testing.T) {
	_, err := FetchContext7Docs(context.Background(), "@evil.com/path", "", "")
	if err == nil {
		t.Fatal("FetchContext7Docs accepted a libraryID without leading slash (authority injection)")
	}
	if !strings.Contains(err.Error(), "must start with '/'") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestFetchContext7Docs_RejectsEmptyLibraryID checks that an empty libraryID is
// also rejected (empty string does not start with "/").
func TestFetchContext7Docs_RejectsEmptyLibraryID(t *testing.T) {
	_, err := FetchContext7Docs(context.Background(), "", "", "")
	if err == nil {
		t.Fatal("FetchContext7Docs accepted an empty libraryID")
	}
}

// TestContext7Client_BlocksRedirectToInternalHostname verifies the end-to-end
// SSRF redirect guard: a server that 302s to a hostname resolving to the
// cloud metadata address must be blocked by context7Client.CheckRedirect.
func TestContext7Client_BlocksRedirectToInternalHostname(t *testing.T) {
	stubDocRedirectResolver(t, map[string][]string{
		"metadata.attacker.example": {"169.254.169.254"},
	})

	// Real HTTP server that redirects to the internal hostname.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://metadata.attacker.example/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	_, err = context7Client.Do(req)
	if err == nil {
		t.Fatal("context7Client followed a redirect to a hostname resolving to 169.254.169.254 (SSRF)")
	}
	if !strings.Contains(err.Error(), "private/internal host blocked") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestUseContext7TestServerDoesNotWeakenRedirectGuard verifies that the
// httptest routing seam keeps the same SSRF redirect protection as the
// production Context7 client.
func TestUseContext7TestServerDoesNotWeakenRedirectGuard(t *testing.T) {
	var redirectorHits atomic.Int32
	var privateHits atomic.Int32
	privateTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		privateHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer privateTarget.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectorHits.Add(1)
		http.Redirect(w, r, privateTarget.URL+"/metadata", http.StatusFound)
	}))
	defer redirector.Close()

	useContext7TestServer(t, redirector.URL)
	if context7Client.CheckRedirect == nil {
		t.Fatal("useContext7TestServer installed a client without redirect protection")
	}

	_, err := FetchContext7Docs(context.Background(), "/safe/lib", "", "")
	if err == nil {
		t.Fatal("test Context7 client followed a redirect to a private host")
	}
	if !strings.Contains(err.Error(), "private/internal host blocked") {
		t.Fatalf("expected private-host redirect guard error, got: %v", err)
	}
	if redirectorHits.Load() != 1 {
		t.Fatalf("redirector received %d request(s), want 1", redirectorHits.Load())
	}
	if privateHits.Load() != 0 {
		t.Fatalf("redirect guard allowed %d request(s) to private target, want 0", privateHits.Load())
	}
}
