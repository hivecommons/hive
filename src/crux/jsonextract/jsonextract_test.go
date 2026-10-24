package jsonextract

import "testing"

// TestObject merges the table tests that previously lived alongside the
// per-package copies in pkg/retro and pkg/intent.
func TestObject(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"no braces", "hello world", ""},
		{"empty string", "", ""},
		{"only open brace", "hello { world", ""},
		{"only close brace", "hello } world", ""},
		{"close before open", "} { ", ""},
		{"bare object", `{"a":1}`, `{"a":1}`},
		{"simple object", `noise {"a":1} noise`, `{"a":1}`},
		{"nested object takes outermost span", `pre {"a":{"b":1}} post`, `{"a":{"b":1}}`},
		{"empty object", "{}", "{}"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := Object(tt.input); got != tt.want {
				t.Fatalf("Object(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
