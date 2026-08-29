package knowledge

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/ledongthuc/pdf"
)

const (
	// maxChunkChars is the maximum characters per chunk before splitting further.
	maxChunkChars = 2000

	// maxFactsPerDocument caps how many facts a single document can produce.
	maxFactsPerDocument = 50

	// defaultDocConfidence is the confidence score for externally-sourced facts.
	defaultDocConfidence = 0.6

	// docSummaryMaxChars is the max body length for the summary fact.
	docSummaryMaxChars = 500

	// rawTextSplitTarget is the target chunk size when splitting raw text.
	rawTextSplitTarget = 1500

	// chunkTitleMaxChars caps the auto-extracted title from chunk content.
	chunkTitleMaxChars = 80

	// docxDocumentEntry is the path inside a .docx (OOXML zip) archive that
	// holds the WordprocessingML body text.
	docxDocumentEntry = "word/document.xml"

	// docxMaxEntrySize bounds how much of a single zip entry we will read,
	// since .docx uploads are attacker-influenced input.
	docxMaxEntrySize = 64 << 20 // 64MiB
)

// DocChunk is a section of a parsed document, ready for conversion to a fact.
type DocChunk struct {
	Title   string
	Body    string
	PageNum int
	Section string
}

// pdfLineGapRatio is the fraction of font size that triggers a newline between
// text segments on different lines.
const pdfLineGapRatio = 0.8

// parsePDF extracts text from binary PDF data using ledongthuc/pdf's positioned
// text objects, reconstructing word spacing from glyph coordinates.
func parsePDF(data []byte) ([]DocChunk, string) {
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, ""
	}

	numPages := reader.NumPage()
	if numPages == 0 {
		return nil, ""
	}

	var chunks []DocChunk
	var docTitle string

	for i := 1; i <= numPages; i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}

		text := extractPageText(page)
		if strings.TrimSpace(text) == "" {
			continue
		}
		text = strings.TrimSpace(text)

		if docTitle == "" {
			firstLine := strings.SplitN(text, "\n", 2)[0]
			firstLine = strings.TrimSpace(firstLine)
			if len(firstLine) > 0 && len(firstLine) <= chunkTitleMaxChars && !strings.HasSuffix(firstLine, ".") {
				docTitle = firstLine
			}
		}

		if len(text) <= maxChunkChars {
			chunks = append(chunks, DocChunk{
				Title:   extractPageTitle(text, i),
				Body:    text,
				PageNum: i,
			})
			continue
		}

		subChunks := splitAtParagraphs(text, maxChunkChars)
		for j, sub := range subChunks {
			title := extractPageTitle(sub, i)
			if len(subChunks) > 1 {
				title = fmt.Sprintf("Page %d, Part %d", i, j+1)
			}
			chunks = append(chunks, DocChunk{
				Title:   title,
				Body:    sub,
				PageNum: i,
			})
		}
	}
	return chunks, docTitle
}

// extractPageText extracts readable text from a PDF page. Prefers the PDF's
// own text stream (GetPlainText) which preserves correct character ordering.
// Falls back to positioned-glyph reconstruction only when GetPlainText fails.
func extractPageText(page pdf.Page) (out string) {
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()

	text, err := page.GetPlainText(nil)
	if err == nil && strings.TrimSpace(text) != "" {
		result := strings.ReplaceAll(text, "\r\n", "\n")
		result = strings.ReplaceAll(result, "\r", "\n")
		return sanitizePDFText(result)
	}

	content := page.Content()
	if len(content.Text) == 0 {
		return ""
	}

	texts := make([]pdf.Text, len(content.Text))
	copy(texts, content.Text)
	sort.Slice(texts, func(i, j int) bool {
		if math.Abs(texts[i].Y-texts[j].Y) > texts[i].FontSize*pdfLineGapRatio {
			return texts[i].Y > texts[j].Y
		}
		return texts[i].X < texts[j].X
	})

	var buf strings.Builder
	var prevY, prevFontSize float64
	first := true

	for _, t := range texts {
		fontSize := t.FontSize
		if fontSize <= 0 {
			fontSize = 12
		}

		if first {
			buf.WriteString(t.S)
			first = false
		} else {
			yDiff := math.Abs(t.Y - prevY)
			lineThreshold := prevFontSize * pdfLineGapRatio
			if yDiff > lineThreshold {
				paragraphThreshold := prevFontSize * 1.8
				if yDiff > paragraphThreshold {
					buf.WriteString("\n\n")
				} else {
					buf.WriteString("\n")
				}
			}
			buf.WriteString(t.S)
		}

		prevY = t.Y
		prevFontSize = fontSize
	}

	result := buf.String()
	result = strings.ReplaceAll(result, "\r\n", "\n")
	result = strings.ReplaceAll(result, "\r", "\n")
	return sanitizePDFText(result)
}

