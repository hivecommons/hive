package netguard

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// echoResolver answers like the system resolver does for the hosts these
// tests use: an IP literal resolves to itself, and a few names map to fixed
// answers. Anything else fails, which HostBlocked must treat as blocked.
func echoResolver(_ context.Context, host string) ([]string, error) {
	if _, err := netip.ParseAddr(host); err == nil {
		return []string{host}, nil
	}
	switch host {
	case "public.example":
		return []string{"93.184.216.34", "2606:4700:4700::1111"}, nil
	case "mixed.example":
		return []string{"93.184.216.34", "10.0.0.5"}, nil
	case "cgnat.example":
		return []string{"100.64.0.9"}, nil
	case "ula.example":
		return []string{"fd00::1"}, nil
	case "v6loop.example":
		return []string{"::1"}, nil
	case "mapped.example":
		return []string{"::ffff:7f00:1"}, nil
	case "garbage.example":
		return []string{"not-an-ip"}, nil
	case "empty.example":
		return nil, nil
	}
	return nil, errors.New("no such host")
}

func TestHostBlocked(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		// Classes the old string-prefix list missed.
		{"cgnat literal", "http://100.64.0.1/", true},
		{"cgnat metadata", "http://100.100.100.200/latest", true},
		{"ula literal", "http://[fd00::1]:8080/", true},
		{"ula fc literal", "http://[fc00::1]/", true},
		{"link-local v6", "http://[fe80::1]/", true},
		{"link-local v6 zone", "http://[fe80::1%25eth0]/", true},
		{"loopback v6 with port", "http://[::1]:8080/x", true},
		{"loopback v6 bare", "::1", true},
		{"ipv4-mapped bracketed", "http://[::ffff:127.0.0.1]/", true},
		{"ipv4-mapped hex bracketed", "http://[::ffff:7f00:1]/", true},
		{"nat64 loopback", "http://[64:ff9b::7f00:1]/", true},
		{"6to4 private", "http://[2002:0a00:0001::]/", true},
		{"multicast v4", "http://224.0.0.1/", true},
		{"multicast v6", "http://[ff02::1]/", true},
		{"name to cgnat", "https://cgnat.example/", true},
		{"name to ula", "https://ula.example/", true},
		{"name to v6 loopback", "https://v6loop.example/", true},
		{"name to mapped loopback", "https://mapped.example/", true},
		{"one private answer among public", "https://mixed.example/", true},
		// Spellings.
		{"decimal integer", "http://2130706433/", true},
		{"octal dotted", "http://0177.0.0.1/", true},
		{"hex dotted", "http://0x7f.1/", true},
		{"short form", "http://127.1/", true},
		{"trailing dot", "http://127.0.0.1./", true},
		{"uppercase localhost", "http://LOCALHOST:3000/", true},
		{"localhost trailing dot", "http://localhost./", true},
		{"userinfo before loopback", "http://public.example@127.0.0.1/", true},
		{"userinfo before v6 loopback", "http://public.example@[::1]:80/", true},
		{"userinfo with colon", "http://user:pw@169.254.169.254/", true},
		{"metadata", "http://169.254.169.254/latest/meta-data", true},
		// Already covered before; must stay blocked.
		{"rfc1918 10", "http://10.1.2.3/", true},
		{"rfc1918 172", "http://172.20.0.1/", true},
		{"rfc1918 192", "http://192.168.1.1/", true},
		{"unspecified", "http://0.0.0.0/", true},
		{"this network", "http://0.0.0.1/", true},
		// Fail closed.
		{"resolver error", "https://nxdomain.example/", true},
		{"empty answer", "https://empty.example/", true},
		{"unparseable answer", "https://garbage.example/", true},
		{"empty host", "http:///path", true},
		// Public must stay allowed.
		{"public name", "https://public.example/path", false},
		{"public name with port", "https://public.example:8443/path", false},
		{"public name with userinfo", "https://user@public.example/", false},
		{"public v4 literal", "https://93.184.216.34/", false},
		{"public v6 literal", "https://[2606:4700:4700::1111]/", false},
		{"scheme-less host:port", "public.example:8443", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HostBlocked(context.Background(), tc.raw, echoResolver); got != tc.want {
				t.Fatalf("HostBlocked(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestAddrBlockedInvalid(t *testing.T) {
	if !AddrBlocked(netip.Addr{}) {
		t.Fatal("the zero Addr must be blocked")
	}
}

func TestExtractHost(t *testing.T) {
	for raw, want := range map[string]string{
		"https://Example.COM:8443/a?b#c":     "example.com",
		"http://u:p@host.example/":           "host.example",
		"http://[::1]:80/":                   "::1",
		"http://[fe80::1%25eth0]/":           "fe80::1",
		"host.example.":                      "host.example",
		"ws://a@b@127.0.0.1/":                "127.0.0.1",
		strings.Repeat("a", 3) + ".example/": "aaa.example",
	} {
		if got := ExtractHost(raw); got != want {
			t.Errorf("ExtractHost(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestHostBlockedCGNATOptIn(t *testing.T) {
	t.Setenv(AllowCGNATEnv, "true")
	ctx := context.Background()
	if HostBlocked(ctx, "http://100.64.0.1/", echoResolver) {
		t.Error("CGNAT literal must be allowed when the operator opts in")
	}
	if HostBlocked(ctx, "https://cgnat.example/", echoResolver) {
		t.Error("a name resolving to CGNAT must be allowed when the operator opts in")
	}
	for _, raw := range []string{"http://10.0.0.1/", "http://127.0.0.1/", "http://169.254.169.254/", "http://[fd00::1]/", "https://ula.example/"} {
		if !HostBlocked(ctx, raw, echoResolver) {
			t.Errorf("%s must stay blocked with the CGNAT opt-in", raw)
		}
	}
	if !AddrBlocked(netip.MustParseAddr("100.64.0.1")) {
		t.Error("AddrBlocked (link fetch, proxy-grade) must not be affected by the opt-in")
	}
	t.Setenv(AllowCGNATEnv, "yes")
	if !HostBlocked(ctx, "http://100.64.0.1/", echoResolver) {
		t.Error("only the exact string true enables the opt-in")
	}
}
