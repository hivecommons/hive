package logread

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestScrubTextScrubsLikeLogLines(t *testing.T) {
	own := "own-secret-value-0123456789"
	ghToken := "ghp_" + strings.Repeat("a", 36)
	key := strings.Join([]string{"-----BEGIN PRIVATE KEY-----", strings.Repeat("A", 40), strings.Repeat("B", 40), "-----END PRIVATE KEY-----"}, "\n")
	text := strings.Join([]string{"start", "used " + own, "pushed with " + ghToken, key, "already <redacted:jwt> here", "end"}, "\n")
	got := ScrubText(text, NewSecretScrubber([]string{own}))
	for _, leaked := range []string{own, ghToken, strings.Repeat("A", 40), strings.Repeat("B", 40)} {
		if strings.Contains(got.Text, leaked) {
			t.Fatalf("scrubbed text kept %q: %s", leaked, got.Text)
		}
	}
	if !strings.HasPrefix(got.Text, "start\n") || !strings.HasSuffix(got.Text, "\nend") {
		t.Fatalf("line structure changed: %q", got.Text)
	}
	if got.Counts[SecretValueKind] != 1 || got.Counts["github-token"] != 1 || got.Counts[PrivateKeyKind] == 0 {
		t.Fatalf("counts = %v", got.Counts)
	}
	if got.PreExistingMarkerLines != 1 {
		t.Fatalf("pre-existing marker lines = %d", got.PreExistingMarkerLines)
	}
}

func TestScrubTextKeyCutByWindow(t *testing.T) {
	// The window starts inside a key: an END with no BEGIN before it means
	// every earlier line is key material.
	text := strings.Repeat("C", 40) + "\n" + strings.Repeat("D", 40) + "\n-----END PRIVATE KEY-----\nafter"
	got := ScrubText(text, nil)
	if strings.Contains(got.Text, strings.Repeat("C", 40)) || strings.Contains(got.Text, strings.Repeat("D", 40)) || !strings.HasSuffix(got.Text, "after") {
		t.Fatalf("key body cut by the window survived: %q", got.Text)
	}
	// A BEGIN with no END is redacted to the end of the block.
	got = ScrubText("before\n-----BEGIN PRIVATE KEY-----\n"+strings.Repeat("E", 40), nil)
	if strings.Contains(got.Text, strings.Repeat("E", 40)) || !strings.HasPrefix(got.Text, "before\n") {
		t.Fatalf("unterminated key survived: %q", got.Text)
	}
}

func TestRedactionList(t *testing.T) {
	if got := RedactionList(nil); got == nil || len(got) != 0 {
		t.Fatalf("empty list = %#v", got)
	}
	got := RedactionList(map[string]int{"jwt": 2, "api-key": 1, "none": 0})
	if len(got) != 2 || got[0].Kind != "api-key" || got[1].Kind != "jwt" || got[1].Count != 2 {
		t.Fatalf("list = %+v", got)
	}
}

func TestHeadText(t *testing.T) {
	if HeadText("abc", 0) != "" || HeadText("abc", 5) != "abc" || HeadText("abcdef", 3) != "abc" {
		t.Fatal("HeadText byte bounds")
	}
	if got := HeadText("aé", 2); got != "a" || !utf8.ValidString(got) {
		t.Fatalf("HeadText split a rune: %q", got)
	}
}

func TestTailText(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		n        int
		want     string
		cut      bool
	}{
		{"fits", "abc", 3, "abc", false},
		{"zero", "abc", 0, "", true},
		{"empty zero", "", 0, "", false},
		{"line aligned", "one\ntwo\nthree", 9, "two\nthree", true},
		{"skips partial line", "first line\nsecond line\nthird", 20, "second line\nthird", true},
		{"no break keeps bytes", "abcdefghij", 4, "ghij", true},
		{"late break kept", "xxxxxxxxxx\nabcdefgh\nab", 6, "fgh\nab", true},
		{"rune boundary", "ééé", 3, "é", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, cut := TailText(tc.in, tc.n)
			if got != tc.want || cut != tc.cut || !utf8.ValidString(got) || len(got) > tc.n {
				t.Fatalf("TailText(%q, %d) = %q, %v; want %q, %v", tc.in, tc.n, got, cut, tc.want, tc.cut)
			}
		})
	}
}
