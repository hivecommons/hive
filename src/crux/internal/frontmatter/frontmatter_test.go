package frontmatter

import "testing"

func TestSplit(t *testing.T) {
	cases := []struct{ name, in, fm, rest string }{
		{"none", "# Title\nbody", "", "# Title\nbody"},
		{"block", "---\na: 1\n---\nbody", "a: 1", "body"},
		{"leading newline", "\n---\na: 1\n---\nbody", "a: 1", "body"},
		{"unclosed", "---\na: 1\nbody", "", "---\na: 1\nbody"},
		{"empty", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fm, rest := Split(c.in)
			if fm != c.fm || rest != c.rest {
				t.Fatalf("Split(%q) = %q, %q; want %q, %q", c.in, fm, rest, c.fm, c.rest)
			}
		})
	}
}
