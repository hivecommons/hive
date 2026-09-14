package dashboard

import (
	"reflect"
	"testing"
)

// contribute_ws_quota_preflight_negotiation_test.go pins the hub half of
// kubestellar/hive#6954: the hub must decide whether to withhold the auto-accept
// credential from the ADVERTISED relay_capabilities set, never from a
// protocol-version proxy. #6931 added quota_preflight_v1 to the hub's outbound
// set but left the reverse direction unbuilt, so the hub read
// contributorSupportsQuotaPreflight off RelayProtocolVersion != "" — which
// withholds from every relay that declares any version. These tests fail if that
// proxy returns.

// TestQuotaPreflightGatesOnAdvertisedCapability is the mutation-visible core:
// the gate must key on the quota_preflight_v1 token, so a relay that declares a
// protocol version but NOT the capability is treated as unable to preflight.
// Under the reverted proxy (RelayProtocolVersion != "") the version-only relay
// returns true and this fails.
func TestQuotaPreflightGatesOnAdvertisedCapability(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		caps *ContributorCapabilities
		want bool
	}{
		{
			// new relay ↔ new hub: advertises the capability → hub withholds and
			// waits for task_accepted / task_declined.
			name: "advertises quota_preflight_v1",
			caps: &ContributorCapabilities{
				RelayProtocolVersion: "1.2",
				RelayCapabilities:    []string{"quota_preflight_v1"},
			},
			want: true,
		},
		{
			// old relay ↔ new hub: declares a version (every deployed relay does,
			// #6931 never bumped it) but NOT the capability → hub must NOT withhold,
			// i.e. pre-#6833 immediate credential delivery. This is the exact case
			// the version proxy got wrong.
			name: "declares version only, no capability",
			caps: &ContributorCapabilities{RelayProtocolVersion: "1.2"},
			want: false,
		},
		{
			// unversioned/undeclared relay: nothing to gate on → deliver.
			name: "declares nothing",
			caps: &ContributorCapabilities{},
			want: false,
		},
		{
			// A relay that advertises OTHER capabilities but not this one is still
			// not a preflight relay — the token, not the presence of a list, decides.
			name: "advertises unrelated capabilities only",
			caps: &ContributorCapabilities{RelayCapabilities: []string{"token_refresh", "some_future_thing"}},
			want: false,
		},
		{
			// Fail closed on a nil declaration.
			name: "nil capabilities",
			caps: nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &ContributorConnection{capabilities: tc.caps}
			if got := contributorSupportsQuotaPreflight(conn); got != tc.want {
				t.Fatalf("contributorSupportsQuotaPreflight = %v, want %v (caps=%+v)", got, tc.want, tc.caps)
			}
		})
	}
	// A nil connection must also fail closed rather than panic.
	if contributorSupportsQuotaPreflight(nil) {
		t.Fatal("a nil connection must not be treated as preflight-capable")
	}
}

// TestDeclaresCapabilityExactMatch pins that capability matching is exact against
// a sanitized token: padding a client sends cannot make a capability appear, and
// an honest token is not defeated by trailing whitespace on the wire.
func TestDeclaresCapabilityExactMatch(t *testing.T) {
	t.Parallel()
	c := ContributorCapabilities{RelayCapabilities: []string{"  quota_preflight_v1\t"}}
	if !c.DeclaresCapability("quota_preflight_v1") {
		t.Fatal("a token that only differs by surrounding whitespace must still match after sanitizing")
	}
	if c.DeclaresCapability("quota_preflight_v2") {
		t.Fatal("a near-miss token must not match")
	}
	if c.DeclaresCapability("") {
		t.Fatal("an empty query token must never match")
	}
	empty := ContributorCapabilities{}
	if empty.DeclaresCapability("quota_preflight_v1") {
		t.Fatal("an undeclared relay must declare no capability")
	}
}

// TestSanitizeCapabilityTokensBoundsAndCleans pins that the relay-declared list
// gets the same hygiene the scalar declared fields do: empties dropped, control
// characters/whitespace collapsed, and the list capped so a client cannot bloat
// every fleet poll. Hygiene, not validation — a nonsense token survives but
// cannot match a real capability.
func TestSanitizeCapabilityTokensBoundsAndCleans(t *testing.T) {
	t.Parallel()
	if got := sanitizeCapabilityTokens(nil); got != nil {
		t.Fatalf("nil in must stay nil, got %v", got)
	}
	if got := sanitizeCapabilityTokens([]string{"   ", "\t\n"}); got != nil {
		t.Fatalf("a list of only-whitespace tokens must sanitize to nil, got %v", got)
	}
	got := sanitizeCapabilityTokens([]string{"quota_preflight_v1", "  ", "token_refresh"})
	if !reflect.DeepEqual(got, []string{"quota_preflight_v1", "token_refresh"}) {
		t.Fatalf("empties must be dropped and honest tokens kept, got %v", got)
	}
	over := make([]string, capabilityListMaxLen+50)
	for i := range over {
		over[i] = "cap"
	}
	if got := sanitizeCapabilityTokens(over); len(got) > capabilityListMaxLen {
		t.Fatalf("list must be capped at %d, got %d", capabilityListMaxLen, len(got))
	}
}
