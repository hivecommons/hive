package netguard

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

func TestDenyReason(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want string
	}{
		{"127.0.0.1", ReasonLoopback},
		{"::1", ReasonLoopback},
		{"10.0.0.1", ReasonPrivate},
		{"172.16.0.1", ReasonPrivate},
		{"172.31.255.255", ReasonPrivate},
		{"192.168.1.1", ReasonPrivate},
		{"169.254.169.254", ReasonLinkLocal},
		{"100.64.0.1", ReasonCGNAT},
		{"100.127.255.255", ReasonCGNAT},
		{"fc00::1", ReasonPrivate},
		{"fd00:ec2::254", ReasonPrivate},
		{"fe80::1", ReasonLinkLocal},
		{"fe80::1%eth0", ReasonLinkLocal},
		{"::ffff:127.0.0.1", ReasonLoopback},
		{"::ffff:10.0.0.1", ReasonPrivate},
		{"64:ff9b::a9fe:a9fe", ReasonLinkLocal}, // NAT64 of 169.254.169.254
		{"64:ff9b::7f00:1", ReasonLoopback},     // NAT64 of 127.0.0.1
		{"2002:a00:1::", ReasonPrivate},         // 6to4 of 10.0.0.1
		{"2002:7f00:1::1", ReasonLoopback},      // 6to4 of 127.0.0.1
		{"224.0.0.1", ReasonLinkLocal},          // 224.0.0.0/24 is link-local multicast
		{"239.255.255.250", ReasonMulticast},
		{"ff02::1", ReasonLinkLocal},
		{"ff05::2", ReasonMulticast},
		{"0.0.0.0", ReasonUnspecified},
		{"::", ReasonUnspecified},
		{"0.1.2.3", ReasonThisNetwork},
		{"255.255.255.255", ReasonBroadcast},
		// Globally routable.
		{"8.8.8.8", ""},
		{"93.184.216.34", ""},
		{"203.0.113.10", ""}, // test helpers resolve public names here
		{"100.128.0.1", ""},  // just outside CGNAT
		{"172.32.0.1", ""},   // just outside 172.16/12
		{"2606:4700::1111", ""},
		{"::ffff:8.8.8.8", ""},
		{"64:ff9b::808:808", ""},
		{"2002:808:808::", ""},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			if got := DenyReason(netip.MustParseAddr(tc.addr)); got != tc.want {
				t.Fatalf("DenyReason(%s) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
	if DenyReason(netip.Addr{}) == "" {
		t.Fatal("the zero Addr must be refused")
	}
}

func TestParseHostLiteral(t *testing.T) {
	for _, tc := range []struct {
		host string
		want string // "" means not a literal
	}{
		{"127.0.0.1", "127.0.0.1"},
		{"2130706433", "127.0.0.1"},
		{"0x7f000001", "127.0.0.1"},
		{"0X7F000001", "127.0.0.1"},
		{"0x7f.1", "127.0.0.1"},
		{"0177.0.0.1", "127.0.0.1"},
		{"0177.0.0.01", "127.0.0.1"},
		{"127.1", "127.0.0.1"},
		{"127.0.1", "127.0.0.1"},
		{"127.0.0.1.", "127.0.0.1"},
		{"10.0.0.010", "10.0.0.8"},
		{"0xa9.0xfe.0xa9.0xfe", "169.254.169.254"},
		{"025177524776", "169.254.169.254"},
		{"[::1]", "::1"},
		{"::1", "::1"},
		{"[::FFFF:127.0.0.1]", "::ffff:127.0.0.1"},
		{"[fe80::1%25eth0]", "fe80::1"},
		{"fe80::1%eth0", "fe80::1"},
		{"example.com", ""},
		{"0xdead.com", ""},
		{"1.2.3.4.5", ""},
		{"256.0.0.1", ""},
		{"4294967296", ""},
		{"08.0.0.1", ""},
		{"127..1", ""},
		{"", ""},
	} {
		t.Run(tc.host, func(t *testing.T) {
			got, ok := ParseHostLiteral(tc.host)
			if tc.want == "" {
				if ok {
					t.Fatalf("ParseHostLiteral(%q) = %s, want not a literal", tc.host, got)
				}
				return
			}
			if !ok || got != netip.MustParseAddr(tc.want) {
				t.Fatalf("ParseHostLiteral(%q) = %s, %v, want %s", tc.host, got, ok, tc.want)
			}
		})
	}
}

func TestURLHost(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://Example.COM:443/x", "example.com"},
		{"http://user:pw@10.0.0.1/", "10.0.0.1"},
		{"http://127.0.0.1:80@evil.example/", "evil.example"},
		{"http://[::1]:8080/", "::1"},
		{"http://[fe80::1%25eth0]/", "fe80::1%eth0"},
		{"example.com:8080/path", "example.com"},
		{"wss://host.example./ws", "host.example"},
		{"HTTP://127.0.0.1", "127.0.0.1"},
		{"http://host.example?q=1", "host.example"},
		{"http://host.example#frag", "host.example"},
	} {
		if got := URLHost(tc.raw); got != tc.want {
			t.Errorf("URLHost(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestIsPrivateURL(t *testing.T) {
	resolveTo := map[string][]string{
		"public.example":  {"203.0.113.10"},
		"bare-v6.example": {"::1"},
		"cgnat.example":   {"100.64.0.1"},
		"ula.example":     {"fd00::1"},
		"ll6.example":     {"fe80::1"},
		"mapped.example":  {"::ffff:10.0.0.1"},
		"nat64.example":   {"64:ff9b::a9fe:a9fe"},
		"6to4.example":    {"2002:a00:1::"},
		"mcast.example":   {"ff02::1"},
		"mixed.example":   {"203.0.113.10", "10.0.0.1"},
		"garbage.example": {"not-an-ip"},
		"empty.example":   {},
	}
	var lookups []string
	resolve := func(_ context.Context, host string) ([]string, error) {
		lookups = append(lookups, host)
		if a, ok := resolveTo[host]; ok {
			return a, nil
		}
		return nil, errors.New("no such host")
	}
	ctx := context.Background()
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"https://public.example/path", false},
		{"https://8.8.8.8", false},
		{"https://bare-v6.example", true},
		{"https://cgnat.example", true},
		{"https://ula.example", true},
		{"https://ll6.example", true},
		{"https://mapped.example", true},
		{"https://nat64.example", true},
		{"https://6to4.example", true},
		{"https://mcast.example", true},
		{"https://mixed.example", true},
		{"https://garbage.example", true},
		{"https://empty.example", true},
		{"https://missing.example", true},
		{"http://localhost:8080", true},
		{"http://app.localhost", true},
		{"http://2130706433/", true},
		{"http://0x7f.1/", true},
		{"http://0177.0.0.1/", true},
		{"http://127.1/", true},
		{"http://127.0.0.1./", true},
		{"http://0x0a.0.0.1/", true},
		{"http://user@169.254.169.254/", true},
		{"http://[fe80::1%25eth0]/", true},
		{"http://[::ffff:7f00:1]/", true},
		{"http://[fc00::1]:8080/", true},
		{"http://100.64.0.1/", true},
		{"http://0.1.2.3/", true},
		{"", true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			lookups = nil
			if got := IsPrivateURL(ctx, tc.raw, resolve); got != tc.want {
				t.Fatalf("IsPrivateURL(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}

	// A literal is classified on its own: no lookup is made for it.
	lookups = nil
	IsPrivateURL(ctx, "http://2130706433/", resolve)
	IsPrivateURL(ctx, "https://8.8.8.8/", resolve)
	if len(lookups) != 0 {
		t.Fatalf("literal hosts were resolved: %v", lookups)
	}

	if !IsPrivateURL(ctx, "https://public.example", nil) {
		t.Fatal("a nil resolver must fail closed")
	}
}
