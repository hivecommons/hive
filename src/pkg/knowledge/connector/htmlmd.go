package connector

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// richHTMLToMarkdown converts an HTML (or Confluence storage-format XHTML)
// fragment to markdown, keeping the structure that HTMLToMarkdown flattens:
// headings, lists, tables, code blocks, links and quotes. Confluence `ac:` /
// `ri:` elements are understood: code/noformat macros become fenced code,
// info/note/warning/tip/panel macros become blockquotes, and page links,
// images and relative hrefs are made absolute against base.
type mdConverter struct {
	// base is the site URL (for Confluence the base_url including any
	// context path such as /wiki); relative links resolve against it.
	base *url.URL
	// pageID is used to build attachment download links.
	pageID string
}

var (
	reCDATA     = regexp.MustCompile(`(?s)<!\[CDATA\[(.*?)\]\]>`)
	reSelfClose = regexp.MustCompile(`<((?:ac|ri):[A-Za-z-]+)([^<>]*?)/>`)
	reBlankRuns = regexp.MustCompile(`\n{3,}`)
)

// richHTMLToMarkdown is the entry point; base may be nil.
func richHTMLToMarkdown(src string, base *url.URL, pageID string) string {
	c := &mdConverter{base: base, pageID: pageID}
	// The HTML5 parser treats CDATA outside foreign content as a bogus
	// comment and ignores the self-closing flag on unknown elements; rewrite
	// both so storage-format macros keep their content and nesting.
	src = reCDATA.ReplaceAllStringFunc(src, func(m string) string {
		return html.EscapeString(reCDATA.FindStringSubmatch(m)[1])
	})
	src = reSelfClose.ReplaceAllString(src, "<$1$2></$1>")
	// ParseFragment rejects a context node whose DataAtom does not match
	// its Data, so both must be set for the body context.
	ctx := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err := html.ParseFragment(strings.NewReader(src), ctx)
	if err != nil {
		return ""
	}
	root := &html.Node{Type: html.ElementNode, Data: "div"}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	return tidyMarkdown(c.blocks(root, "\n\n"))
}

func tidyMarkdown(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimSpace(reBlankRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

var blockTags = map[string]bool{
	"p": true, "div": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"ul": true, "ol": true, "pre": true, "blockquote": true, "table": true, "hr": true,
	"section": true, "article": true, "header": true, "footer": true, "main": true, "aside": true,
	"dl": true, "dt": true, "dd": true, "figure": true, "details": true, "summary": true,
	"ac:structured-macro": true, "ac:macro": true, "ac:layout": true, "ac:layout-section": true,
	"ac:layout-cell": true, "ac:rich-text-body": true, "ac:task-list": true, "ac:task": true,
	"ac:adf-extension": true, "ac:adf-node": true,
}

var skipTags = map[string]bool{
	"script": true, "style": true, "head": true, "title": true, "nav": true, "noscript": true,
	"ac:parameter": true, "ac:task-id": true, "ac:task-status": true, "ac:placeholder": true,
	"ac:emoticon": true, "ac:adf-attribute": true, "ac:adf-fallback": true,
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func isBlock(n *html.Node) bool {
	return n.Type == html.ElementNode && blockTags[n.Data]
}

// blocks renders n's children as block markdown joined by sep; runs of
// inline content between blocks become paragraphs.
func (c *mdConverter) blocks(n *html.Node, sep string) string {
	var out []string
	var inline strings.Builder
	flush := func() {
		if t := strings.TrimSpace(inline.String()); t != "" {
			out = append(out, t)
		}
		inline.Reset()
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if isBlock(ch) {
			flush()
			if b := strings.TrimSpace(c.block(ch)); b != "" {
				out = append(out, b)
			}
			continue
		}
		inline.WriteString(c.inline(ch))
	}
	flush()
	return strings.Join(out, sep)
}

func (c *mdConverter) block(n *html.Node) string {
	switch n.Data {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level := int(n.Data[1] - '0')
		return strings.Repeat("#", level) + " " + oneLine(c.inlineChildren(n))
	case "ul", "ol":
		return c.list(n, n.Data == "ol")
	case "ac:task-list":
		return c.taskList(n)
	case "pre":
		return fence(textContent(n), attr(n, "data-language"))
	case "blockquote":
		return quote(c.blocks(n, "\n\n"))
	case "table":
		return c.table(n)
	case "hr":
		return "---"
	case "ac:structured-macro", "ac:macro":
		return c.macro(n)
	case "details":
		return c.blocks(n, "\n\n")
	case "summary", "dt":
		return "**" + oneLine(c.inlineChildren(n)) + "**"
	}
	return c.blocks(n, "\n\n")
}

// macro renders a Confluence structured macro.
func (c *mdConverter) macro(n *html.Node) string {
	name := strings.ToLower(attr(n, "ac:name"))
	params := map[string]string{}
	var plain, rich *html.Node
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type != html.ElementNode {
			continue
		}
		switch ch.Data {
		case "ac:parameter":
			params[strings.ToLower(attr(ch, "ac:name"))] = strings.TrimSpace(textContent(ch))
		case "ac:plain-text-body":
			plain = ch
		case "ac:rich-text-body":
			rich = ch
		}
	}
	switch name {
	case "code", "noformat":
		body := ""
		if plain != nil {
			body = textContent(plain)
		}
		return fence(body, params["language"])
	case "info", "note", "warning", "tip", "panel", "success", "error":
		label := strings.ToUpper(name[:1]) + name[1:]
		if t := params["title"]; t != "" {
			label += ": " + t
		}
		body := ""
		if rich != nil {
			body = c.blocks(rich, "\n\n")
		}
		return quote(strings.TrimSpace("**" + label + "**\n\n" + body))
	case "expand":
		title := params["title"]
		if title == "" {
			title = "Details"
		}
		body := ""
		if rich != nil {
			body = c.blocks(rich, "\n\n")
		}
		return strings.TrimSpace("**" + title + "**\n\n" + body)
	case "toc", "anchor", "children", "pagetree", "recently-updated", "livesearch", "contentbylabel":
		return ""
	case "status":
		return "`" + params["title"] + "`"
	}
	if rich != nil {
		return c.blocks(rich, "\n\n")
	}
	if plain != nil {
		return fence(textContent(plain), "")
	}
	return fmt.Sprintf("_[Confluence macro: %s]_", name)
}

func (c *mdConverter) list(n *html.Node, ordered bool) string {
	var items []string
	i := 0
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type != html.ElementNode || ch.Data != "li" {
			continue
		}
		i++
		marker := "- "
		if ordered {
			marker = fmt.Sprintf("%d. ", i)
		}
		items = append(items, indentItem(marker, c.blocks(ch, "\n")))
	}
	return strings.Join(items, "\n")
}

func (c *mdConverter) taskList(n *html.Node) string {
	var items []string
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type != html.ElementNode || ch.Data != "ac:task" {
			continue
		}
		box := "[ ] "
		var body string
		for t := ch.FirstChild; t != nil; t = t.NextSibling {
			if t.Type != html.ElementNode {
				continue
			}
			switch t.Data {
			case "ac:task-status":
				if strings.TrimSpace(textContent(t)) == "complete" {
					box = "[x] "
				}
			case "ac:task-body":
				body = c.blocks(t, "\n")
			}
		}
		items = append(items, indentItem("- "+box, body))
	}
	return strings.Join(items, "\n")
}

