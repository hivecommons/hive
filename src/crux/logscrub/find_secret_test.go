package logscrub

import (
	"strings"
	"testing"
)

// Fixture values are assembled at runtime so the literal strings never
// resemble a real credential to repository scanners.
func TestFindSecretReportsExactShapeCategories(t *testing.T) {
	cases := []struct {
		name, text, category string
	}{
		{"hive-canary", "marker HIVE-CANARY-" + strings.Repeat("0a", 24), "hive-canary"},
		{"github-token", "token gh" + "p_abcdefghijklmnopqrstuvwxyz", "github-token"},
		{"jwt", "jwt eyJaaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbbb.cccccccccccccccccccc", "jwt"},
		{"aws-access-key", "key AKIA" + "IOSFODNN7EXAMPLE", "aws-access-key"},
		{"slack-webhook-url", "hook " + fakeSlackWebhookURL, "slack-webhook-url"},
		{"discord-webhook-url", "hook https://discord.com/api/webhooks/123456789/abcDEF_ghi-JKL", "discord-webhook-url"},
		{"slack-token", "token " + fakeSlackBotToken, "slack-token"},
		{"slack-app-token", "token " + fakeSlackAppToken, "slack-app-token"},
		{"private-key", "-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----", "private-key"},
		{"encrypted-private-key", "-----BEGIN ENCRYPTED PRIVATE KEY-----\nMIIE\n-----END ENCRYPTED PRIVATE KEY-----", "encrypted-private-key"},
		{"pgp-private-key", "-----BEGIN PGP PRIVATE KEY BLOCK-----\nlQ\n-----END PGP PRIVATE KEY BLOCK-----", "pgp-private-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			category, found := FindSecret(tc.text)
			if !found || category != tc.category {
				t.Fatalf("FindSecret(%q) = (%q, %v), want (%q, true)", tc.text, category, found, tc.category)
			}
		})
	}
}

func TestFindSecretSkipsHeuristicShapesAndPlaceholders(t *testing.T) {
	cases := []struct{ name, text string }{
		{"plain prose", "nothing to see here"},
		{"api-key is heuristic", "key sk-test-fixture-key-1234"},
		{"bearer is heuristic", "Authorization: Bearer fixturetoken1234567890"},
		{"basic-auth is heuristic", `authorization: Basic dXNlcjpwYXNz`},
		{"slack token without a digit is skipped", "token xox" + "b-abcdefghijklmnop"},
		{"canary too short", "HIVE-CANARY-" + strings.Repeat("0a", 10)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if category, found := FindSecret(tc.text); found {
				t.Fatalf("FindSecret(%q) = (%q, true), want not found", tc.text, category)
			}
		})
	}
}

// A skipped match must not end the search for its pattern: a digitless
// placeholder earlier in the text (README prose, a fixture) must not hide a
// real token of the same shape that follows it.
func TestFindSecretSkippedMatchDoesNotHideLaterMatch(t *testing.T) {
	text := "example xox" + "b-abcdefghijklmnop then real " + fakeSlackBotToken
	category, found := FindSecret(text)
	if !found || category != "slack-token" {
		t.Fatalf("FindSecret(%q) = (%q, %v), want (\"slack-token\", true)", text, category, found)
	}
}

// Every heuristic pattern FindSecret skips must still be masked by
// ScrubString, so marking a shape heuristic never weakens log scrubbing.
func TestHeuristicShapesStillScrubbed(t *testing.T) {
	for _, p := range secretPatterns {
		if !p.heuristic {
			continue
		}
		var sample string
		switch p.category {
		case "api-key":
			sample = "key sk-abcdefghijklmnop"
		case "bearer-token":
			sample = "Authorization: Bearer abcdefghijklmnopqrstuvwxyz"
		case "basic-auth":
			sample = "authorization: Basic dXNlcjpwYXNz"
		default:
			t.Fatalf("heuristic pattern %q has no sample in this test; add one", p.category)
		}
		if got := ScrubString(sample); !strings.Contains(got, redacted) {
			t.Fatalf("ScrubString(%q) = %q, want %s", sample, got, redacted)
		}
	}
}
