package mailin

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

func TestHTMLToMarkdown(t *testing.T) {
	cases := map[string]string{
		"":         "",
		"   \r\n ": "",
		"<h1>A</h1><h6>F</h6>line<br>next<hr>after":     "# A\n\n###### F\n\nline\nnext\n\n---\n\nafter",
		"<table><tr><td>a</td><td>b</td></tr></table>":  "a b",
		`<a href="mailto:ada@example.com">mail</a>`:     "mail (mailto:ada@example.com)",
		`<a href="https://x.example/a b">spaced</a>`:    "spaced",
		`<a href="ftp://x.example/">ftp</a>`:            "ftp",
		`<a>no href</a>`:                                "no href",
		"<head><meta charset=x><body><p>kept</p>":       "kept",
		"<svg><text>drawn</text></svg><p>after</p>":     "after",
		"<p>a\n\t  b&amp;c&nbsp;d</p>":                  "a b&c d",
		"<iframe src=x></iframe><object>o</object>text": "text",
		"<script>never</script></script>tail":           "tail",
		"<div><div>nested</div></div>":                  "nested",
	}
	for in, want := range cases {
		if got := htmlToMarkdown([]byte(in)); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestCleanTextAndClip(t *testing.T) {
	if got := cleanText([]byte("a\rb\r\nc\x00\x7f\xff\td  ")); got != "a\nb\nc\td" {
		t.Fatalf("clean %q", got)
	}
	long := strings.Repeat("é", wire.MaxNoteBytes)
	got := clipNote(long)
	if len(got) > wire.MaxNoteBytes || !utf8.ValidString(got) || len(got) < wire.MaxNoteBytes-1 {
		t.Fatalf("clip %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	if clipNote("short") != "short" {
		t.Fatal("short note changed")
	}
}

func TestSafeLink(t *testing.T) {
	for in, want := range map[string]string{
		" https://a.example/x ": "https://a.example/x",
		"HTTP://A.example":      "HTTP://A.example",
		"javascript:alert(1)":   "",
		"data:text/html,x":      "",
		"https://a.example/(x)": "",
		"//cdn.example/x":       "",
	} {
		if got := safeLink(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
