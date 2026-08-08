package logscrub

import (
	"strings"
	"testing"
)

func TestScrubStringCredentialPatterns(t *testing.T) {
	cases := []string{
		"github ghp_abcdefghijklmnopqrstuvwxyz123456",
		"oauth gho_abcdefghijklmnopqrstuvwxyz123456",
		"server ghs_abcdefghijklmnopqrstuvwxyz123456",
		"aws AKIA1234567890ABCDEF",
		"auth Bearer abcdefghijklmnopqrstuvwxyz0123456789",
		"pem -----BEGIN PRIVATE KEY-----\nabc123\n-----END PRIVATE KEY-----",
		"encrypted -----BEGIN ENCRYPTED PRIVATE KEY-----\nabc123\n-----END ENCRYPTED PRIVATE KEY-----",
		"pgp -----BEGIN PGP PRIVATE KEY BLOCK-----\nabc123\n-----END PGP PRIVATE KEY BLOCK-----",
		"canary HIVE-CANARY-0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	for _, in := range cases {
		out := ScrubString(in)
		if !strings.Contains(out, redacted) {
			t.Fatalf("%q was not redacted: %q", in, out)
		}
		if strings.Contains(out, "abcdefghijklmnopqrstuvwxyz123456") || strings.Contains(out, "AKIA1234567890ABCDEF") || strings.Contains(out, "abc123") {
			t.Fatalf("secret material leaked after scrub: %q", out)
		}
	}
}
