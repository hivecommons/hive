package knowledge

import (
	"fmt"
	"regexp"
	"strings"
	"time"
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
)

// DocChunk is a section of a parsed document, ready for conversion to a fact.
type DocChunk struct {
	Title   string
	Body    string
	PageNum int
	Section string
}

// parsePDFText splits already-extracted PDF text into chunks by page boundary.
// Pages are delimited by form-feed characters (\f). Pages exceeding
// maxChunkChars are split further at paragraph boundaries.
func parsePDFText(text string) []DocChunk {
	pages := strings.Split(text, "\f")
	var chunks []DocChunk

	for i, page := range pages {
		page = strings.TrimSpace(page)
		if page == "" {
			continue
		}
		pageNum := i + 1

		if len(page) <= maxChunkChars {
			chunks = append(chunks, DocChunk{
				Title:   extractPageTitle(page, pageNum),
				Body:    page,
				PageNum: pageNum,
			})
			continue
		}

		subChunks := splitAtParagraphs(page, maxChunkChars)
		for j, sub := range subChunks {
			title := extractPageTitle(sub, pageNum)
			if len(subChunks) > 1 {
				title = fmt.Sprintf("Page %d, Part %d", pageNum, j+1)
			}
			chunks = append(chunks, DocChunk{
				Title:   title,
				Body:    sub,
				PageNum: pageNum,
			})
		}
	}
	return chunks
}

// extractPageTitle returns the first line if it looks like a heading (short, no
// trailing period), otherwise "Page N".
func extractPageTitle(text string, pageNum int) string {
	firstLine := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if firstLine != "" && len(firstLine) <= chunkTitleMaxChars && !strings.HasSuffix(firstLine, ".") {
		return firstLine
	}
	return fmt.Sprintf("Page %d", pageNum)
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

// extractChunkTitle returns the first sentence (up to chunkTitleMaxChars) or
// falls back to "Section N".
func extractChunkTitle(body string, sectionNum int) string {
	firstLine := strings.SplitN(body, "\n", 2)[0]
	firstLine = strings.TrimSpace(firstLine)

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
		Title:      "Summary: " + chunks[0].Title,
		Body:       summaryBody,
		Type:       FactReference,
		Confidence: defaultDocConfidence,
		Tags:       []string{"doc-import", sourceSlug, "doc-summary"},
		SourcePR:   sourcePR,
		SourceDate: sourceDate,
	})

	// Section facts from all chunks (including the first, which may have
	// content beyond the summary).
	for _, chunk := range chunks {
		if len(facts) >= maxFactsPerDocument {
			break
		}
		facts = append(facts, ExtractedFact{
			Title:      chunk.Title,
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
