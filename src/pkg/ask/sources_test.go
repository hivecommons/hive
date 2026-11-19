package ask

import (
	"strings"
	"testing"
)

func TestAllowedSourceURL(t *testing.T) {
	good := []string{
		"https://docs.hivecommons.dev/docs/dashboard-sections",
		"https://DOCS.hivecommons.dev/docs/x#anchor",
		"https://github.com/hivecommons/hive/blob/v5/src/docs/dashboard-sections.md#knowledge",
		"https://github.com/hivecommons/hive/tree/v5/src/docs/design",
	}
	bad := []string{
		"",
		"http://docs.hivecommons.dev/docs/x",
		"javascript:alert(1)",
		"data:text/html,<script>1</script>",
		"//docs.hivecommons.dev/x",
		"https://docs.hivecommons.dev.evil.example/x",
		"https://evil.example/docs.hivecommons.dev",
		"https://user:pw@docs.hivecommons.dev/x",
		"https://docs.hivecommons.dev:8443/x",
		"https://docs.hivecommons.dev/a/../../x",
		"https://docs.hivecommons.dev/x\nSet-Cookie: a=b",
		"https://github.com/hivecommons/hive/issues/1",
		"https://github.com/hivecommons/hive/blob/v5/src/pkg/hub/ask.go",
		"https://github.com/hivecommons/other/blob/v5/src/docs/x.md",
		"https://github.com/evil/hivecommons/hive/blob/v5/src/docs/x.md",
		"https://github.com/hivecommons/hive/blob/v5/src/docs",
		"https://docs.hivecommons.dev/" + strings.Repeat("a", 600),
	}
	for _, u := range good {
		if !AllowedSourceURL(u) {
			t.Errorf("should allow %q", u)
		}
	}
	for _, u := range bad {
		if AllowedSourceURL(u) {
			t.Errorf("should refuse %q", u)
		}
	}
}

func TestFilterSources(t *testing.T) {
	in := []VendorSource{
		{SourceURL: "https://evil.example/x", Title: "Evil"},
		{SourceURL: "https://docs.hivecommons.dev/docs/a", Title: "<img src=x onerror=alert(1)>A\x07\x1b[31m"},
		{SourceURL: "https://docs.hivecommons.dev/docs/a", Title: "duplicate"},
		{SourceURL: "https://docs.hivecommons.dev/docs/internal", Title: "Internal", ContainsInternalData: true},
		{SourceURL: "https://docs.hivecommons.dev/docs/b", Title: ""},
	}
	got := FilterSources(in)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if strings.ContainsAny(got[0].Title, "\x07\x1b") {
		t.Fatalf("control characters survived: %q", got[0].Title)
	}
	if got[1].Title != "https://docs.hivecommons.dev/docs/b" {
		t.Fatalf("empty title should fall back to the URL: %+v", got[1])
	}
	var many []VendorSource
	for i := 0; i < 20; i++ {
		many = append(many, VendorSource{SourceURL: "https://docs.hivecommons.dev/docs/p" + string(rune('a'+i)), Title: "t"})
	}
	if n := len(FilterSources(many)); n != maxSources {
		t.Fatalf("source cap = %d", n)
	}
}

func TestPlainTextCapsAndCleans(t *testing.T) {
	if got := PlainText("  a\x00b\x1bc\n\td  ", 100); got != "abc\n\td" {
		t.Fatalf("got %q", got)
	}
	if got := PlainText("héllo wörld", 5); got != "héllo" {
		t.Fatalf("rune-safe cap: %q", got)
	}
}

func TestSectionLabelIsAClosedVocabulary(t *testing.T) {
	if SectionLabel("settings-features") != "Settings, Features" {
		t.Fatal("known id lost its label")
	}
	for _, id := range []string{"", "https://example.com", "ghp_x", "Settings, Features", "customers-section/alice"} {
		if SectionLabel(id) != "" {
			t.Fatalf("%q must not resolve", id)
		}
	}
}
