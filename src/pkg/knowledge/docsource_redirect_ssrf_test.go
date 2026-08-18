package knowledge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubDocRedirectResolver points docRedirectResolver at a fixed map for the
// duration of a test, restoring the real resolver afterwards so no test leaks a
// stubbed DNS into the rest of the package.
func stubDocRedirectResolver(t *testing.T, hosts map[string][]string) {
	t.Helper()
	orig := docRedirectResolver
	t.Cleanup(func() { docRedirectResolver = orig })
	docRedirectResolver = func(_ context.Context, host string) ([]string, error) {
		if addrs, ok := hosts[strings.ToLower(host)]; ok {
			return addrs, nil
		}
		return nil, errors.New("no such host")
	}
}

// The exploit this guard exists to stop: a public URL that passes the
// handler's pre-fetch isPrivateURL check, then 302s to a HOSTNAME (not an IP
// literal) that resolves to the cloud metadata address. Checking IP literals
// only let this through and reached 169.254.169.254.
func TestDocNoRedirectToPrivate_BlocksHostnameResolvingToMetadata(t *testing.T) {
	stubDocRedirectResolver(t, map[string][]string{
		"metadata.attacker.example": {"169.254.169.254"},
	})
	req := httptest.NewRequest(http.MethodGet, "http://metadata.attacker.example/latest/meta-data/", nil)
	err := docNoRedirectToPrivate(req, nil)
	if err == nil {
		t.Fatal("redirect to hostname resolving to 169.254.169.254 was allowed (SSRF)")
	}
	if !strings.Contains(err.Error(), "private/internal host blocked") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Same bypass shape against an RFC1918 target.
func TestDocNoRedirectToPrivate_BlocksHostnameResolvingToRFC1918(t *testing.T) {
	stubDocRedirectResolver(t, map[string][]string{"internal.attacker.example": {"10.0.0.5"}})
	req := httptest.NewRequest(http.MethodGet, "https://internal.attacker.example/secret", nil)
	if err := docNoRedirectToPrivate(req, nil); err == nil {
		t.Fatal("redirect to hostname resolving to 10.0.0.5 was allowed (SSRF)")
	}
}

// A host with several A records must be blocked if ANY of them is internal —
// otherwise an attacker just adds one public record alongside the private one.
func TestDocNoRedirectToPrivate_BlocksMixedPublicAndPrivateRecords(t *testing.T) {
	stubDocRedirectResolver(t, map[string][]string{"mixed.attacker.example": {"93.184.216.34", "127.0.0.1"}})
	req := httptest.NewRequest(http.MethodGet, "https://mixed.attacker.example/x", nil)
	if err := docNoRedirectToPrivate(req, nil); err == nil {
		t.Fatal("redirect to host with one private record was allowed (SSRF)")
	}
}

// DNS failure must fail CLOSED, matching isPrivateURL in the dashboard.
func TestDocNoRedirectToPrivate_FailsClosedOnDNSError(t *testing.T) {
	stubDocRedirectResolver(t, map[string][]string{})
	req := httptest.NewRequest(http.MethodGet, "https://unresolvable.attacker.example/x", nil)
	if err := docNoRedirectToPrivate(req, nil); err == nil {
		t.Fatal("redirect with failing DNS was allowed; guard must fail closed")
	}
}

// A redirect to a non-http(s) scheme is never legitimate for a document fetch.
func TestDocNoRedirectToPrivate_BlocksNonHTTPScheme(t *testing.T) {
	stubDocRedirectResolver(t, map[string][]string{"evil.example": {"93.184.216.34"}})
	req := httptest.NewRequest(http.MethodGet, "https://evil.example/x", nil)
	req.URL.Scheme = "file"
	if err := docNoRedirectToPrivate(req, nil); err == nil {
		t.Fatal("redirect to file:// was allowed")
	}
}

// A genuinely public redirect target must still be followed, so the guard does
// not break legitimate document imports behind a CDN or shortener.
func TestDocNoRedirectToPrivate_AllowsPublicTarget(t *testing.T) {
	stubDocRedirectResolver(t, map[string][]string{"docs.example.com": {"93.184.216.34"}})
	req := httptest.NewRequest(http.MethodGet, "https://docs.example.com/guide.md", nil)
	if err := docNoRedirectToPrivate(req, nil); err != nil {
		t.Fatalf("public redirect target blocked: %v", err)
	}
}

// The chain-depth cap is enforced before any host check, so a redirect loop
// between public hosts still terminates.
func TestDocNoRedirectToPrivate_CapsChainDepth(t *testing.T) {
	stubDocRedirectResolver(t, map[string][]string{"docs.example.com": {"93.184.216.34"}})
	req := httptest.NewRequest(http.MethodGet, "https://docs.example.com/guide.md", nil)
	via := []*http.Request{req, req, req}
	err := docNoRedirectToPrivate(req, via)
	if err == nil || !strings.Contains(err.Error(), "stopped after") {
		t.Fatalf("chain depth not capped: %v", err)
	}
}

// End-to-end through fetchURL: a real server 302s to a hostname that resolves
// internally, and the fetch must fail rather than return metadata content.
func TestFetchURL_RejectsRedirectToInternalHostname(t *testing.T) {
	stubDocRedirectResolver(t, map[string][]string{"metadata.attacker.example": {"169.254.169.254"}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://metadata.attacker.example/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	ds := &DocumentSource{}
	if _, _, err := ds.fetchURL(context.Background(), srv.URL); err == nil {
		t.Fatal("fetchURL followed a redirect to an internal hostname")
	}
}
