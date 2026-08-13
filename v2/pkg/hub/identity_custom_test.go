package hub

import "testing"

// TestCanonical_CustomProvider: the generic "custom" provider is a first-class
// canonical namespace, distinct from every branded provider, and it accepts the
// real-world OIDC subject shapes IdPs emit (Okta/Auth0 subs contain '|').
func TestCanonical_CustomProvider(t *testing.T) {
	// A path-hostile subject (contains ':') is still rejected.
	if _, err := makeCanonical("custom", "a:b"); err == nil {
		t.Fatal("subject with ':' must be rejected")
	}
	// A realistic Okta-style subject ('|' is allowed) round-trips, and its
	// on-disk filename stays path-safe via the -XX- escape.
	id, err := makeCanonical("custom", "okta|00u123")
	if err != nil {
		t.Fatalf("makeCanonical(custom, okta|00u123): %v", err)
	}
	if id != "custom:okta|00u123" {
		t.Errorf("id = %q", id)
	}
	stem, err := encodeUserFilename(id)
	if err != nil {
		t.Fatalf("encodeUserFilename: %v", err)
	}
	if got, ok := decodeUserFilename(stem); !ok || got != id {
		t.Errorf("filename round-trip: stem=%q got=%q ok=%v", stem, got, ok)
	}
	// Cross-provider isolation: custom:x is NOT github:x.
	if canonicalEqual("custom:x", "github:x") {
		t.Error("custom:x must NOT equal github:x — cross-provider collision")
	}
}
