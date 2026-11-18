package prosefilter

import "testing"

const (
	em = "\u2014"
	en = "\u2013"
)

func TestSanitize(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"no dash is unchanged", "plain - text", "plain - text"},
		{"spaced em-dash", "a " + em + " b", "a - b"},
		{"unspaced em-dash", "a" + em + "b", "a - b"},
		{"several em-dashes", "a " + em + " b " + em + " c", "a - b - c"},
		{"leading em-dash", em + " hive: agent=x", "- hive: agent=x"},
		{"leading em-dash keeps indent", "  " + em + " note", "  - note"},
		{"trailing em-dash", "wait " + em, "wait -"},
		{"spaced en-dash", "a " + en + " b", "a - b"},
		{"en-dash range untouched", "levels 3" + en + "5", "levels 3" + en + "5"},
		{"inline code untouched", "see `a " + em + " b` and c " + em + " d", "see `a " + em + " b` and c - d"},
		{"double-backtick span untouched", "``a ` " + em + " b`` " + em + " c", "``a ` " + em + " b`` - c"},
		{"unclosed backtick is prose", "a ` b " + em + " c", "a ` b - c"},
		{"fenced block untouched", "x " + em + " y\n```\nfile " + em + " list\n```\nz " + em + " w", "x - y\n```\nfile " + em + " list\n```\nz - w"},
		{"tilde fence untouched", "~~~go\na " + em + " b\n~~~\nc " + em + " d", "~~~go\na " + em + " b\n~~~\nc - d"},
		{"longer closing fence", "````\na " + em + " b\n`````\nc " + em + " d", "````\na " + em + " b\n`````\nc - d"},
		{"short closing fence does not close", "````\na\n```\nb " + em + " c\n````", "````\na\n```\nb " + em + " c\n````"},
		{"blockquote untouched", "> quoted " + em + " text\nmine " + em + " here", "> quoted " + em + " text\nmine - here"},
		{"markdown list", "- file.go " + em + " added", "- file.go - added"},
		{"CRLF preserved", "a " + em + " b\r\nc", "a - b\r\nc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Sanitize(tc.in); got != tc.want {
				t.Fatalf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if got := Sanitize(Sanitize(tc.in)); got != tc.want {
				t.Fatalf("Sanitize is not idempotent for %q: %q", tc.in, got)
			}
		})
	}
}

func TestSanitizeTitle(t *testing.T) {
	if got, want := SanitizeTitle("fix: a "+em+" b `c "+em+" d`"), "fix: a - b `c "+em+" d`"; got != want {
		t.Fatalf("SanitizeTitle = %q, want %q", got, want)
	}
	if got := SanitizeTitle("fix: plain"); got != "fix: plain" {
		t.Fatalf("clean title changed: %q", got)
	}
}

func TestHasDash(t *testing.T) {
	if HasDash("a - b") {
		t.Fatal("hyphen reported as dash")
	}
	if !HasDash("a "+em+" b") || !HasDash("a "+en+" b") {
		t.Fatal("em or en dash not detected")
	}
}
