package inbox

import (
	"context"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// HTML extraction limits. The page is parsed with golang.org/x/net/html (a
// spec-compliant tokenizer; nothing is executed, no resource is loaded) and
// reduced to readable markdown: scripts, styles, forms and page chrome are
// dropped; headings, paragraphs, lists, quotes, code and tables are kept.
const (
	// htmlMaxDepth stops descending into absurdly nested markup.
	htmlMaxDepth = 256
	// htmlMaxNodes bounds the nodes one page may render.
	htmlMaxNodes = 200000
	// htmlHeadingLevels is the deepest markdown heading.
	htmlHeadingLevels = 6
)

// htmlSkipped are elements whose content never reaches the text.
var htmlSkipped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true,
	atom.Iframe: true, atom.Object: true, atom.Embed: true, atom.Svg: true,
	atom.Canvas: true, atom.Form: true, atom.Button: true, atom.Input: true,
	atom.Select: true, atom.Textarea: true, atom.Nav: true, atom.Footer: true,
	atom.Aside: true, atom.Head: true, atom.Dialog: true, atom.Audio: true,
	atom.Video: true, atom.Picture: true, atom.Source: true, atom.Track: true,
}

// htmlBlocks are rendered as their own paragraph.
var htmlBlocks = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true,
	atom.Main: true, atom.Body: true, atom.Header: true, atom.Figure: true,
	atom.Figcaption: true, atom.Dl: true, atom.Dt: true, atom.Dd: true,
	atom.Address: true, atom.Details: true, atom.Summary: true, atom.Center: true,
}

type htmlConv struct {
	ctx   context.Context
	nodes int
}

// extractHTML turns an HTML page into markdown with its <title>.
func extractHTML(ctx context.Context, text string) (Extracted, *UploadError) {
	doc, err := html.Parse(strings.NewReader(text))
	if err != nil {
		return Extracted{}, uploadErr(UploadErrParse, "the page is not readable HTML (%s)", err)
	}
	title := truncateRunes(strings.Join(strings.Fields(htmlText(htmlFind(doc, atom.Title))), " "), uploadTitleRunes)
	root := htmlFind(doc, atom.Article)
	if root == nil {
		root = htmlFind(doc, atom.Main)
	}
	if root == nil {
		root = htmlFind(doc, atom.Body)
	}
	if root == nil {
		root = doc
	}
	c := &htmlConv{ctx: ctx}
	md := c.children(root, 0)
	if ctx.Err() != nil {
		return Extracted{}, uploadErr(UploadErrParse, "reading the page took too long")
	}
	md = tidyMarkdown(md)
	if md == "" {
		return Extracted{}, uploadErr(UploadErrEmpty, "the page has no readable text")
	}
	if title == "" {
		title = htmlFirstHeading(root)
	}
	return Extracted{Markdown: md, Title: title}, nil
}

// htmlFind returns the first element of type a, depth first.
func htmlFind(n *html.Node, a atom.Atom) *html.Node {
	if n == nil {
		return nil
	}
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := htmlFind(c, a); f != nil {
			return f
		}
	}
	return nil
}

// htmlText is the raw text content of n.
func htmlText(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node, int)
	walk = func(n *html.Node, depth int) {
		if depth > htmlMaxDepth {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, depth+1)
		}
	}
	walk(n, 0)
	return b.String()
}

func htmlHasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

// htmlFirstHeading is the text of the first h1, h2 or h3 under n.
func htmlFirstHeading(n *html.Node) string {
	for _, a := range []atom.Atom{atom.H1, atom.H2, atom.H3} {
		if h := htmlFind(n, a); h != nil {
			if t := oneLine(htmlText(h)); t != "" {
				return truncateRunes(t, uploadTitleRunes)
			}
		}
	}
	return ""
}

func htmlAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func (c *htmlConv) children(n *html.Node, depth int) string {
	var b strings.Builder
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		b.WriteString(c.node(ch, depth+1))
	}
	return b.String()
}

