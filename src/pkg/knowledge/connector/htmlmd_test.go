package connector

import (
	"net/url"
	"strings"
	"testing"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestRichHTMLToMarkdownMacros(t *testing.T) {
	base := mustURL(t, "https://acme.atlassian.net/wiki")
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"info with title",
			`<ac:structured-macro ac:name="info"><ac:parameter ac:name="title">Heads up</ac:parameter><ac:rich-text-body><p>Body text</p></ac:rich-text-body></ac:structured-macro>`,
			"> **Info: Heads up**\n>\n> Body text"},
		{"warning", `<ac:structured-macro ac:name="warning"><ac:rich-text-body><p>Careful</p></ac:rich-text-body></ac:structured-macro>`,
			"> **Warning**\n>\n> Careful"},
		{"note", `<ac:structured-macro ac:name="note"><ac:rich-text-body><p>N</p></ac:rich-text-body></ac:structured-macro>`,
			"> **Note**\n>\n> N"},
		{"tip", `<ac:structured-macro ac:name="tip"><ac:rich-text-body><p>T</p></ac:rich-text-body></ac:structured-macro>`,
			"> **Tip**\n>\n> T"},
		{"panel without body", `<ac:structured-macro ac:name="panel"></ac:structured-macro>`, "> **Panel**"},
		{"code with language",
			`<ac:structured-macro ac:name="code"><ac:parameter ac:name="language">bash</ac:parameter><ac:plain-text-body><![CDATA[make deploy
echo "<done>"]]></ac:plain-text-body></ac:structured-macro>`,
			"```bash\nmake deploy\necho \"<done>\"\n```"},
		{"code containing fence",
			"<ac:structured-macro ac:name=\"code\"><ac:plain-text-body><![CDATA[```inner```]]></ac:plain-text-body></ac:structured-macro>",
			"````\n```inner```\n````"},
		{"noformat", `<ac:structured-macro ac:name="noformat"><ac:plain-text-body><![CDATA[raw]]></ac:plain-text-body></ac:structured-macro>`,
			"```\nraw\n```"},
		{"expand", `<ac:structured-macro ac:name="expand"><ac:parameter ac:name="title">More</ac:parameter><ac:rich-text-body><p>hidden</p></ac:rich-text-body></ac:structured-macro>`,
			"**More**\n\nhidden"},
		{"expand default title", `<ac:structured-macro ac:name="expand"><ac:rich-text-body><p>x</p></ac:rich-text-body></ac:structured-macro>`,
			"**Details**\n\nx"},
		{"toc dropped", `<ac:structured-macro ac:name="toc"></ac:structured-macro><p>after</p>`, "after"},
		{"status", `<ac:structured-macro ac:name="status"><ac:parameter ac:name="title">DONE</ac:parameter></ac:structured-macro>`, "`DONE`"},
		{"unknown with rich body", `<ac:structured-macro ac:name="section"><ac:rich-text-body><p>inside</p></ac:rich-text-body></ac:structured-macro>`, "inside"},
		{"unknown with plain body", `<ac:structured-macro ac:name="html"><ac:plain-text-body><![CDATA[<b>x</b>]]></ac:plain-text-body></ac:structured-macro>`, "```\n<b>x</b>\n```"},
		{"unknown without body", `<ac:structured-macro ac:name="jira"><ac:parameter ac:name="key">ABC-1</ac:parameter></ac:structured-macro>`, "_[Confluence macro: jira]_"},
		{"page link with body",
			`<p><ac:link><ri:page ri:content-title="Other Page" ri:space-key="ENG" /><ac:plain-text-link-body><![CDATA[see this]]></ac:plain-text-link-body></ac:link></p>`,
			"[see this](https://acme.atlassian.net/wiki/display/ENG/Other%20Page)"},
		{"page link title only and anchor",
			`<p><ac:link ac:anchor="sec"><ri:page ri:content-title="Home" ri:space-key="ENG"/></ac:link></p>`,
			"[Home](https://acme.atlassian.net/wiki/display/ENG/Home#sec)"},
		{"page link without space", `<p><ac:link><ri:page ri:content-title="Local"/></ac:link></p>`, "Local"},
		{"attachment link", `<p><ac:link><ri:attachment ri:filename="spec.pdf"/></ac:link></p>`, "spec.pdf"},
		{"url link", `<p><ac:link><ri:url ri:value="https://example.com/x"/><ac:link-body>Ex</ac:link-body></ac:link></p>`, "[Ex](https://example.com/x)"},
		{"user mention", `<p>Ping <ac:link><ri:user ri:username="jdoe"/></ac:link></p>`, "Ping @jdoe"},
		{"user mention by account", `<p><ri:user ri:account-id="123"/></p>`, "@user"},
		{"image attachment", `<ac:image ac:alt="arch"><ri:attachment ri:filename="a b.png"/></ac:image>`,
			"![arch](https://acme.atlassian.net/wiki/download/attachments/42/a%20b.png)"},
		{"image url", `<ac:image><ri:url ri:value="https://img.example/x.png"/></ac:image>`, "![](https://img.example/x.png)"},
		{"image without source", `<ac:image></ac:image><p>x</p>`, "x"},
		{"task list",
			`<ac:task-list><ac:task><ac:task-id>1</ac:task-id><ac:task-status>complete</ac:task-status><ac:task-body>done it</ac:task-body></ac:task><ac:task><ac:task-status>incomplete</ac:task-status><ac:task-body>todo</ac:task-body></ac:task></ac:task-list>`,
			"- [x] done it\n- [ ] todo"},
		{"layout cells", `<ac:layout><ac:layout-section><ac:layout-cell><p>left</p></ac:layout-cell><ac:layout-cell><p>right</p></ac:layout-cell></ac:layout-section></ac:layout>`,
			"left\n\nright"},
		{"emoticon dropped", `<p>ok <ac:emoticon ac:name="tick"/></p>`, "ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := richHTMLToMarkdown(tt.in, base, "42"); got != tt.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestRichHTMLToMarkdownHTML(t *testing.T) {
	base := mustURL(t, "https://acme.atlassian.net/wiki")
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"headings", `<h1>Top</h1><h2>Sub <em>x</em></h2><h6>Six</h6>`, "# Top\n\n## Sub *x*\n\n###### Six"},
		{"inline", `<p>a <em>b</em> <code>c</code> <s>d</s> <a href="/spaces/X">e</a> <img src="/i.png" alt="i"/><br/>f</p>`,
			"a *b* `c` ~~d~~ [e](https://acme.atlassian.net/wiki/spaces/X) ![i](https://acme.atlassian.net/wiki/i.png)\nf"},
		{"bold", `<p><strong>s</strong> <b>t</b></p>`, "**s** **t**"},
		{"base path not doubled", `<p><a href="/wiki/spaces/Y">y</a></p>`, "[y](https://acme.atlassian.net/wiki/spaces/Y)"},
		{"absolute and special hrefs", `<p><a href="https://o.example/">o</a> <a href="#frag">f</a> <a href="mailto:a@b.c">m</a></p>`,
			"[o](https://o.example/) [f](#frag) [m](mailto:a@b.c)"},
		{"link without text or href", `<p><a href="https://o.example/x"></a> <a>plain</a></p>`, "[https://o.example/x](https://o.example/x) plain"},
		{"empty inline", `<p><code></code><img alt="none"/>x</p>`, "x"},
		{"time", `<p>on <time datetime="2026-10-08"/></p>`, "on 2026-10-08"},
		{"nested lists", `<ul><li>one<ul><li>two</li></ul></li><li>three</li></ul>`, "- one\n  - two\n- three"},
		{"ordered list", `<ol><li>a</li><li><p>b</p></li></ol>`, "1. a\n2. b"},
		{"table", `<table><tbody><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>x|y</td></tr><tr><td>only</td></tr></tbody></table>`,
			"| A | B |\n| --- | --- |\n| 1 | x\\|y |\n| only |  |"},
		{"empty table", `<table></table><p>x</p>`, "x"},
		{"blockquote", `<blockquote><p>q1</p><p>q2</p></blockquote>`, "> q1\n>\n> q2"},
		{"pre", "<pre data-language=\"go\">line1\n  line2</pre>", "```go\nline1\n  line2\n```"},
		{"hr and script", `<script>alert(1)</script><p>x</p><hr/><p>y</p>`, "x\n\n---\n\ny"},
		{"details and dl", `<details><summary>S</summary><p>b</p></details><dl><dt>T</dt><dd>D</dd></dl>`, "**S**\n\nb\n\n**T**\n\nD"},
		{"mixed inline and blocks", `<div>lead<p>para</p>tail</div>`, "lead\n\npara\n\ntail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := richHTMLToMarkdown(tt.in, base, ""); got != tt.want {
				t.Fatalf("got:\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
	if got := richHTMLToMarkdown(`<p><a href="/x">x</a></p><ac:image><ri:attachment ri:filename="f.png"/></ac:image>`, nil, ""); got != "[x](/x)\n\n![f.png](f.png)" {
		t.Fatalf("nil base: %q", got)
	}
	if got := markdownTable([][]string{{}}); got != "" {
		t.Fatalf("zero-column table = %q", got)
	}
	if got := quote("  "); got != "" {
		t.Fatalf("empty quote = %q", got)
	}
	if !strings.HasPrefix(fence("x", ""), "```\n") {
		t.Fatal("fence prefix")
	}
}
