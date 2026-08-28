package knowledge

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseRawText(t *testing.T) {
	// Build text with several paragraphs that exceed rawTextSplitTarget.
	var paragraphs []string
	for i := 0; i < 10; i++ {
		paragraphs = append(paragraphs, fmt.Sprintf("Paragraph %d. %s", i, strings.Repeat("word ", 60)))
	}
	text := strings.Join(paragraphs, "\n\n")

	chunks := parseRawText(text)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}

	for i, c := range chunks {
		if c.Body == "" {
			t.Errorf("chunk %d has empty body", i)
		}
		if c.Title == "" {
			t.Errorf("chunk %d has empty title", i)
		}
	}
}

func TestParseRawText_Short(t *testing.T) {
	text := "This is a short paragraph.\n\nAnother short one."
	chunks := parseRawText(text)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for short text, got %d", len(chunks))
	}
	if !strings.Contains(chunks[0].Body, "short paragraph") {
		t.Error("chunk body missing expected content")
	}
}

func TestParseRawText_Empty(t *testing.T) {
	chunks := parseRawText("")
	if len(chunks) != 0 {
		t.Fatalf("expected 0 chunks for empty text, got %d", len(chunks))
	}
}

func TestParseHTMLText(t *testing.T) {
	html := `<html>
<head><title>Test Document</title></head>
<body>
<nav><a href="/">Home</a></nav>
<script>var x = 1;</script>
<style>.hidden { display: none; }</style>
<header><h1>Header</h1></header>
<main>
<p>First paragraph with &amp; entity.</p>
<p>Second paragraph with &lt;code&gt; blocks.</p>
</main>
<footer>Copyright 2024</footer>
</body></html>`

	chunks, title := parseHTMLText([]byte(html))

	if title != "Test Document" {
		t.Errorf("expected title 'Test Document', got %q", title)
	}

	if len(chunks) == 0 {
		t.Fatal("expected at least 1 chunk from HTML")
	}

	combined := ""
	for _, c := range chunks {
		combined += c.Body
	}

	if strings.Contains(combined, "var x = 1") {
		t.Error("script content should have been stripped")
	}
	if strings.Contains(combined, "display: none") {
		t.Error("style content should have been stripped")
	}
	if strings.Contains(combined, "Copyright 2024") {
		t.Error("footer content should have been stripped")
	}
	if !strings.Contains(combined, "First paragraph with & entity") {
		t.Error("HTML entities should have been decoded")
	}
}

func TestParseHTMLText_NoTitle(t *testing.T) {
	html := `<html><body><p>Just text.</p></body></html>`
	chunks, title := parseHTMLText([]byte(html))

	if title != "" {
		t.Errorf("expected empty title, got %q", title)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least 1 chunk")
	}
}

func TestParsePDF_InvalidData(t *testing.T) {
	chunks, title := parsePDF([]byte("not a pdf"))
	if len(chunks) != 0 {
		t.Fatalf("expected 0 chunks for invalid PDF, got %d", len(chunks))
	}
	if title != "" {
		t.Errorf("expected empty title for invalid PDF, got %q", title)
	}
}

func TestParsePDF_EmptyData(t *testing.T) {
	chunks, title := parsePDF(nil)
	if len(chunks) != 0 {
		t.Fatalf("expected 0 chunks for nil data, got %d", len(chunks))
	}
	if title != "" {
		t.Errorf("expected empty title for nil data, got %q", title)
	}
}

func TestChunksToFacts(t *testing.T) {
	chunks := []DocChunk{
		{Title: "Introduction", Body: "This is the intro."},
		{Title: "Methods", Body: "We used method X."},
		{Title: "Results", Body: "Results were positive."},
	}

	sourceDate := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)
	facts := chunksToFacts(chunks, "test-paper", "https://example.com/paper.pdf", sourceDate)

	// 1 summary + 3 section facts = 4
	if len(facts) != 4 {
		t.Fatalf("expected 4 facts (1 summary + 3 sections), got %d", len(facts))
	}

	// Verify summary fact.
	summary := facts[0]
	if !strings.HasPrefix(summary.Title, "Summary: ") {
		t.Errorf("summary title should start with 'Summary: ', got %q", summary.Title)
	}
	if summary.Type != FactType("reference") {
		t.Errorf("expected type 'reference', got %q", summary.Type)
	}

	hasSummaryTag := false
	for _, tag := range summary.Tags {
		if tag == "doc-summary" {
			hasSummaryTag = true
		}
	}
	if !hasSummaryTag {
		t.Error("summary fact should have 'doc-summary' tag")
	}

	// Verify section facts.
	for i := 1; i < len(facts); i++ {
		f := facts[i]
		if f.SourcePR != "doc:test-paper" {
			t.Errorf("fact %d: expected SourcePR 'doc:test-paper', got %q", i, f.SourcePR)
		}
		if f.Confidence != defaultDocConfidence {
			t.Errorf("fact %d: expected confidence %f, got %f", i, defaultDocConfidence, f.Confidence)
		}

		hasImportTag := false
		for _, tag := range f.Tags {
			if tag == "doc-import" {
				hasImportTag = true
			}
		}
		if !hasImportTag {
			t.Errorf("fact %d: missing 'doc-import' tag", i)
		}
	}
}

func TestChunksToFacts_Cap(t *testing.T) {
	// 60 chunks should produce at most maxFactsPerDocument facts.
	const chunkCount = 60
	chunks := make([]DocChunk, chunkCount)
	for i := range chunks {
		chunks[i] = DocChunk{
			Title: fmt.Sprintf("Section %d", i+1),
			Body:  fmt.Sprintf("Content for section %d.", i+1),
		}
	}

	facts := chunksToFacts(chunks, "big-doc", "", time.Now())
	if len(facts) > maxFactsPerDocument {
		t.Errorf("expected at most %d facts, got %d", maxFactsPerDocument, len(facts))
	}
	if len(facts) != maxFactsPerDocument {
		t.Errorf("expected exactly %d facts (cap hit), got %d", maxFactsPerDocument, len(facts))
	}
}

