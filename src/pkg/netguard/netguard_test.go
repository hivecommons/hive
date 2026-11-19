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
		// Special-purpose ranges, IPv4 forms and the edges around them.
		{"192.0.0.0", ReasonSpecialPurpose},
		{"192.0.0.192", ReasonSpecialPurpose},
		{"192.0.0.255", ReasonSpecialPurpose},
		{"192.0.1.1", ""},
		{"192.0.2.0", ReasonSpecialPurpose},
		{"192.0.2.255", ReasonSpecialPurpose},
		{"192.0.3.1", ""},
		{"192.88.99.1", ReasonSpecialPurpose},
		{"192.88.98.255", ""},
		{"192.88.100.0", ""},
		{"198.17.255.255", ""},
		{"198.18.0.1", ReasonSpecialPurpose},
		{"198.19.255.255", ReasonSpecialPurpose},
		{"198.20.0.0", ""},
		{"198.51.99.255", ""},
		{"198.51.100.1", ReasonSpecialPurpose},
		{"198.51.100.255", ReasonSpecialPurpose},
		{"198.51.101.0", ""},
		{"203.0.112.255", ""},
		{"203.0.113.0", ReasonSpecialPurpose},
		{"203.0.113.10", ReasonSpecialPurpose},
		{"203.0.114.0", ""},
		// Special-purpose ranges, IPv6 forms.
		{"100::", ReasonSpecialPurpose},
		{"100::ffff:ffff:ffff:ffff", ReasonSpecialPurpose},
		{"100:0:0:1::", ""},
		{"2001::", ReasonSpecialPurpose},
		{"2001::1", ReasonSpecialPurpose},
		{"2001:0:ffff:ffff:ffff:ffff:ffff:ffff", ReasonSpecialPurpose},
		{"2001:1::1", ""},
		{"2001:db8::", ReasonSpecialPurpose},
		{"2001:db8:ffff::1", ReasonSpecialPurpose},
		{"2001:db9::1", ""},
		{"fec0::1", ReasonSpecialPurpose},
		{"feff:ffff::1", ReasonSpecialPurpose},
		{"ff00::1", ReasonMulticast},
		// IPv4-mapped, NAT64 and 6to4 embeddings of special-purpose IPv4.
		{"::ffff:192.0.0.192", ReasonSpecialPurpose},
		{"::ffff:192.0.2.1", ReasonSpecialPurpose},
		{"::ffff:192.88.99.1", ReasonSpecialPurpose},
		{"::ffff:198.18.0.1", ReasonSpecialPurpose},
		{"::ffff:198.51.100.1", ReasonSpecialPurpose},
		{"::ffff:203.0.113.10", ReasonSpecialPurpose},
		{"64:ff9b::c000:c0", ReasonSpecialPurpose},   // NAT64 of 192.0.0.192
		{"64:ff9b::c000:201", ReasonSpecialPurpose},  // NAT64 of 192.0.2.1
		{"64:ff9b::c058:6301", ReasonSpecialPurpose}, // NAT64 of 192.88.99.1
		{"64:ff9b::c612:1", ReasonSpecialPurpose},    // NAT64 of 198.18.0.1
		{"64:ff9b::c633:6401", ReasonSpecialPurpose}, // NAT64 of 198.51.100.1
		{"64:ff9b::cb00:710a", ReasonSpecialPurpose}, // NAT64 of 203.0.113.10
		{"2002:c000:c0::", ReasonSpecialPurpose},     // 6to4 of 192.0.0.192
		{"2002:c000:201::", ReasonSpecialPurpose},    // 6to4 of 192.0.2.1
		{"2002:c058:6301::", ReasonSpecialPurpose},   // 6to4 of 192.88.99.1
		{"2002:c612:1::", ReasonSpecialPurpose},      // 6to4 of 198.18.0.1
		{"2002:c633:6401::", ReasonSpecialPurpose},   // 6to4 of 198.51.100.1
		{"2002:cb00:710a::", ReasonSpecialPurpose},   // 6to4 of 203.0.113.10
		// Globally routable.
		{"8.8.8.8", ""},
		{"93.184.216.34", ""},
		{"100.128.0.1", ""}, // just outside CGNAT
		{"172.32.0.1", ""},  // just outside 172.16/12
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
		"public.example":  {"93.184.216.34"},
		"bare-v6.example": {"::1"},
		"cgnat.example":   {"100.64.0.1"},
		"ula.example":     {"fd00::1"},
		"ll6.example":     {"fe80::1"},
		"mapped.example":  {"::ffff:10.0.0.1"},
		"nat64.example":   {"64:ff9b::a9fe:a9fe"},
		"6to4.example":    {"2002:a00:1::"},
		"mcast.example":   {"ff02::1"},
		"mixed.example":   {"93.184.216.34", "10.0.0.1"},
		"garbage.example": {"not-an-ip"},
		"empty.example":   {},
		"doc4.example":    {"198.51.100.1"},
		"doc6.example":    {"2001:db8::1"},
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
		{"http://203.0.113.10/", true},
		{"http://[2001:db8::1]/", true},
		{"http://[::ffff:198.51.100.1]/", true},
		{"http://1.2.3.4.5/", true},
		{"http://256.0.0.1/", true},
		{"http://08.0.0.1/", true},
		{"http://0x1.0x2.0x3.0x4.0x5/", true},
		{"http://doc6.example/", true},
		{"http://doc4.example/", true},
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

	// A numeric dotted host that is not an IPv4 literal is refused without a
	// lookup, and valid loose literals and names with digits still behave.
	lookups = nil
	for _, raw := range []string{"http://1.2.3.4.5/", "http://256.0.0.1/", "http://08.0.0.1/"} {
		if !IsPrivateURL(ctx, raw, resolve) {
			t.Errorf("IsPrivateURL(%q) = false, want true", raw)
		}
	}
	if len(lookups) != 0 {
		t.Fatalf("malformed numeric hosts were resolved: %v", lookups)
	}
	if reason, literal := HostDenyReason("1.2.3.4.5"); reason != ReasonNumericHost || literal {
		t.Fatalf("HostDenyReason(1.2.3.4.5) = %q, %v", reason, literal)
	}
	for _, name := range []string{"public.example", "1password.com", "a1.2b.example", "0xdead.com", "10.example.com", "127..1", "4294967296"} {
		if reason, _ := HostDenyReason(name); reason == ReasonNumericHost {
			t.Errorf("HostDenyReason(%q) refused as a numeric host", name)
		}
	}

	if !IsPrivateURL(ctx, "https://public.example", nil) {
		t.Fatal("a nil resolver must fail closed")
	}
}
