package mailin

import (
	"bytes"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// A message's text body becomes a Markdown note (hivecommons/hive#18564).
// text/plain is kept as sent; text/html is reduced to its text, headings,
// list items and link targets. Nothing remote survives: images, scripts,
// styles, frames and embedded objects are dropped, and a link is written
// out as its text and URL, never as markup that loads anything. The note is
// untrusted data on the spoke, like any attachment.

// skippedHTML are elements whose content never reaches the note.
var skippedHTML = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Head: true, atom.Title: true, atom.Noscript: true,
	atom.Template: true, atom.Svg: true, atom.Math: true, atom.Iframe: true, atom.Object: true,
	atom.Embed: true, atom.Select: true, atom.Textarea: true,
}

// blockHTML are elements that start and end a paragraph.
var blockHTML = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Table: true, atom.Tr: true, atom.Blockquote: true,
	atom.Section: true, atom.Article: true, atom.Header: true, atom.Footer: true, atom.Ul: true,
	atom.Ol: true, atom.Pre: true, atom.Center: true, atom.Address: true, atom.Form: true,
}

var headingLevel = map[atom.Atom]int{atom.H1: 1, atom.H2: 2, atom.H3: 3, atom.H4: 4, atom.H5: 5, atom.H6: 6}

var (
	spaceRun   = regexp.MustCompile(`[ \t\r\n\f\x{00a0}]+`)
	blankLines = regexp.MustCompile(`\n{3,}`)
)

// safeLink keeps an http(s) or mailto link target, "" for anything else.
func safeLink(href string) string {
	href = strings.TrimSpace(href)
	lower := strings.ToLower(href)
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "mailto:") {
		return ""
	}
	if strings.ContainsAny(href, " \t\r\n<>()[]\"'`") {
		return ""
	}
	return href
}

// htmlToMarkdown reduces an HTML body to Markdown text.
func htmlToMarkdown(src []byte) string {
	if len(bytes.TrimSpace(src)) == 0 {
		return ""
	}
	z := html.NewTokenizer(bytes.NewReader(src))
	var b strings.Builder
	skip := 0
	var links []string
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return cleanText([]byte(b.String()))
		case html.TextToken:
			if skip == 0 {
				b.WriteString(spaceRun.ReplaceAllString(string(z.Text()), " "))
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := atom.Lookup(name)
			if tag == atom.Body {
				// A head left open ends at the body.
				skip = 0
				continue
			}
			if skippedHTML[tag] {
				if tt == html.StartTagToken {
					skip++
				}
				continue
			}
			if skip > 0 {
				continue
			}
			switch {
			case tag == atom.Br:
				b.WriteString("\n")
			case tag == atom.Hr:
				b.WriteString("\n\n---\n\n")
			case tag == atom.Li:
				b.WriteString("\n- ")
			case headingLevel[tag] > 0:
				b.WriteString("\n\n" + strings.Repeat("#", headingLevel[tag]) + " ")
			case blockHTML[tag]:
				b.WriteString("\n\n")
			case tag == atom.Td || tag == atom.Th:
				b.WriteString(" ")
			case tag == atom.A && tt == html.StartTagToken:
				href := ""
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					if string(k) == "href" {
						href = safeLink(string(v))
					}
				}
				links = append(links, href)
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := atom.Lookup(name)
			if skippedHTML[tag] {
				if skip > 0 {
					skip--
				}
				continue
			}
			if skip > 0 {
				continue
			}
			switch {
			case headingLevel[tag] > 0, blockHTML[tag]:
				b.WriteString("\n\n")
			case tag == atom.A && len(links) > 0:
				href := links[len(links)-1]
				links = links[:len(links)-1]
				if href != "" {
					b.WriteString(" (" + href + ")")
				}
			}
		}
	}
}

// cleanText normalizes a body to Markdown-safe text: valid UTF-8, LF line
// ends, no control characters other than tab, no trailing spaces, at most
// one blank line in a row, and at most wire.MaxNoteBytes.
func cleanText(b []byte) string {
	s := strings.ToValidUTF8(string(b), "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return -1
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	s = blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return clipNote(strings.TrimSpace(s))
}

// clipNote cuts s to wire.MaxNoteBytes on a rune boundary.
func clipNote(s string) string {
	if len(s) <= wire.MaxNoteBytes {
		return s
	}
	s = s[:wire.MaxNoteBytes]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
