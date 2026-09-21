package logscrub

import "testing"

func TestFindRedactionMarkerRecognizesMaskedText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"go placeholder", "header = [REDACTED]", "[REDACTED]"},
		{"launch script jwt placeholder", "token=[REDACTED-JWT] end", "[REDACTED-JWT]"},
		{"categorized placeholder", "value [REDACTED:bearer-token] here", "[REDACTED:bearer-token]"},
		{"angle marker", "Authorization: <redacted>", "<redacted>"},
		{"categorized angle marker", "Authorization: <redacted:bearer-token>", "<redacted:bearer-token>"},
		{"starred marker", "gho_***REDACTED***", "***REDACTED***"},
		{"lowercase", "auth [redacted]", "[redacted]"},
		// The shape from hivecommons/hive#8067: the reviewer read this and
		// reported that the format string had no %s.
		{"opaque mask after colon", `printf 'header = "Authorization: ******"\n'`, ": ******"},
		{"opaque mask after equals", "token=********", "=********"},
		{"opaque mask opening a quoted value", `{"authorization":"******"}`, `"******`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FindRedactionMarker(tc.in); got != tc.want {
				t.Fatalf("FindRedactionMarker(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !ContainsRedactionMarker(tc.in) {
				t.Fatalf("ContainsRedactionMarker(%q) = false, want true", tc.in)
			}
		})
	}
}

func TestFindRedactionMarkerLeavesRealCodeAlone(t *testing.T) {
	// A false positive here suppresses a legitimate review, so the recognizer
	// must stay quiet on code that merely looks starred or merely says the
	// word in prose.
	cases := []string{
		`stdin_config="$(printf 'header = "Authorization: Bearer %s"\n' "$TOKEN")"`,
		"/**************** section banner ****************/",
		"cp ./*.go /tmp/ && ls **/*.md",
		"x := a ** b ** c",
		"the relay redacted the token before forwarding it",
		"See docs/security.md for what redaction covers.",
		`{"authorization":"Bearer not-a-real-key"}`,
		"",
	}
	for _, in := range cases {
		if got := FindRedactionMarker(in); got != "" {
			t.Errorf("FindRedactionMarker(%q) = %q, want no marker", in, got)
		}
	}
}

// TestBearerPatternIgnoresPlaceholders pins the third fix proposed on
// hivecommons/hive#8067: a format placeholder or a shell/template variable is
// never a secret, so the bearer rule must not mask one. Today's `{16,}`
// character class already excludes them — this test is what keeps a future
// widening of that rule from re-introducing the false positive that made a
// reviewer read `Authorization: ******` in place of `Authorization: Bearer %s`.
func TestBearerPatternIgnoresPlaceholders(t *testing.T) {
	cases := []string{
		`printf 'header = "Authorization: Bearer %s"\n' "$TOKEN"`,
		`fmt.Sprintf("Authorization: Bearer %q", tok)`,
		`Authorization: Bearer $LLMMAN_TOKEN_VALUE`,
		`Authorization: Bearer ${LLMMAN_TOKEN_VALUE}`,
		`Authorization: Bearer {{ .Token }}`,
		`Authorization: Bearer <your-token-here>`,
	}
	for _, in := range cases {
		if got := ScrubString(in); got != in {
			t.Errorf("ScrubString(%q) = %q, want it unchanged: a placeholder is not a secret", in, got)
		}
	}
}