func (c *htmlConv) node(n *html.Node, depth int) string {
	c.nodes++
	if depth > htmlMaxDepth || c.nodes > htmlMaxNodes || (c.nodes%1024 == 0 && c.ctx.Err() != nil) {
		return ""
	}
	switch n.Type {
	case html.TextNode:
		return collapseSpace(n.Data)
	case html.ElementNode:
	default:
		return c.children(n, depth)
	}
	if htmlSkipped[n.DataAtom] || htmlHasAttr(n, "hidden") || strings.EqualFold(htmlAttr(n, "aria-hidden"), "true") {
		return ""
	}
	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		level := int(n.Data[1] - '0')
		text := oneLine(c.children(n, depth))
		if text == "" {
			return ""
		}
		return "\n\n" + strings.Repeat("#", min(level, htmlHeadingLevels)) + " " + text + "\n\n"
	case atom.Br:
		return "\n"
	case atom.Hr:
		return "\n\n---\n\n"
	case atom.Pre:
		return "\n\n" + fenced("", strings.Trim(htmlText(n), "\n")) + "\n\n"
	case atom.Blockquote:
		inner := tidyMarkdown(c.children(n, depth))
		if inner == "" {
			return ""
		}
		return "\n\n> " + strings.ReplaceAll(inner, "\n", "\n> ") + "\n\n"
	case atom.Ul, atom.Ol:
		return c.list(n, depth)
	case atom.Table:
		return c.table(n, depth)
	case atom.Strong, atom.B:
		return wrapInline("**", c.children(n, depth))
	case atom.Em, atom.I:
		return wrapInline("_", c.children(n, depth))
	case atom.Code:
		if t := strings.TrimSpace(htmlText(n)); t != "" {
			return "`" + strings.ReplaceAll(t, "`", "'") + "`"
		}
		return ""
	case atom.A:
		text := c.children(n, depth)
		href := htmlAttr(n, "href")
		if t := strings.TrimSpace(text); t != "" && (strings.HasPrefix(href, "https://") || strings.HasPrefix(href, "http://")) {
			return "[" + oneLine(t) + "](" + strings.ReplaceAll(href, ")", "%29") + ")"
		}
		return text
	case atom.Img:
		if alt := strings.TrimSpace(htmlAttr(n, "alt")); alt != "" {
			return "[image: " + oneLine(alt) + "]"
		}
		return ""
	}
	inner := c.children(n, depth)
	if htmlBlocks[n.DataAtom] || n.DataAtom == atom.Li {
		return "\n\n" + inner + "\n\n"
	}
	return inner
}

func (c *htmlConv) list(n *html.Node, depth int) string {
	var b strings.Builder
	num := 0
	for li := n.FirstChild; li != nil; li = li.NextSibling {
		if li.Type != html.ElementNode || li.DataAtom != atom.Li {
			continue
		}
		num++
		item := tidyMarkdown(c.children(li, depth+1))
		// An item is one block: its paragraphs and sub-lists stay together.
		for strings.Contains(item, "\n\n") {
			item = strings.ReplaceAll(item, "\n\n", "\n")
		}
		if item == "" {
			continue
		}
		marker := "- "
		if n.DataAtom == atom.Ol {
			marker = strconv.Itoa(num) + ". "
		}
		b.WriteString(marker + strings.ReplaceAll(item, "\n", "\n"+strings.Repeat(" ", len(marker))) + "\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\n" + b.String() + "\n"
}

func (c *htmlConv) table(n *html.Node, depth int) string {
	var rows [][]string
	var walk func(*html.Node, int)
	walk = func(x *html.Node, d int) {
		if d > htmlMaxDepth {
			return
		}
		for ch := x.FirstChild; ch != nil; ch = ch.NextSibling {
			if ch.Type != html.ElementNode {
				continue
			}
			if ch.DataAtom == atom.Tr {
				var row []string
				for cell := ch.FirstChild; cell != nil; cell = cell.NextSibling {
					if cell.Type == html.ElementNode && (cell.DataAtom == atom.Td || cell.DataAtom == atom.Th) {
						row = append(row, oneLine(c.children(cell, d+1)))
					}
				}
				if len(row) > 0 {
					rows = append(rows, row)
				}
				continue
			}
			if ch.DataAtom != atom.Table {
				walk(ch, d+1)
			}
		}
	}
	walk(n, depth)
	if len(rows) == 0 {
		return ""
	}
	cols := 0
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	if len(rows) > uploadTablePreviewRows {
		rows = rows[:uploadTablePreviewRows]
	}
	return "\n\n" + markdownTable(rows, cols) + "\n\n"
}

// wrapInline puts marks around trimmed inline text, keeping outer spaces.
func wrapInline(mark, s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return s
	}
	lead, trail := "", ""
	if strings.HasPrefix(s, " ") {
		lead = " "
	}
	if strings.HasSuffix(s, " ") {
		trail = " "
	}
	return lead + mark + t + mark + trail
}

// collapseSpace folds runs of whitespace into one space.
func collapseSpace(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' || r == '\f' {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// tidyMarkdown trims every line and squeezes blank-line runs.
func tidyMarkdown(s string) string {
	lines := strings.Split(s, "\n")
	inFence := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inFence = !inFence
			lines[i] = strings.TrimSpace(l)
			continue
		}
		if !inFence {
			l = strings.TrimRight(l, " ")
			// One leading space is a whitespace artefact; two or more
			// indent a list continuation.
			if strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "  ") {
				l = l[1:]
			}
			lines[i] = l
		}
	}
	return collapseBlankLines(strings.Join(lines, "\n"))
}
