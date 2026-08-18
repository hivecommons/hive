package dashboard

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestNoRedirectToPrivate_MaxDepth verifies that the redirect chain depth limit
// is enforced even when every hop targets a public address.
func TestNoRedirectToPrivate_MaxDepth(t *testing.T) {
	// Stub the resolver so all hosts resolve to a public IP.
	origResolver := privateURLResolver
	privateURLResolver = func(_ context.Context, host string) ([]string, error) {
		return []string{"93.184.216.34"}, nil // public
	}
	t.Cleanup(func() { privateURLResolver = origResolver })

	target, _ := url.Parse("https://example.com/final")
	req := &http.Request{URL: target}

	// At exactly ssrfMaxRedirects prior hops → error.
	via := make([]*http.Request, ssrfMaxRedirects)
	for i := range via {
		u, _ := url.Parse("https://example.com/hop")
		via[i] = &http.Request{URL: u}
	}

	err := noRedirectToPrivate(req, via)
	if err == nil {
		t.Fatal("expected error when redirect chain reaches ssrfMaxRedirects")
	}
	if !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("error should mention redirects, got: %v", err)
	}
}

// TestNoRedirectToPrivate_BelowMaxDepthAllowed verifies that fewer than
// ssrfMaxRedirects hops to a public target is allowed.
func TestNoRedirectToPrivate_BelowMaxDepthAllowed(t *testing.T) {
	origResolver := privateURLResolver
	privateURLResolver = func(_ context.Context, host string) ([]string, error) {
		return []string{"93.184.216.34"}, nil
	}
	t.Cleanup(func() { privateURLResolver = origResolver })

	target, _ := url.Parse("https://example.com/ok")
	req := &http.Request{URL: target}
	via := make([]*http.Request, ssrfMaxRedirects-1)
	for i := range via {
		u, _ := url.Parse("https://example.com/hop")
		via[i] = &http.Request{URL: u}
	}

	if err := noRedirectToPrivate(req, via); err != nil {
		t.Fatalf("expected no error for %d redirects to public host, got: %v", len(via), err)
	}
}

// TestNoRedirectToPrivate_PrivateTargetBlocked verifies that a redirect to a
// private/loopback IP is rejected even on the first redirect.
func TestNoRedirectToPrivate_PrivateTargetBlocked(t *testing.T) {
	origResolver := privateURLResolver
	privateURLResolver = func(_ context.Context, host string) ([]string, error) {
		return []string{"127.0.0.1"}, nil // loopback
	}
	t.Cleanup(func() { privateURLResolver = origResolver })

	target, _ := url.Parse("https://internal.example.com/secret")
	req := &http.Request{URL: target}
	via := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "public.example.com"}}}

	err := noRedirectToPrivate(req, via)
	if err == nil {
		t.Fatal("expected error for redirect to private host")
	}
	if !strings.Contains(err.Error(), "private") {
		t.Fatalf("error should mention private/internal, got: %v", err)
	}
}

// TestNoRedirectToPrivate_LinkLocalBlocked verifies that link-local metadata
// endpoints (e.g., cloud IMDS at 169.254.169.254) are blocked on redirect.
func TestNoRedirectToPrivate_LinkLocalBlocked(t *testing.T) {
	origResolver := privateURLResolver
	privateURLResolver = func(_ context.Context, host string) ([]string, error) {
		return []string{"169.254.169.254"}, nil
	}
	t.Cleanup(func() { privateURLResolver = origResolver })

	target, _ := url.Parse("http://169.254.169.254/latest/meta-data/")
	req := &http.Request{URL: target}
	via := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "legit.example.com"}}}

	err := noRedirectToPrivate(req, via)
	if err == nil {
		t.Fatal("expected error for redirect to link-local (cloud IMDS) address")
	}
}

// TestNoRedirectToPrivate_RFC1918Blocked verifies that RFC 1918 private ranges
// (10.x, 172.16-31.x, 192.168.x) are rejected on redirect.
func TestNoRedirectToPrivate_RFC1918Blocked(t *testing.T) {
	privateRanges := []string{"10.0.0.1", "172.16.0.1", "192.168.1.1"}

	for _, ip := range privateRanges {
		t.Run(ip, func(t *testing.T) {
			origResolver := privateURLResolver
			privateURLResolver = func(_ context.Context, host string) ([]string, error) {
				return []string{ip}, nil
			}
			t.Cleanup(func() { privateURLResolver = origResolver })

			target, _ := url.Parse("https://redir.example.com/")
			req := &http.Request{URL: target}
			via := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "start.example.com"}}}

			if err := noRedirectToPrivate(req, via); err == nil {
				t.Fatalf("expected redirect to %s to be blocked", ip)
			}
		})
	}
}

// TestNoRedirectToPrivate_DNSFailureBlocked verifies that a DNS lookup failure
// on a redirect target results in blocking (fail-closed).
func TestNoRedirectToPrivate_DNSFailureBlocked(t *testing.T) {
	origResolver := privateURLResolver
	privateURLResolver = func(_ context.Context, host string) ([]string, error) {
		return nil, &dnsError{host: host}
	}
	t.Cleanup(func() { privateURLResolver = origResolver })

	target, _ := url.Parse("https://unresolvable.example.com/path")
	req := &http.Request{URL: target}
	via := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "start.example.com"}}}

	err := noRedirectToPrivate(req, via)
	if err == nil {
		t.Fatal("expected redirect to unresolvable host to be blocked (fail-closed)")
	}
}

// TestNoRedirectToPrivate_FirstRedirectPublicAllowed verifies that the very
// first redirect to a public host is permitted.
func TestNoRedirectToPrivate_FirstRedirectPublicAllowed(t *testing.T) {
	origResolver := privateURLResolver
	privateURLResolver = func(_ context.Context, host string) ([]string, error) {
		return []string{"93.184.216.34"}, nil
	}
	t.Cleanup(func() { privateURLResolver = origResolver })

	target, _ := url.Parse("https://cdn.example.com/resource")
	req := &http.Request{URL: target}
	via := []*http.Request{} // first redirect: via is empty

	if err := noRedirectToPrivate(req, via); err != nil {
		t.Fatalf("first redirect to public host should be allowed, got: %v", err)
	}
}

// dnsError is a minimal error type to simulate DNS failures in tests.
type dnsError struct {
	host string
}

func (e *dnsError) Error() string {
	return "lookup " + e.host + ": no such host"
}