func TestChunksToFacts_Empty(t *testing.T) {
	facts := chunksToFacts(nil, "empty", "", time.Now())
	if len(facts) != 0 {
		t.Fatalf("expected 0 facts for nil chunks, got %d", len(facts))
	}
}

func TestChunksToFacts_SummaryTruncation(t *testing.T) {
	longBody := strings.Repeat("x", docSummaryMaxChars*2)
	chunks := []DocChunk{{Title: "Long", Body: longBody}}

	facts := chunksToFacts(chunks, "long-doc", "", time.Now())
	if len(facts) == 0 {
		t.Fatal("expected at least 1 fact")
	}
	if len(facts[0].Body) > docSummaryMaxChars {
		t.Errorf("summary body should be truncated to %d chars, got %d", docSummaryMaxChars, len(facts[0].Body))
	}
}

// buildTestDocx assembles a minimal in-memory .docx (OOXML zip) containing
// one WordprocessingML paragraph per input string. It only writes the
// word/document.xml entry, which is all parseDocx reads.
func buildTestDocx(t *testing.T, paragraphs []string) []byte {
	t.Helper()

	var body strings.Builder
	for _, text := range paragraphs {
		body.WriteString(`<w:p><w:r><w:t xml:space="preserve">`)
		body.WriteString(xmlEscape(text))
		body.WriteString(`</w:t></w:r></w:p>`)
	}

	documentXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body>` + body.String() + `</w:body></w:document>`

	return buildTestDocxArchive(t, documentXML)
}

// buildTestDocxArchive writes a minimal .docx zip archive whose
// word/document.xml entry holds the given raw XML content.
func buildTestDocxArchive(t *testing.T, documentXML string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("failed to create document.xml entry: %v", err)
	}
	if _, err := w.Write([]byte(documentXML)); err != nil {
		t.Fatalf("failed to write document.xml: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("failed to close test docx archive: %v", err)
	}
	return buf.Bytes()
}

// xmlEscape escapes the minimal set of characters WordprocessingML text
// content requires escaping for use inside test fixture XML.
func xmlEscape(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return replacer.Replace(s)
}

func TestParseDocx(t *testing.T) {
	paras := []string{
		"Introduction to Autoscaling",
		"Kubernetes HPA uses CPU and memory metrics to scale pods horizontally.",
		"Custom metrics adapters allow scaling based on application-specific signals.",
		"VPA adjusts resource requests based on historical usage patterns.",
	}
	data := buildTestDocx(t, paras)

	chunks, title := parseDocx(data)
	if len(chunks) == 0 {
		t.Fatal("expected at least 1 chunk from docx")
	}
	if title == "" {
		t.Error("expected a title extracted from first paragraph")
	}

	allText := ""
	for _, c := range chunks {
		allText += c.Body + " "
	}
	if !strings.Contains(allText, "Kubernetes HPA") {
		t.Error("expected parsed text to contain 'Kubernetes HPA'")
	}
}

func TestParseDocx_Empty(t *testing.T) {
	data := buildTestDocx(t, nil)

	chunks, title := parseDocx(data)
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks from empty docx, got %d", len(chunks))
	}
	if title != "" {
		t.Errorf("expected empty title from empty docx, got %q", title)
	}
}

func TestParseDocx_RoundTrip(t *testing.T) {
	paras := []string{
		"Title Line",
		"Body paragraph one.",
		"Body paragraph two with  preserved   spacing.",
	}
	data := buildTestDocx(t, paras)

	chunks, title := parseDocx(data)
	if title != "Title Line" {
		t.Errorf("expected title %q, got %q", "Title Line", title)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for short document, got %d", len(chunks))
	}

	body := chunks[0].Body
	for _, want := range paras {
		if !strings.Contains(body, want) {
			t.Errorf("expected chunk body to contain %q, got %q", want, body)
		}
	}
}

func TestParseDocx_Malformed(t *testing.T) {
	cases := map[string][]byte{
		"not a zip":   []byte("this is not a zip archive"),
		"empty input": nil,
		"zip missing document.xml": func() []byte {
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			w, err := zw.Create("word/other.xml")
			if err != nil {
				t.Fatalf("failed to create zip entry: %v", err)
			}
			if _, err := w.Write([]byte("<root/>")); err != nil {
				t.Fatalf("failed to write zip entry: %v", err)
			}
			if err := zw.Close(); err != nil {
				t.Fatalf("failed to close zip: %v", err)
			}
			return buf.Bytes()
		}(),
		"corrupt document.xml": buildTestDocxArchive(t, "<w:document><not-closed>"),
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			chunks, title := parseDocx(data)
			if chunks != nil {
				t.Errorf("%s: expected nil chunks, got %v", name, chunks)
			}
			if title != "" {
				t.Errorf("%s: expected empty title, got %q", name, title)
			}
		})
	}
}

func TestParseDocx_LargeDocument(t *testing.T) {
	var paras []string
	for i := 0; i < 100; i++ {
		paras = append(paras, fmt.Sprintf("Section %d content with enough text to be meaningful. %s",
			i, strings.Repeat("Lorem ipsum dolor sit amet. ", 10)))
	}
	data := buildTestDocx(t, paras)

	chunks, _ := parseDocx(data)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks from large docx, got %d", len(chunks))
	}
}
