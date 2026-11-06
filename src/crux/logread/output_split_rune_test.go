package logread

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// A line that never ends is stored in records of at most outputLineMaxBytes.
// The split was made at exactly that many bytes, so a multi-byte character
// that straddled it was cut in half: each half was stored as U+FFFD and the
// character was lost.
func TestOutputLineSplitKeepsMultiByteCharacters(t *testing.T) {
	for _, ch := range []string{"é", "€", "😀"} {
		for lead := outputLineMaxBytes - 4; lead <= outputLineMaxBytes; lead++ {
			dir := t.TempDir()
			log := openTestOutput(t, dir, SourceProxy, &fakeClock{t: base})
			w := log.Stream(StreamStdout)
			line := strings.Repeat("a", lead) + ch + strings.Repeat("b", 10)
			// The line arrives in two writes, the first without its newline,
			// as a program flushing a long line through a pipe sends it.
			_, _ = w.Write([]byte(line))
			_, _ = w.Write([]byte(" end\n"))
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			recs := decodeRecords(t, filepath.Join(dir, "proxy.log"))
			if len(recs) != 2 {
				t.Fatalf("%q after %d bytes: %d records, want 2", ch, lead, len(recs))
			}
			if got := recs[0].Text + recs[1].Text; got != line+" end" {
				t.Errorf("%q after %d bytes: the stored text differs from what was written near the split: %q", ch, lead, got[max(lead-2, 0):min(lead+8, len(got))])
			}
			if len(recs[0].Text) > outputLineMaxBytes {
				t.Errorf("%q after %d bytes: first record holds %d bytes of text, over the cap", ch, lead, len(recs[0].Text))
			}
			for i, r := range recs {
				if !utf8.ValidString(r.Text) || strings.ContainsRune(r.Text, utf8.RuneError) {
					t.Errorf("%q after %d bytes: record %d holds a broken character", ch, lead, i)
				}
			}
		}
	}
}

func TestRuneSafeCut(t *testing.T) {
	for name, tc := range map[string]struct {
		data string
		n    int
		want int
	}{
		"ascii":                        {"abcdef", 4, 4},
		"two-byte char ends at cut":    {"abé", 4, 4},
		"two-byte char straddles":      {"abéz", 3, 2},
		"three-byte char straddles":    {"ab€z", 4, 2},
		"four-byte char, one byte in":  {"ab😀z", 3, 2},
		"four-byte char, three in":     {"ab😀z", 5, 2},
		"four-byte char ends at cut":   {"ab😀z", 6, 6},
		"not UTF-8, cut where asked":   {"ab\x80\x80\x80\x80\x80", 5, 5},
		"lone start byte before cut":   {"abc\xe2", 4, 3},
		"cut at the end of everything": {"abé", 4, 4},
	} {
		if got := runeSafeCut([]byte(tc.data), tc.n); got != tc.want {
			t.Errorf("%s: runeSafeCut(%q, %d) = %d, want %d", name, tc.data, tc.n, got, tc.want)
		}
	}
}
