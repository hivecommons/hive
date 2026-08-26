package knowledge

import (
	"fmt"
	"strings"
	"testing"
)

// buildTestPDF assembles a minimal but structurally valid PDF from the given
// numbered objects (1-based), computing the xref table offsets.
func buildTestPDF(objs []string) []byte {
	var buf []byte
	buf = append(buf, []byte("%PDF-1.4\n")...)
	offsets := make([]int, len(objs)+1)
	for i, o := range objs {
		offsets[i+1] = len(buf)
		buf = append(buf, []byte(fmt.Sprintf("%d 0 obj\n%s\nendobj\n", i+1, o))...)
	}
	xref := len(buf)
	buf = append(buf, []byte(fmt.Sprintf("xref\n0 %d\n0000000000 65535 f \n", len(objs)+1))...)
	for i := 1; i <= len(objs); i++ {
		buf = append(buf, []byte(fmt.Sprintf("%010d 00000 n \n", offsets[i]))...)
	}
	buf = append(buf, []byte(fmt.Sprintf(
		"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref))...)
	return buf
}

// buildSinglePagePDF wraps one content stream and font dict into a 1-page PDF.
func buildSinglePagePDF(content, font string) []byte {
	return buildTestPDF([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		font,
	})
}

const helveticaFont = "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"

// TestParsePDFPlainTextPath covers the primary extractPageText branch where
// the PDF's own text stream (GetPlainText) succeeds.
func TestParsePDFPlainTextPath(t *testing.T) {
	content := "BT /F1 12 Tf 72 720 Td (Quarterly Report) Tj ET"
	data := buildSinglePagePDF(content, helveticaFont)

	chunks, title := parsePDF(data)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if title != "Quarterly Report" {
		t.Errorf("title = %q, want %q", title, "Quarterly Report")
	}
	if chunks[0].PageNum != 1 {
		t.Errorf("PageNum = %d, want 1", chunks[0].PageNum)
	}
	if !strings.Contains(chunks[0].Body, "Quarterly Report") {
		t.Errorf("body missing text: %q", chunks[0].Body)
	}
}

// TestParsePDFGlyphFallback covers the positioned-glyph reconstruction branch
// of extractPageText. A valid `"` (set-spacing-and-show) operator makes
// GetPlainText fail — its fallthrough to the `'` case sees three args and
// panics inside the library's recover — while Content() interprets the same
// stream correctly, so extractPageText must rebuild the text from glyph
// coordinates: same-line glyphs joined, near lines separated by \n, and far
// lines by a paragraph break (\n\n).
func TestParsePDFGlyphFallback(t *testing.T) {
	content := `BT /F1 12 Tf 20 TL 1 0 0 1 72 720 Tm (A) Tj 1 0 0 1 84 720 Tm (B) Tj 1 0 0 1 72 706 Tm (C) Tj 1 0 0 1 72 640 Tm (D) Tj 0 0 (E) " ET`
	data := buildSinglePagePDF(content, helveticaFont)

	chunks, title := parsePDF(data)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	body := chunks[0].Body
	// Glyphs sorted top-down: A and B share Y=720 (same line), C at 706 is a
	// near line (gap 14 > 12*0.8), D at 640 opens a paragraph (gap > 12*1.8),
	// and the `"` operator draws E one leading (20) below D — a near line.
	if !strings.HasPrefix(body, "AB\nC\n\nD\nE") {
		t.Errorf("reconstructed body = %q, want prefix %q", body, "AB\nC\n\nD\nE")
	}
	if title == "" {
		t.Error("expected a title from the reconstructed first line")
	}
}

// TestParsePDFGlyphFallbackWhitespaceOnly drives the fallback path where the
// reconstructed glyph text is entirely whitespace (a /Differences encoding
// maps every glyph to /space), so the page must be skipped without chunks.
func TestParsePDFGlyphFallbackWhitespaceOnly(t *testing.T) {
	content := `BT /F1 12 Tf 72 720 Td (A) Tj 72 700 Td (A) Tj 0 0 (A) " ET`
	font := "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding << /Differences [65 /space] >> >>"
	data := buildSinglePagePDF(content, font)

	chunks, title := parsePDF(data)
	if len(chunks) != 0 {
		t.Errorf("expected no chunks for whitespace-only page, got %d: %+v", len(chunks), chunks)
	}
	if title != "" {
		t.Errorf("expected empty title, got %q", title)
	}
}

// TestParsePDFSkipsEmptyPageAndSplitsLongPage covers the multi-page loop:
// an empty page is skipped (continue branch) and a page longer than
// maxChunkChars is split into multiple "Page N, Part M" chunks.
func TestParsePDFSkipsEmptyPageAndSplitsLongPage(t *testing.T) {
	// Page 1: empty content stream. Page 2: paragraphs exceeding maxChunkChars.
	var sb strings.Builder
	sb.WriteString("BT /F1 12 Tf 20 TL 72 720 Td (Long Document Title) Tj ")
	sentence := "This sentence pads the page well past the chunk limit for splitting. "
	for i := 0; i < (maxChunkChars/len(sentence))+3; i++ {
		sb.WriteString(fmt.Sprintf("T* (%s) Tj ", strings.TrimSpace(sentence)))
		if i%5 == 4 {
			sb.WriteString("T* ( ) Tj ") // blank line -> paragraph boundary
		}
	}
	sb.WriteString("ET")
	longContent := sb.String()

	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 5 0 R /Resources << /Font << /F1 7 0 R >> >> >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 6 0 R /Resources << /Font << /F1 7 0 R >> >> >>",
		"<< /Length 0 >>\nstream\n\nendstream",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(longContent), longContent),
		helveticaFont,
	}
	data := buildTestPDF(objs)

	chunks, title := parsePDF(data)
	if title != "Long Document Title" {
		t.Errorf("title = %q, want %q", title, "Long Document Title")
	}
	if len(chunks) < 2 {
		t.Fatalf("expected long page split into >=2 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.PageNum != 2 {
			t.Errorf("chunk %d PageNum = %d, want 2 (page 1 is empty)", i, c.PageNum)
		}
		if want := fmt.Sprintf("Page 2, Part %d", i+1); c.Title != want {
			t.Errorf("chunk %d title = %q, want %q", i, c.Title, want)
		}
		if len(c.Body) > maxChunkChars {
			t.Errorf("chunk %d body length %d exceeds maxChunkChars %d", i, len(c.Body), maxChunkChars)
		}
	}
}
