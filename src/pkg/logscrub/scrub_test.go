package logscrub

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestScrubStringCredentialPatterns(t *testing.T) {
	cases := []string{
		"github ghp_abcdefghijklmnopqrstuvwxyz123456",
		"oauth gho_abcdefghijklmnopqrstuvwxyz123456",
		"oauth underscore gho_ab_cdEF1234",
		"server ghs_abcdefghijklmnopqrstuvwxyz123456",
		"jwt eyJaaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbbb.cccccccccccccccccccc",
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

func TestRelayAndGoSecretPatternCategoriesAgree(t *testing.T) {
	const relayPath = "../../../bin/contributor-relay.js"
	body, err := os.ReadFile(relayPath)
	if err != nil {
		t.Fatalf("read relay secret patterns from %s: %v", relayPath, err)
	}

	blockRE := regexp.MustCompile(`(?s)const RELAY_SECRET_PATTERNS = \[(.*?)\n\];`)
	block := blockRE.FindSubmatch(body)
	if block == nil {
		t.Fatalf("RELAY_SECRET_PATTERNS declaration not found in %s", relayPath)
	}
	categoryRE := regexp.MustCompile(`category:\s*'([^']+)'`)
	relayMatches := categoryRE.FindAllSubmatch(block[1], -1)
	if len(relayMatches) == 0 {
		t.Fatalf("RELAY_SECRET_PATTERNS in %s contains no named categories", relayPath)
	}

	goSet := make(map[string]bool, len(secretPatterns))
	var problems []string
	for _, pattern := range secretPatterns {
		if goSet[pattern.category] {
			problems = append(problems, "duplicate pkg/logscrub category "+pattern.category)
		}
		goSet[pattern.category] = true
	}
	relaySet := make(map[string]bool, len(relayMatches))
	for _, match := range relayMatches {
		category := string(match[1])
		if relaySet[category] {
			problems = append(problems, "duplicate relay category "+category)
		}
		relaySet[category] = true
	}

	for category := range goSet {
		if !relaySet[category] {
			problems = append(problems, "pkg/logscrub has category "+category+" but the relay does not")
		}
	}
	for category := range relaySet {
		if !goSet[category] {
			problems = append(problems, "relay has category "+category+" but pkg/logscrub does not")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("relay and Go secret-pattern categories have drifted:\n  %s", strings.Join(problems, "\n  "))
	}
}
