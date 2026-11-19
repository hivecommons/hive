package inbox

import (
	"context"
	"strings"
	"testing"
)

func TestExtractHTMLReadableMarkdown(t *testing.T) {
	page := `<html><head><title> The   Title </title><style>.x{}</style></head><body>
<header>Site header</header><nav>Menu</nav>
<main>
<h1>Main heading</h1>
<p>Read <a href="https://example.org/doc">the doc</a> and <a href="/rel">relative</a>, <em>now</em>.</p>
<p hidden>secret hidden</p><p aria-hidden="true">aria secret</p>
<ol><li>first</li><li>second <code>x()</code></li></ol>
<blockquote><p>quoted line</p></blockquote>
<pre>  code
    indented</pre>
<table><tr><th>k</th><th>v</th></tr><tr><td>a|b</td><td>2</td></tr></table>
<img src="x.png" alt="diagram"><hr>
<form><input value="x">form text</form><script>steal()</script>
</main><footer>Footer</footer></body></html>`
	ex, err := ExtractUpload(context.Background(), UploadKindHTML, []byte(page))
	if err != nil {
		t.Fatal(err)
	}
	if ex.Title != "The Title" {
		t.Fatalf("title = %q", ex.Title)
	}
	for _, want := range []string{"# Main heading", "Read [the doc](https://example.org/doc) and relative, _now_.", "1. first\n2. second `x()`",
		"> quoted line", "```\n  code\n    indented\n```", "| k | v |", `| a\|b | 2 |`, "[image: diagram]", "---"} {
		if !strings.Contains(ex.Markdown, want) {
			t.Errorf("markdown missing %q:\n%s", want, ex.Markdown)
		}
	}
	for _, gone := range []string{"steal", "Menu", "Footer", "form text", "secret", ".x{}", "Site header"} {
		if strings.Contains(ex.Markdown, gone) {
			t.Errorf("markdown kept %q:\n%s", gone, ex.Markdown)
		}
	}
}

func TestExtractHTMLEdgeCases(t *testing.T) {
	// No <title>: the first heading names it; body without main/article.
	ex, err := ExtractUpload(context.Background(), UploadKindHTML, []byte("<body><h2>Only heading</h2><ul><li><p>nested</p><ul><li>deep</li></ul></li></ul></body>"))
	if err != nil || ex.Title != "Only heading" || !strings.Contains(ex.Markdown, "- nested\n  - deep") {
		t.Fatalf("no title = %+v, %v", ex, err)
	}
	if _, err := ExtractUpload(context.Background(), UploadKindHTML, []byte("<html><script>x</script></html>")); err == nil || err.Code != UploadErrEmpty {
		t.Fatalf("empty page = %v", err)
	}
	// Absurd nesting is cut off instead of recursing without bound.
	deep := strings.Repeat("<div>", htmlMaxDepth*4) + "bottom" + strings.Repeat("</div>", htmlMaxDepth*4)
	// The parser itself refuses absurd nesting; either way nothing recurses
	// without bound.
	if _, err := ExtractUpload(context.Background(), UploadKindHTML, []byte("<p>top</p>"+deep)); err != nil && err.Code != UploadErrParse {
		t.Fatalf("deep nesting = %v", err)
	}
	if ex, err := ExtractUpload(context.Background(), UploadKindHTML, []byte("<p>top</p>"+strings.Repeat("<span>", htmlMaxDepth+20)+"bottom")); err != nil || !strings.Contains(ex.Markdown, "top") || strings.Contains(ex.Markdown, "bottom") {
		t.Fatalf("depth cap = %+v, %v", ex, err)
	}
	if got := wrapInline("**", " "); got != " " {
		t.Fatalf("empty inline = %q", got)
	}
	if k, err := SniffUpload("page.htm", []byte("<p>hi</p>")); err != nil || k != UploadKindHTML {
		t.Fatalf("sniff .htm = %q, %v", k, err)
	}
}
