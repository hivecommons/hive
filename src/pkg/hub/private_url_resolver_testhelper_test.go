package hub

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Audit finding L3 (2026-08-17 security review): several tests in this package
// asserted the NON-private branch of isPrivateURL using public hostnames such
// as "example.com". isPrivateURL resolves DNS and FAILS CLOSED when resolution
// errors — correct, intended production behaviour — so those tests passed only
// on a machine with working external DNS and failed closed in a
// network-isolated CI sandbox. The tests were non-hermetic; the guard was not
// wrong. The fix is to give the test control of resolution, never to soften the
// guard.
//
// stubPrivateURLResolver installs a deterministic, in-process resolver for the
// duration of one test and restores the production resolver afterwards. It
// resolves any host to publicTestResolvedAddr — a documentation-range address
// that is deliberately OUTSIDE every private prefix isPrivateURL blocks — so a
// public hostname is classified public without a single external DNS query.
//
// It resolves ONLY hosts the caller lists. Any other host returns an error,
// which isPrivateURL treats as private (fail-closed), so a test that starts
// touching an unexpected hostname fails loudly instead of silently inheriting a
// permissive answer. Production is untouched: privateURLResolver is
// defaultHostResolver everywhere except inside a test that calls this helper.
func stubPrivateURLResolver(t *testing.T, publicHosts ...string) {
	t.Helper()

	allowed := make(map[string]struct{}, len(publicHosts))
	for _, h := range publicHosts {
		allowed[strings.ToLower(h)] = struct{}{}
	}

	orig := privateURLResolver
	privateURLResolver = func(_ context.Context, host string) ([]string, error) {
		if _, ok := allowed[strings.ToLower(host)]; ok {
			return []string{publicTestResolvedAddr}, nil
		}
		return nil, fmt.Errorf("stubPrivateURLResolver: host %q not registered as public in this test", host)
	}
	t.Cleanup(func() { privateURLResolver = orig })
}

// publicTestResolvedAddr is the address stubPrivateURLResolver returns for
// hosts a test declares public. 203.0.113.0/24 is TEST-NET-3 (RFC 5737),
// reserved for documentation, so it is guaranteed never to be routed and never
// to collide with any private range isPrivateURL blocks.
const publicTestResolvedAddr = "203.0.113.10"

// TestStubPrivateURLResolverIsFailClosed is the positive control for the helper
// above: it proves the stub does not accidentally make isPrivateURL permissive.
// An unregistered host must still be reported private, and the private-prefix
// checks must still fire even for a host the stub was told is public.
func TestStubPrivateURLResolverIsFailClosed(t *testing.T) {
	stubPrivateURLResolver(t, "registered.example")

	if isPrivateURL(context.Background(), "https://registered.example") {
		t.Error("registered public host classified private; stub resolver is not wired in")
	}
	if !isPrivateURL(context.Background(), "https://unregistered.example") {
		t.Error("unregistered host must fail closed (private), got public")
	}
	// Literal private addresses are rejected before resolution ever happens,
	// so the stub cannot whitelist them.
	if !isPrivateURL(context.Background(), "https://127.0.0.1") {
		t.Error("loopback literal must stay private with the stub installed")
	}
	if !isPrivateURL(context.Background(), "https://192.168.1.1") {
		t.Error("RFC1918 literal must stay private with the stub installed")
	}
}
