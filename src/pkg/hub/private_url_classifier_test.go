package hub

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

// TestIsPrivateURLSharedClassifier pins the address classes the old
// string-prefix blocklist missed (audit #18986). The resolver answers the way
// the system resolver does for an IP literal (it returns the literal), so a
// literal is judged on the address it denotes and not rescued by a resolver
// error.
func TestIsPrivateURLSharedClassifier(t *testing.T) {
	orig := privateURLResolver
	t.Cleanup(func() { privateURLResolver = orig })
	privateURLResolver = func(_ context.Context, host string) ([]string, error) {
		if _, err := netip.ParseAddr(host); err == nil {
			return []string{host}, nil
		}
		switch host {
		case "public.example":
			return []string{"93.184.216.34"}, nil
		case "cgnat.example":
			return []string{"100.64.0.9"}, nil
		case "ula.example":
			return []string{"fd00::1"}, nil
		case "v6loop.example":
			return []string{"::1"}, nil
		}
		return nil, errors.New("no such host")
	}

	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"cgnat literal", "http://100.64.0.1/", true},
		{"cgnat metadata", "http://100.100.100.200/", true},
		{"ula literal", "http://[fd00::1]:8080/", true},
		{"link-local v6", "http://[fe80::1]/", true},
		{"loopback v6 with port", "http://[::1]:8080/", true},
		{"ipv4-mapped", "http://[::ffff:127.0.0.1]/", true},
		{"nat64 loopback", "http://[64:ff9b::7f00:1]/", true},
		{"multicast", "http://224.0.0.1/", true},
		{"name to cgnat", "https://cgnat.example/", true},
		{"name to ula", "https://ula.example/", true},
		{"name to v6 loopback", "https://v6loop.example/", true},
		{"userinfo before loopback", "http://public.example@127.0.0.1/", true},
		{"userinfo before v6 loopback", "http://public.example@[::1]/", true},
		{"decimal literal", "http://2130706433/", true},
		{"octal literal", "http://0177.0.0.1/", true},
		{"hex literal", "http://0x7f.1/", true},
		{"short literal", "http://127.1/", true},
		{"trailing dot", "http://127.0.0.1./", true},
		{"metadata", "http://169.254.169.254/", true},
		{"public name", "https://public.example/x", false},
		{"public name with userinfo", "https://u@public.example/x", false},
		{"public v4 literal", "https://93.184.216.34/", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPrivateURL(context.Background(), tc.raw); got != tc.want {
				t.Fatalf("isPrivateURL(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
