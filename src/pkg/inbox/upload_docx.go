package inbox

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"strconv"
	"strings"
)

// Word (.docx) extraction: the zip is opened in memory with archive/zip and
// word/document.xml is read with encoding/xml, both in Go and in-process.
// Zip bombs are refused by entry count, declared and actual uncompressed
// size and compression ratio; XML with a DOCTYPE or an entity the decoder
// does not know (XXE) is refused, and encoding/xml never fetches anything.
const (
	// docxMaxEntries bounds the zip's entries.
	docxMaxEntries = 1000
	// docxMaxPartBytes bounds one XML part once inflated.
	docxMaxPartBytes = 50 << 20
	// docxMaxTotalBytes bounds the declared uncompressed size of all entries.
	docxMaxTotalBytes = 200 << 20
	// docxMaxRatio bounds one entry's compression ratio.
	docxMaxRatio = 100
	// docxMaxListLevel caps list indentation.
	docxMaxListLevel = 8
	// docxTableMaxRows caps a rendered table.
	docxTableMaxRows = 200
	docxDocumentPart = "word/document.xml"
	docxCorePart     = "docProps/core.xml"
	docxNamespace    = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
)

// UploadKindDOCX is a Word document.
const UploadKindDOCX = "docx"

func docxErr(format string, args ...any) *UploadError {
	return uploadErr(UploadErrParse, "the file is not a readable Word document: "+format, args...)
}

// openDocx validates the archive and returns its parts by name.
func openDocx(data []byte) (map[string]*zip.File, *UploadError) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, docxErr("%s", err)
	}
	if len(zr.File) > docxMaxEntries {
		return nil, docxErr("the archive has %d entries (at most %d)", len(zr.File), docxMaxEntries)
	}
	parts := make(map[string]*zip.File, len(zr.File))
	var total uint64
	for _, f := range zr.File {
		total += f.UncompressedSize64
		if total > docxMaxTotalBytes {
			return nil, docxErr("the archive inflates past %s", humanBytes(docxMaxTotalBytes))
		}
		if f.CompressedSize64 > 0 && f.UncompressedSize64/f.CompressedSize64 > docxMaxRatio {
			return nil, docxErr("%s is compressed more than %d:1 (a zip bomb?)", f.Name, docxMaxRatio)
		}
		if f.CompressedSize64 == 0 && f.UncompressedSize64 > 0 {
			return nil, docxErr("%s has an impossible size", f.Name)
		}
		parts[f.Name] = f
	}
	if parts[docxDocumentPart] == nil {
		return nil, docxErr("%s is missing", docxDocumentPart)
	}
	return parts, nil
}

// readDocxPart inflates one part, refusing more than docxMaxPartBytes even
// when the zip header claims less.
func readDocxPart(f *zip.File) ([]byte, *UploadError) {
	if f.UncompressedSize64 > docxMaxPartBytes {
		return nil, docxErr("%s is larger than %s", f.Name, humanBytes(docxMaxPartBytes))
	}
	rc, err := f.Open()
	if err != nil {
		return nil, docxErr("%s", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, docxMaxPartBytes+1))
	if err != nil {
		return nil, docxErr("%s", err)
	}
	if len(data) > docxMaxPartBytes {
		return nil, docxErr("%s is larger than %s", f.Name, humanBytes(docxMaxPartBytes))
	}
	return data, nil
}

// newSafeXMLDecoder is strict and knows no entities beyond the predefined
// five, so &xxe; fails; DOCTYPE directives are refused by the callers.
func newSafeXMLDecoder(data []byte) *xml.Decoder {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	d.Entity = nil
	return d
}

// errDocxDTD is returned for a part that declares a DTD.
var errDocxDTD = errors.New("a DOCTYPE (DTD) is not allowed")

func xmlDirectiveIsDTD(d xml.Directive) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(string(d))), "DOCTYPE")
}

func extractDOCX(ctx context.Context, data []byte) (Extracted, *UploadError) {
	parts, uerr := openDocx(data)
	if uerr != nil {
		return Extracted{}, uerr
	}
	doc, uerr := readDocxPart(parts[docxDocumentPart])
	if uerr != nil {
		return Extracted{}, uerr
	}
	md, heading, err := docxMarkdown(ctx, doc)
	if err != nil {
		return Extracted{}, docxErr("%s", err)
	}
	if md == "" {
		return Extracted{}, uploadErr(UploadErrEmpty, "the Word document has no text")
	}
	title := heading
	if f := parts[docxCorePart]; f != nil {
		if core, uerr := readDocxPart(f); uerr == nil {
			if t := docxCoreTitle(core); t != "" {
				title = t
			}
		}
	}
	return Extracted{Markdown: md, Title: truncateRunes(title, uploadTitleRunes)}, nil
}

// docxCoreTitle reads dc:title from docProps/core.xml.
func docxCoreTitle(core []byte) string {
	d := newSafeXMLDecoder(core)
	in := false
	var b strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.Directive:
			if xmlDirectiveIsDTD(t) {
				return ""
			}
		case xml.StartElement:
			in = t.Name.Local == "title"
		case xml.CharData:
			if in {
				b.Write(t)
			}
		case xml.EndElement:
			if in && t.Name.Local == "title" {
				return oneLine(b.String())
			}
		}
	}
	return ""
}

