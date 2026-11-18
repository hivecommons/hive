package hub

import (
	"context"
	"testing"
)

// TestIsPrivateURLSharedClassifier pins isPrivateURL to the shared netguard
// classifier (#18991). The resolver answers a public address for every name it
// is not told otherwise, so a literal spelling the old string-prefix list did
// not recognise (it handed it to the resolver as a name) would be classified
// public, and every resolved answer outside the old textual prefixes (CGNAT,
// fc00::/7, fe80::/10, bare ::1, mapped, NAT64, 6to4, multicast) would pass.
func TestIsPrivateURLSharedClassifier(t *testing.T) {
	answers := map[string][]string{
		"bare-v6.example": {"::1"},
		"cgnat.example":   {"100.64.0.1"},
		"ula.example":     {"fd00::1"},
		"ll6.example":     {"fe80::1"},
		"mapped.example":  {"::ffff:10.0.0.1"},
		"nat64.example":   {"64:ff9b::a9fe:a9fe"},
		"6to4.example":    {"2002:a00:1::"},
		"mcast.example":   {"ff05::2"},
		"mixed.example":   {"93.184.216.34", "fd12::1"},
	}
	orig := privateURLResolver
	t.Cleanup(func() { privateURLResolver = orig })
	privateURLResolver = func(_ context.Context, host string) ([]string, error) {
		if a, ok := answers[host]; ok {
			return a, nil
		}
		return []string{"93.184.216.34"}, nil
	}

	ctx := context.Background()
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		// Resolved answers.
		{"https://public.example/path", false},
		{"https://bare-v6.example", true},
		{"https://cgnat.example", true},
		{"https://ula.example", true},
		{"https://ll6.example", true},
		{"https://mapped.example", true},
		{"https://nat64.example", true},
		{"https://6to4.example", true},
		{"https://mcast.example", true},
		{"https://mixed.example", true},
		// Literal spellings.
		{"http://2130706433/", true},
		{"http://0x7f000001/", true},
		{"http://0x7f.1/", true},
		{"http://0177.0.0.1/", true},
		{"http://127.1/", true},
		{"http://127.0.0.1./", true},
		{"HTTP://127.0.0.1/", true},
		{"http://0xa9.0xfe.0xa9.0xfe/latest", true},
		{"http://user@169.254.169.254/", true},
		{"http://100.64.0.1/", true},
		{"http://[fc00::1]:8080/", true},
		{"http://[fe80::1%25eth0]/", true},
		{"http://[::ffff:7f00:1]/", true},
		{"http://[64:ff9b::7f00:1]/", true},
		{"http://[2002:a00:1::]/", true},
		{"http://[ff02::1]/", true},
		{"https://8.8.8.8/", false},
		{"https://[2606:4700::1111]/", false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := isPrivateURL(ctx, tc.raw); got != tc.want {
				t.Fatalf("isPrivateURL(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