// indentItem prefixes the first line with marker and indents the rest so
// nested content stays inside the list item.
func indentItem(marker, body string) string {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	pad := strings.Repeat(" ", len(marker))
	for i := range lines {
		if i == 0 {
			lines[i] = marker + lines[i]
		} else if lines[i] != "" {
			lines[i] = pad + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func (c *mdConverter) table(n *html.Node) string {
	var rows [][]string
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		for ch := x.FirstChild; ch != nil; ch = ch.NextSibling {
			if ch.Type != html.ElementNode {
				continue
			}
			if ch.Data == "tr" {
				var row []string
				for td := ch.FirstChild; td != nil; td = td.NextSibling {
					if td.Type == html.ElementNode && (td.Data == "td" || td.Data == "th") {
						row = append(row, tableCell(c.blocks(td, " ")))
					}
				}
				rows = append(rows, row)
				continue
			}
			if ch.Data != "table" {
				walk(ch)
			}
		}
	}
	walk(n)
	return markdownTable(rows)
}

// markdownTable renders rows (the first is the header) as a GFM table.
func markdownTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return ""
	}
	var b strings.Builder
	line := func(r []string) {
		b.WriteString("|")
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(r) {
				cell = r[i]
			}
			b.WriteString(" " + cell + " |")
		}
		b.WriteString("\n")
	}
	line(rows[0])
	b.WriteString("|" + strings.Repeat(" --- |", cols) + "\n")
	for _, r := range rows[1:] {
		line(r)
	}
	return strings.TrimRight(b.String(), "\n")
}

func tableCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", `\|`)
}

func (c *mdConverter) inlineChildren(n *html.Node) string {
	var b strings.Builder
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if isBlock(ch) {
			b.WriteString(" " + c.block(ch) + " ")
			continue
		}
		b.WriteString(c.inline(ch))
	}
	return b.String()
}

var wsRun = regexp.MustCompile(`[ \t\r\n]+`)