type docxTable struct {
	rows [][]string
	row  []string
	cell []string
}

type docxPara struct {
	text    strings.Builder
	heading int
	list    bool
	level   int
}

// docxMarkdown streams document.xml into markdown: headings from Heading
// and Title styles, list items from numbering, tables as markdown tables.
// It returns the first heading too.
func docxMarkdown(ctx context.Context, doc []byte) (string, string, error) {
	d := newSafeXMLDecoder(doc)
	var out strings.Builder
	var tables []*docxTable
	var p *docxPara
	firstHeading := ""
	inText := false
	// afterList ends a list with a blank line before the next block.
	afterList := false
	endList := func() {
		if afterList {
			out.WriteString("\n")
			afterList = false
		}
	}
	tokens := 0
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", "", err
		}
		tokens++
		if tokens%4096 == 0 && ctx.Err() != nil {
			return "", "", errors.New("reading the document took too long")
		}
		switch t := tok.(type) {
		case xml.Directive:
			if xmlDirectiveIsDTD(t) {
				return "", "", errDocxDTD
			}
		case xml.StartElement:
			if t.Name.Space != docxNamespace {
				continue
			}
			switch t.Name.Local {
			case "p":
				p = &docxPara{}
			case "pStyle":
				if p != nil {
					p.heading = docxHeadingLevel(docxAttr(t, "val"))
				}
			case "numPr":
				if p != nil {
					p.list = true
				}
			case "ilvl":
				if p != nil {
					if n, err := strconv.Atoi(docxAttr(t, "val")); err == nil && n > 0 {
						p.level = min(n, docxMaxListLevel)
					}
				}
			case "t":
				inText = true
			case "tab":
				if p != nil {
					p.text.WriteByte(' ')
				}
			case "br", "cr":
				if p != nil {
					p.text.WriteByte(' ')
				}
			case "tbl":
				tables = append(tables, &docxTable{})
			case "tr":
				if len(tables) > 0 {
					tables[len(tables)-1].row = nil
				}
			case "tc":
				if len(tables) > 0 {
					tables[len(tables)-1].cell = nil
				}
			}
		case xml.CharData:
			if inText && p != nil {
				p.text.Write(t)
			}
		case xml.EndElement:
			if t.Name.Space != docxNamespace {
				continue
			}
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				if p == nil {
					continue
				}
				text := oneLine(p.text.String())
				if len(tables) > 0 {
					if text != "" {
						tb := tables[len(tables)-1]
						tb.cell = append(tb.cell, text)
					}
				} else if text != "" {
					if p.heading > 0 && firstHeading == "" {
						firstHeading = text
					}
					if !p.list || p.heading > 0 {
						endList()
					}
					out.WriteString(docxBlock(p, text))
					afterList = p.list && p.heading == 0
				}
				p = nil
			case "tc":
				if len(tables) > 0 {
					tb := tables[len(tables)-1]
					tb.row = append(tb.row, strings.Join(tb.cell, " "))
				}
			case "tr":
				if len(tables) > 0 {
					tb := tables[len(tables)-1]
					if len(tb.row) > 0 && len(tb.rows) < docxTableMaxRows {
						tb.rows = append(tb.rows, tb.row)
					}
				}
			case "tbl":
				if len(tables) == 0 {
					continue
				}
				tb := tables[len(tables)-1]
				tables = tables[:len(tables)-1]
				if len(tb.rows) == 0 {
					continue
				}
				if len(tables) > 0 {
					// A nested table becomes text in its parent's cell.
					parent := tables[len(tables)-1]
					for _, r := range tb.rows {
						parent.cell = append(parent.cell, strings.Join(r, " "))
					}
					continue
				}
				cols := 0
				for _, r := range tb.rows {
					cols = max(cols, len(r))
				}
				endList()
				out.WriteString(markdownTable(tb.rows, cols) + "\n\n")
			}
		}
	}
	return strings.TrimSpace(out.String()), firstHeading, nil
}

func docxBlock(p *docxPara, text string) string {
	switch {
	case p.heading > 0:
		return strings.Repeat("#", p.heading) + " " + text + "\n\n"
	case p.list:
		return strings.Repeat("  ", p.level) + "- " + text + "\n"
	}
	return text + "\n\n"
}

// docxHeadingLevel maps a paragraph style id to a heading level: Title is
// 1, Heading1..Heading6 are 1..6.
func docxHeadingLevel(style string) int {
	s := strings.ToLower(strings.ReplaceAll(style, " ", ""))
	if s == "title" {
		return 1
	}
	if rest, ok := strings.CutPrefix(s, "heading"); ok {
		if n, err := strconv.Atoi(rest); err == nil && n >= 1 {
			return min(n, htmlHeadingLevels)
		}
	}
	return 0
}

func docxAttr(t xml.StartElement, local string) string {
	for _, a := range t.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// docxLooksLikeZip reports a zip signature.
func docxLooksLikeZip(data []byte) bool {
	return bytes.HasPrefix(data, []byte("PK\x03\x04"))
}
