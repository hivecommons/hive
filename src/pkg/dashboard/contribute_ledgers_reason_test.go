package dashboard

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestVerdictLedgerReasonTruncation(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty"},
		{name: "audit text unchanged", in: " \n`@reporter` <!-- marker --> §€😀 ", want: " \n`@reporter` <!-- marker --> §€😀 "},
		{name: "ascii at cap", in: strings.Repeat("a", noWorkReasonMaxLen), want: strings.Repeat("a", noWorkReasonMaxLen)},
		{name: "ascii over cap", in: strings.Repeat("a", noWorkReasonMaxLen+1), want: strings.Repeat("a", noWorkReasonMaxLen)},
	}
	for _, r := range []string{"§", "€", "😀"} {
		// Exercise every possible byte boundary inside a multibyte rune,
		// plus the boundary immediately after it.
		for retained := 1; retained <= len(r); retained++ {
			prefix := strings.Repeat("a", noWorkReasonMaxLen-retained)
			want := prefix
			if retained == len(r) {
				want += r
			}
			cases = append(cases, struct {
				name string
				in   string
				want string
			}{
				name: fmt.Sprintf("%s/%d bytes before cap", r, retained),
				in:   prefix + r + "tail",
				want: want,
			})
		}
	}

	for _, path := range []string{"already_done_unverified", completionVerdictBlocked, completionVerdictNeedsDecision, completionVerdictNoWorkNeeded} {
		t.Run(path, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					hub := ledgerHub(t.TempDir())
					if path == "already_done_unverified" {
						hub.markAlreadyDoneUnverified("o/r", 13110, "reporter", tc.in, nil)
					} else {
						hub.markTaskCompletedVerdict("o/r", 13110, "", path, "reporter", tc.in)
					}
					const key = "o/r#13110"
					assertReason := func(where string, records map[string]noWorkVerdictRecord) {
						t.Helper()
						rec, ok := records[key]
						if !ok {
							t.Fatalf("%s: verdict was not recorded", where)
						}
						if !utf8.ValidString(rec.Reason) {
							t.Errorf("%s: reason contains a split rune: %q", where, rec.Reason)
						}
						if len(rec.Reason) > noWorkReasonMaxLen {
							t.Errorf("%s: reason has %d bytes, cap is %d", where, len(rec.Reason), noWorkReasonMaxLen)
						}
						if rec.Reason != tc.want {
							t.Errorf("%s: reason = %q, want %q", where, rec.Reason, tc.want)
						}
					}
					assertReason("in memory", hub.noWorkVerdicts)

					// A JSON round trip must not replace a split rune with U+FFFD.
					hub.noWorkVerdicts = make(map[string]noWorkVerdictRecord)
					hub.loadNoWorkVerdicts()
					assertReason("after reload", hub.noWorkVerdicts)
				})
			}
		})
	}
}
