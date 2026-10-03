package dashboard

import "testing"

func TestPromQuote(t *testing.T) {
	cases := map[string]string{
		"gpt-5":       `"gpt-5"`,
		`a"b\c`:       `"a\"b\\c"`,
		"a\nb":        `"a\nb"`,
		"tab\there":   "\"tab\there\"",
		"bad\xffbyte": "\"bad\uFFFDbyte\"",
		"caf\u00e9":   "\"caf\u00e9\"",
	}
	for in, want := range cases {
		if got := promQuote(in); got != want {
			t.Errorf("promQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
