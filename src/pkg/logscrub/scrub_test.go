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

func TestScrubStringMarkedMode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "github token",
			in:   "token ghs_abcdefghijklmnopqrstuvwxyz123456",
			want: "token <redacted:github-token>",
		},
		{
			name: "bearer token",
			in:   "header Authorization: Bearer test-token-1234567890",
			want: "header Authorization: <redacted:bearer-token>",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := ScrubString(tt.in, WithMarkers()); got != tt.want {
				t.Fatalf("ScrubString marked = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBearerPatternKeepsExistingTokenCharacterCoverage(t *testing.T) {
	cases := []string{
		"Authorization: Bearer _abcdefghijklmnop",
		"Authorization: Bearer -abcdefghijklmnop",
		"Authorization: Bearer .abcdefghijklmnop",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if got := ScrubString(in, WithMarkers()); got != "Authorization: <redacted:bearer-token>" {
				t.Fatalf("ScrubString marked = %q, want bearer marker", got)
			}
		})
	}
}

func TestScrubStringDefaultKeepsLogRedactionShape(t *testing.T) {
	in := "header Authorization: Bearer test-token-1234567890"
	want := "header Authorization: " + redacted
	if got := ScrubString(in); got != want {
		t.Fatalf("ScrubString default = %q, want %q", got, want)
	}
}

func TestBearerPatternLeavesPlaceholdersUnchanged(t *testing.T) {
	cases := []string{
		`stdin_config="$(printf 'header = "Authorization: Bearer %s"\n' "$LLMMAN_TOKEN_VALUE")"`,
		`header = "Authorization: Bearer %q"`,
		`header = "Authorization: Bearer %v"`,
		`header = "Authorization: Bearer $TOKEN"`,
		`header = "Authorization: Bearer ${TOKEN}"`,
		`header = "Authorization: Bearer {{ .Token }}"`,
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if got := ScrubString(in); got != in {
				t.Fatalf("ScrubString default altered placeholder: got %q", got)
			}
			if got := ScrubString(in, WithMarkers()); got != in {
				t.Fatalf("ScrubString marked altered placeholder: got %q", got)
			}
		})
	}
}

func TestBearerRegressionMarkedMode(t *testing.T) {
	formatLine := `stdin_config="$(printf 'header = "Authorization: Bearer %s"\n' "$LLMMAN_TOKEN_VALUE")"`
	if got := ScrubString(formatLine, WithMarkers()); got != formatLine {
		t.Fatalf("format placeholder line was scrubbed: %q", got)
	}

	fixtureLine := `grep -q 'Authorization: Bearer test-token-1234567890' curl.stdin`
	want := `grep -q 'Authorization: <redacted:bearer-token>' curl.stdin`
	if got := ScrubString(fixtureLine, WithMarkers()); got != want {
		t.Fatalf("fixture bearer token marked = %q, want %q", got, want)
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