// pdfLigatures expands common typographic ligatures back to ASCII.
var pdfLigatures = strings.NewReplacer(
	"û00", "ff",
	"û01", "fi",
	"û02", "fl",
	"û03", "ffi",
	"û04", "ffl",
	"–", "-",
	"—", "-",
	"‘", "'",
	"’", "'",
	"“", "\"",
	"”", "\"",
	"…", "...",
	" ", " ",
)

// isDecorative returns true for Unicode runes that are PDF text artifacts:
// control chars, bullets, geometric shapes, dingbats, private-use area,
// BOM, and replacement characters.
func isDecorative(r rune) bool {
	if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
		return true
	}
	return r == 0x00AD || // soft hyphen
		unicode.Is(unicode.Co, r) || // private use area
		(r >= 0x2700 && r <= 0x27BF) || // dingbats
		(r >= 0x25A0 && r <= 0x25FF) || // geometric shapes
		(r >= 0x2600 && r <= 0x26FF) || // miscellaneous symbols
		r == 0xFEFF || // BOM
		r == 0xFFFC || // object replacement
		r == 0xFFFD || // replacement character
		r == 0x2022 || // bullet
		r == 0x2023 || // triangular bullet
		r == 0x2043 || // hyphen bullet
		r == 0x204C || // black leftwards bullet
		r == 0x204D // black rightwards bullet
}

// sanitizePDFText strips decorative glyphs, normalizes whitespace artifacts,
// and expands common ligatures that PDF text extractors emit.
func sanitizePDFText(s string) string {
	s = pdfLigatures.Replace(s)

	s = strings.Map(func(r rune) rune {
		if isDecorative(r) {
			return -1
		}
		return r
	}, s)

	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// extractPageTitle returns the first line if it looks like a heading (short, no
// trailing period), otherwise "Page N".
func extractPageTitle(text string, pageNum int) string {
	firstLine := stripHeadingMarkers(strings.SplitN(text, "\n", 2)[0])
	if firstLine != "" && len(firstLine) <= chunkTitleMaxChars && !strings.HasSuffix(firstLine, ".") {
		return firstLine
	}
	return fmt.Sprintf("Page %d", pageNum)
}

// docxText models a WordprocessingML <w:t> run-text element. It is matched
// by local name (ignoring the "w:" namespace prefix) so the parser does not
// need to resolve the full OOXML namespace. xml:space="preserve" content
// must be kept verbatim, so the element's chardata is captured as-is.
type docxText struct {
	Text string `xml:",chardata"`
}

// docxRun models a WordprocessingML <w:r> run: a span of text with uniform
// formatting. Only the text children matter for extraction.
type docxRun struct {
	Text []docxText `xml:"t"`
}

// docxParagraph models a WordprocessingML <w:p> paragraph, made up of runs.
type docxParagraph struct {
	Runs []docxRun `xml:"r"`
}

// docxBody models the WordprocessingML <w:body> element containing the
// document's paragraphs.
type docxBody struct {
	Paragraphs []docxParagraph `xml:"p"`
}

// docxDocument models the WordprocessingML root <w:document> element.
type docxDocument struct {
	Body docxBody `xml:"body"`
}

// extractDocxDocumentXML reads the word/document.xml entry out of a .docx
// (OOXML zip) archive. It returns an error if the archive is malformed or
// the entry is missing, so callers can fail closed.
func extractDocxDocumentXML(data []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}

	for _, f := range zr.File {
		if f.Name != docxDocumentEntry {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }() // read-only zip entry; nothing to lose on close error

		limited := io.LimitReader(rc, docxMaxEntrySize)
		return io.ReadAll(limited)
	}

	return nil, fmt.Errorf("knowledge: %s not found in docx archive", docxDocumentEntry)
}

