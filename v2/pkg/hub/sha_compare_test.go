package hub

import "testing"

func TestSameCommit(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"39a095b9455f", "39a095b", true}, // full vs short prefix — the David bug
		{"39a095b", "39a095b9455f", true}, // short vs full
		{"39a095b9455f", "39a095b9455f", true},
		{"39a095b", "39a095b", true},
		{"39A095B", "39a095b9455f", true}, // case-insensitive
		{"abc1234", "def5678", false},
		{"39a095b", "39a0000", false}, // same length, differ
		{"", "39a095b", false},        // empty never equal
		{"39a095b", "", false},
	}
	for _, c := range cases {
		if got := sameCommit(c.a, c.b); got != c.want {
			t.Errorf("sameCommit(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestShortSHA(t *testing.T) {
	cases := []struct{ in, want string }{
		{"03c0fc4b671a", "03c0fc4b"[:7]}, // 12 → 7
		{"03c0fc4b671a", "03c0fc4"},      // explicit
		{"39a095b", "39a095b"},           // already 7
		{"abc", "abc"},                   // shorter, unchanged
		{"", ""},                         // empty
		{"  03c0fc4b671a  ", "03c0fc4"},  // trimmed then truncated
	}
	for _, c := range cases {
		if got := shortSHA(c.in); got != c.want {
			t.Errorf("shortSHA(%q)=%q want %q", c.in, got, c.want)
		}
	}
}