func (c *mdConverter) inline(n *html.Node) string {
	switch n.Type {
	case html.TextNode:
		return wsRun.ReplaceAllString(n.Data, " ")
	case html.ElementNode:
	default:
		return ""
	}
	if skipTags[n.Data] {
		return ""
	}
	switch n.Data {
	case "br":
		return "\n"
	case "strong", "b":
		return wrap("**", c.inlineChildren(n))
	case "em", "i":
		return wrap("*", c.inlineChildren(n))
	case "s", "del", "strike":
		return wrap("~~", c.inlineChildren(n))
	case "code", "kbd", "tt":
		if t := strings.TrimSpace(textContent(n)); t != "" {
			return "`" + t + "`"
		}
		return ""
	case "a":
		text := strings.TrimSpace(c.inlineChildren(n))
		href := c.abs(attr(n, "href"))
		if href == "" {
			return text
		}
		if text == "" {
			text = href
		}
		return "[" + text + "](" + href + ")"
	case "img":
		src := c.abs(attr(n, "src"))
		if src == "" {
			return ""
		}
		return "![" + attr(n, "alt") + "](" + src + ")"
	case "time":
		if d := attr(n, "datetime"); d != "" {
			return d
		}
	case "ac:link":
		return c.acLink(n)
	case "ac:image":
		return c.acImage(n)
	case "ri:user":
		if u := attr(n, "ri:username"); u != "" {
			return "@" + u
		}
		return "@user"
	case "ac:plain-text-link-body", "ac:link-body":
		return ""
	}
	return c.inlineChildren(n)
}

func wrap(marker, s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return s
	}
	lead := s[:len(s)-len(strings.TrimLeft(s, " "))]
	trail := s[len(strings.TrimRight(s, " ")):]
	return lead + marker + t + marker + trail
}

// acLink renders <ac:link> to a page, attachment, user or URL.
func (c *mdConverter) acLink(n *html.Node) string {
	var text, href string
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type != html.ElementNode {
			continue
		}
		switch ch.Data {
		case "ri:page":
			title := attr(ch, "ri:content-title")
			text = title
			if space := attr(ch, "ri:space-key"); space != "" && title != "" {
				href = c.abs("/display/" + url.PathEscape(space) + "/" + url.PathEscape(title))
			}
		case "ri:attachment":
			text = attr(ch, "ri:filename")
		case "ri:url":
			href = c.abs(attr(ch, "ri:value"))
		case "ri:user":
			text = c.inline(ch)
		case "ac:plain-text-link-body", "ac:link-body":
			if t := strings.TrimSpace(c.inlineChildren(ch)); t != "" {
				text = t
			}
		}
		// ri:page and friends may (after self-close rewriting) contain the
		// link body as a child.
		for gc := ch.FirstChild; gc != nil; gc = gc.NextSibling {
			if gc.Type == html.ElementNode && (gc.Data == "ac:plain-text-link-body" || gc.Data == "ac:link-body") {
				if t := strings.TrimSpace(c.inlineChildren(gc)); t != "" {
					text = t
				}
			}
		}
	}
	if anchor := attr(n, "ac:anchor"); anchor != "" && href != "" {
		href += "#" + anchor
	}
	if text == "" {
		text = href
	}
	if href == "" {
		return text
	}
	return "[" + text + "](" + href + ")"
}

func (c *mdConverter) acImage(n *html.Node) string {
	alt := attr(n, "ac:alt")
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type != html.ElementNode {
			continue
		}
		switch ch.Data {
		case "ri:url":
			return "![" + alt + "](" + c.abs(attr(ch, "ri:value")) + ")"
		case "ri:attachment":
			name := attr(ch, "ri:filename")
			if alt == "" {
				alt = name
			}
			if c.pageID == "" {
				return "![" + alt + "](" + name + ")"
			}
			return "![" + alt + "](" + c.abs("/download/attachments/"+url.PathEscape(c.pageID)+"/"+url.PathEscape(name)) + ")"
		}
	}
	return ""
}

// abs resolves href against the base URL. Paths starting with "/" that
// already carry the base path (e.g. /wiki/spaces/...) are not doubled.
func (c *mdConverter) abs(href string) string {
	href = strings.TrimSpace(href)
	if href == "" || c.base == nil {
		return href
	}
	u, err := url.Parse(href)
	if err != nil || u.IsAbs() || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "mailto:") {
		return href
	}
	if strings.HasPrefix(href, "/") && !strings.HasPrefix(href, "//") {
		basePath := strings.TrimRight(c.base.Path, "/")
		if basePath != "" && !strings.HasPrefix(href, basePath+"/") {
			href = basePath + href
		}
		return c.base.Scheme + "://" + c.base.Host + href
	}
	return c.base.ResolveReference(u).String()
}

func textContent(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type == html.ElementNode && ch.Data == "br" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(textContent(ch))
	}
	return b.String()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// fence wraps body in a fenced code block long enough not to collide with
// backticks inside it.
func fence(body, lang string) string {
	body = strings.Trim(body, "\n")
	ticks := "```"
	for strings.Contains(body, ticks) {
		ticks += "`"
	}
	return ticks + strings.TrimSpace(lang) + "\n" + body + "\n" + ticks
}

func quote(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if l == "" {
			lines[i] = ">"
		} else {
			lines[i] = "> " + l
		}
	}
	return strings.Join(lines, "\n")
}
