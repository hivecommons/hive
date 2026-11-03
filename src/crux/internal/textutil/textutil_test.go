package textutil

import "testing"

func TestTruncate(t *testing.T) {
	cases := []struct {
		name              string
		in                string
		n                 int
		head, tail, runes string
	}{
		{"ascii longer", "abcdef", 3, "abc...", "...def", "abc"},
		{"ascii exact", "abc", 3, "abc", "abc", "abc"},
		{"ascii shorter", "ab", 3, "ab", "ab", "ab"},
		{"multibyte longer", "héllo wörld", 5, "héllo...", "...wörld", "héllo"},
		{"multibyte exact", "日本語", 3, "日本語", "日本語", "日本語"},
		{"multibyte cut", "日本語です", 2, "日本...", "...です", "日本"},
		{"zero", "abc", 0, "...", "...", "abc"},
		{"empty", "", 3, "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Head(c.in, c.n); got != c.head {
				t.Errorf("Head = %q, want %q", got, c.head)
			}
			if got := Tail(c.in, c.n); got != c.tail {
				t.Errorf("Tail = %q, want %q", got, c.tail)
			}
			if got := Runes(c.in, c.n); got != c.runes {
				t.Errorf("Runes = %q, want %q", got, c.runes)
			}
		})
	}
}

func TestRunesNonPositive(t *testing.T) {
	for _, n := range []int{0, -1} {
		if got := Runes("abc", n); got != "abc" {
			t.Errorf("Runes(max=%d) = %q, want input", n, got)
		}
	}
}