// parseDocx extracts text from a .docx file and splits into chunks.
// It returns the chunks and the extracted document title (empty if none).
//
// A .docx is a ZIP archive whose body text lives in word/document.xml as
// WordprocessingML: <w:body> contains <w:p> paragraphs, each made of <w:r>
// runs, each made of <w:t> text nodes. Extraction concatenates the text of
// each paragraph's runs, trims it, and keeps non-empty lines — mirroring
// the previous go-docx-based behavior exactly.
func parseDocx(data []byte) ([]DocChunk, string) {
	docXML, err := extractDocxDocumentXML(data)
	if err != nil {
		return nil, ""
	}

	var doc docxDocument
	if err := xml.Unmarshal(docXML, &doc); err != nil {
		return nil, ""
	}

	var paragraphs []string
	for _, p := range doc.Body.Paragraphs {
		var line strings.Builder
		for _, run := range p.Runs {
			for _, t := range run.Text {
				line.WriteString(t.Text)
			}
		}
		if text := strings.TrimSpace(line.String()); text != "" {
			paragraphs = append(paragraphs, text)
		}
	}

	if len(paragraphs) == 0 {
		return nil, ""
	}

	title := ""
	if len(paragraphs[0]) <= chunkTitleMaxChars && !strings.HasSuffix(paragraphs[0], ".") {
		title = paragraphs[0]
	}

	fullText := strings.Join(paragraphs, "\n\n")
	chunks := parseRawText(fullText)
	return chunks, title
}

var (
	reScript = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyle  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reNav    = regexp.MustCompile(`(?is)<nav[^>]*>.*?</nav>`)
	reHeader = regexp.MustCompile(`(?is)<header[^>]*>.*?</header>`)
	reFooter = regexp.MustCompile(`(?is)<footer[^>]*>.*?</footer>`)
	reTitle  = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reTags   = regexp.MustCompile(`<[^>]*>`)
)

// parseHTMLText strips HTML to plain text and splits into chunks. It returns
// the chunks and the extracted <title> (empty string if none found).
func parseHTMLText(htmlBytes []byte) ([]DocChunk, string) {
	html := string(htmlBytes)

	// Extract title before stripping.
	var title string
	if m := reTitle.FindStringSubmatch(html); len(m) > 1 {
		title = strings.TrimSpace(decodeHTMLEntities(m[1]))
	}

	// Remove non-content blocks.
	html = reScript.ReplaceAllString(html, "")
	html = reStyle.ReplaceAllString(html, "")
	html = reNav.ReplaceAllString(html, "")
	html = reHeader.ReplaceAllString(html, "")
	html = reFooter.ReplaceAllString(html, "")

	// Strip remaining tags.
	text := reTags.ReplaceAllString(html, "")
	text = decodeHTMLEntities(text)

	// Collapse whitespace runs but preserve paragraph breaks.
	text = collapseWhitespace(text)

	chunks := parseRawText(text)
	return chunks, title
}

// decodeHTMLEntities replaces common HTML entities with their plain-text form.
func decodeHTMLEntities(s string) string {
	replacer := strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&apos;", "'",
		"&nbsp;", " ",
	)
	return replacer.Replace(s)
}

// collapseWhitespace normalises whitespace: runs of spaces/tabs become a single
// space; runs of 2+ newlines become a double-newline paragraph break.
func collapseWhitespace(s string) string {
	// Normalise line endings.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	// Collapse blank-line runs into exactly two newlines.
	reBlankRun := regexp.MustCompile(`\n{3,}`)
	s = reBlankRun.ReplaceAllString(s, "\n\n")

	// Within each line, collapse horizontal whitespace.
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// parseRawText splits plain text into chunks of approximately
// rawTextSplitTarget characters, breaking at paragraph boundaries.
func parseRawText(text string) []DocChunk {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	paragraphs := strings.Split(text, "\n\n")
	var chunks []DocChunk
	var current strings.Builder
	sectionNum := 1

	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}

		if current.Len() > 0 && current.Len()+len(para)+2 > rawTextSplitTarget {
			body := current.String()
			chunks = append(chunks, DocChunk{
				Title: extractChunkTitle(body, sectionNum),
				Body:  body,
			})
			current.Reset()
			sectionNum++
		}

		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(para)
	}

	if current.Len() > 0 {
		body := current.String()
		chunks = append(chunks, DocChunk{
			Title: extractChunkTitle(body, sectionNum),
			Body:  body,
		})
	}
	return chunks
}

