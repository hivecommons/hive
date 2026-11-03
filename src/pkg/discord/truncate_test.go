package discord

import "testing"

func TestTruncateRunesRetainsCompletionTextBound(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		limit            int
	}{
		{name: "nonpositive", text: "日本語", limit: 0},
		{name: "negative", text: "日本語", limit: -1},
		{name: "unicode", text: "日本語", limit: 2, want: "日本"},
		{name: "exact", text: "日本語", limit: 3, want: "日本語"},
		{name: "short", text: "日", limit: 3, want: "日"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateRunes(tc.text, tc.limit); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