// headingMarkerRe matches leading markdown heading syntax ("# ".."###### ")
// so auto-extracted titles don't carry raw markdown into fact titles.
var headingMarkerRe = regexp.MustCompile(`^\s*#{1,6}\s+`)

// dividerOnlyRe matches content made only of divider characters
// (horizontal rules, setext underlines) and whitespace.
var dividerOnlyRe = regexp.MustCompile(`^[\s\-=_*]+$`)

// stripHeadingMarkers removes leading markdown heading syntax from a title.
func stripHeadingMarkers(s string) string {
	return strings.TrimSpace(headingMarkerRe.ReplaceAllString(s, ""))
}

// isDividerOnly reports whether the string is empty or made only of divider
// characters — such content carries no information and should not become a fact.
func isDividerOnly(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || dividerOnlyRe.MatchString(s)
}

// extractChunkTitle returns the first sentence (up to chunkTitleMaxChars) or
// falls back to "Section N".
func extractChunkTitle(body string, sectionNum int) string {
	firstLine := strings.SplitN(body, "\n", 2)[0]
	firstLine = stripHeadingMarkers(firstLine)

	// Try first sentence (period-delimited).
	if idx := strings.Index(firstLine, ". "); idx > 0 && idx < chunkTitleMaxChars {
		return firstLine[:idx]
	}

	if len(firstLine) > 0 && len(firstLine) <= chunkTitleMaxChars {
		return firstLine
	}

	if len(firstLine) > chunkTitleMaxChars {
		return firstLine[:chunkTitleMaxChars]
	}

	return fmt.Sprintf("Section %d", sectionNum)
}

// splitAtParagraphs splits text into pieces of at most maxLen characters,
// breaking at double-newline paragraph boundaries where possible.
func splitAtParagraphs(text string, maxLen int) []string {
	paragraphs := strings.Split(text, "\n\n")
	var result []string
	var current strings.Builder

	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}

		if current.Len() > 0 && current.Len()+len(para)+2 > maxLen {
			result = append(result, current.String())
			current.Reset()
		}

		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(para)
	}

	if current.Len() > 0 {
		result = append(result, current.String())
	}
	return result
}

// chunksToFacts converts parsed document chunks into ExtractedFact values ready
// for vault ingestion. The first fact is a summary; subsequent facts are
// individual sections. The total is capped at maxFactsPerDocument.
func chunksToFacts(chunks []DocChunk, sourceSlug, sourceURL string, sourceDate time.Time) []ExtractedFact {
	if len(chunks) == 0 {
		return nil
	}

	tags := []string{"doc-import", sourceSlug}
	sourcePR := "doc:" + sourceSlug

	var facts []ExtractedFact

	// Summary fact from the first chunk.
	summaryBody := chunks[0].Body
	if len(summaryBody) > docSummaryMaxChars {
		summaryBody = summaryBody[:docSummaryMaxChars]
	}
	facts = append(facts, ExtractedFact{
		Title:      "Summary: " + stripHeadingMarkers(chunks[0].Title),
		Body:       summaryBody,
		Type:       FactReference,
		Confidence: defaultDocConfidence,
		Tags:       []string{"doc-import", sourceSlug, "doc-summary"},
		SourcePR:   sourcePR,
		SourceDate: sourceDate,
	})

	// Section facts from all chunks (including the first, which may have
	// content beyond the summary). Divider-only chunks (horizontal rules,
	// setext underlines) carry no information and are dropped.
	for _, chunk := range chunks {
		if len(facts) >= maxFactsPerDocument {
			break
		}
		if isDividerOnly(chunk.Title) && isDividerOnly(chunk.Body) {
			continue
		}
		facts = append(facts, ExtractedFact{
			Title:      stripHeadingMarkers(chunk.Title),
			Body:       chunk.Body,
			Type:       FactReference,
			Confidence: defaultDocConfidence,
			Tags:       append([]string{}, tags...),
			SourcePR:   sourcePR,
			SourceDate: sourceDate,
		})
	}

	if len(facts) > maxFactsPerDocument {
		facts = facts[:maxFactsPerDocument]
	}
	return facts
}
