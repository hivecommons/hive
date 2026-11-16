package inbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Extraction limits. The original file is stored whole; these bound the
// markdown the Inbox shows and writes into the Knowledge fact.
const (
	// uploadExtractTimeout bounds one in-process extraction.
	uploadExtractTimeout = 30 * time.Second
	// uploadMaxMarkdownBytes caps the extracted markdown (the connector
	// fact page cap).
	uploadMaxMarkdownBytes = 1 << 20
	// uploadPreviewBytes caps the fenced preview of a structured file.
	uploadPreviewBytes = 256 << 10
	// uploadJSONLPreviewLines is how many JSON Lines records the preview shows.
	uploadJSONLPreviewLines = 20
	// uploadTablePreviewRows is how many CSV/TSV rows the table shows
	// (the header row included).
	uploadTablePreviewRows = 21
	// uploadTableMaxColumns caps the columns of the table preview.
	uploadTableMaxColumns = 20
	// uploadTableCellRunes caps one table cell.
	uploadTableCellRunes = 120
	// uploadSummaryKeys is how many top-level keys a summary names.
	uploadSummaryKeys = 10
	// uploadTitleRunes bounds a title derived from content.
	uploadTitleRunes = 120
	// minFenceTicks is the shortest markdown code fence.
	minFenceTicks = 3
)

// Extracted is the text the Inbox keeps for one upload.
type Extracted struct {
	// Markdown is shown in the item, stored, and written into the fact.
	Markdown string
	// Summary is a one-line description for the listing ("JSON object,
	// 3 top-level keys: a, b, c"); empty for prose, whose first lines are
	// shown instead.
	Summary string
	// Title is derived from the content (a markdown # heading), if any.
	Title string
}

// ExtractUpload turns a sniffed upload into markdown, bounded in time and size.
func ExtractUpload(ctx context.Context, kind string, data []byte) (Extracted, *UploadError) {
	ctx, cancel := context.WithTimeout(ctx, uploadExtractTimeout)
	defer cancel()
	type result struct {
		ex  Extracted
		err *UploadError
	}
	done := make(chan result, 1)
	go func() {
		ex, err := extractUpload(ctx, kind, data)
		done <- result{ex, err}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			r.ex.Markdown = capMarkdown(r.ex.Markdown)
		}
		return r.ex, r.err
	case <-ctx.Done():
		return Extracted{}, uploadErr(UploadErrParse, "reading the file took longer than %s", uploadExtractTimeout)
	}
}

func extractUpload(ctx context.Context, kind string, data []byte) (Extracted, *UploadError) {
	if kind == UploadKindDOCX {
		return extractDOCX(ctx, data)
	}
	text := normalizeText(data)
	switch kind {
	case UploadKindMarkdown:
		return Extracted{Markdown: text, Title: markdownTitle(text)}, nil
	case UploadKindText:
		return Extracted{Markdown: text}, nil
	case UploadKindJSON:
		return extractJSON(text)
	case UploadKindJSONL:
		return extractJSONL(ctx, text)
	case UploadKindYAML:
		return extractYAML(ctx, text)
	case UploadKindCSV:
		return extractTable(ctx, text, ',', "CSV")
	case UploadKindTSV:
		return extractTable(ctx, text, '\t', "TSV")
	case UploadKindHTML:
		return extractHTML(ctx, text)
	}
	return Extracted{}, uploadErr(UploadErrUnsupported, "unsupported file type %q", kind)
}

// normalizeText strips a UTF-8 BOM and normalises newlines to \n.
func normalizeText(data []byte) string {
	s := string(bytes.TrimPrefix(data, utf8BOM))
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}

// markdownTitle returns the first "# " heading.
func markdownTitle(md string) string {
	for _, l := range strings.Split(md, "\n") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(l), "# "); ok && strings.TrimSpace(t) != "" {
			return truncateRunes(strings.TrimSpace(t), uploadTitleRunes)
		}
	}
	return ""
}

// fenced wraps body in a code fence longer than any backtick run inside it,
// so the content cannot close the fence early.
func fenced(lang, body string) string {
	longest, run := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
	}
	fence := strings.Repeat("`", max(minFenceTicks, longest+1))
	return fence + lang + "\n" + strings.TrimRight(body, "\n") + "\n" + fence
}

// previewCut bounds s to uploadPreviewBytes at a line boundary and reports
// whether it cut.
func previewCut(s string) (string, bool) {
	if len(s) <= uploadPreviewBytes {
		return s, false
	}
	cut := strings.LastIndexByte(s[:uploadPreviewBytes], '\n')
	if cut <= 0 {
		cut = uploadPreviewBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
	}
	return s[:cut], true
}

func truncatedNote(shown, total int, unit string) string {
	return fmt.Sprintf("\n\n_Preview: the first %d of %d %s. The whole file is stored on this hive._", shown, total, unit)
}

// capMarkdown bounds the extracted markdown at uploadMaxMarkdownBytes.
func capMarkdown(md string) string {
	if len(md) <= uploadMaxMarkdownBytes {
		return md
	}
	cut := uploadMaxMarkdownBytes
	for cut > 0 && !utf8.RuneStart(md[cut]) {
		cut--
	}
	return md[:cut] + fmt.Sprintf("\n\n_(truncated by hive: the text exceeds %d bytes; the whole file is stored on this hive)_", uploadMaxMarkdownBytes)
}

// jsonErrLine names the line of a JSON syntax error.
func jsonErrLine(text string, err error) string {
	var se *json.SyntaxError
	if errors.As(err, &se) && se.Offset > 0 && int(se.Offset) <= len(text) {
		return fmt.Sprintf("line %d: %s", strings.Count(text[:se.Offset], "\n")+1, se.Error())
	}
	return err.Error()
}

func extractJSON(text string) (Extracted, *UploadError) {
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return Extracted{}, uploadErr(UploadErrParse, "the file is not valid JSON (%s)", jsonErrLine(text, err))
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(text), "", "  "); err != nil {
		return Extracted{}, uploadErr(UploadErrParse, "the file is not valid JSON (%s)", err)
	}
	summary := jsonSummary(v)
	body := pretty.String()
	lines := strings.Count(body, "\n") + 1
	shown, cut := previewCut(body)
	md := summary + "\n\n" + fenced("json", shown)
	if cut {
		md += truncatedNote(strings.Count(shown, "\n")+1, lines, "lines")
	}
	return Extracted{Markdown: md, Summary: summary}, nil
}

func jsonSummary(v any) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return fmt.Sprintf("JSON object, %d top-level %s%s", len(keys), plural(len(keys), "key", "keys"), namedKeys(keys))
	case []any:
		return fmt.Sprintf("JSON array, %d %s", len(t), plural(len(t), "item", "items"))
	case string:
		return "JSON string"
	case float64:
		return "JSON number"
	case bool:
		return "JSON boolean"
	}
	return "JSON null"
}

func namedKeys(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	shown := keys
	more := ""
	if len(shown) > uploadSummaryKeys {
		shown, more = shown[:uploadSummaryKeys], ", …"
	}
	quoted := make([]string, len(shown))
	for i, k := range shown {
		quoted[i] = truncateRunes(k, uploadTableCellRunes)
	}
	return ": " + strings.Join(quoted, ", ") + more
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func extractJSONL(ctx context.Context, text string) (Extracted, *UploadError) {
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), len(text)+1)
	var preview []string
	records, line := 0, 0
	for sc.Scan() {
		line++
		if line%1024 == 0 && ctx.Err() != nil {
			return Extracted{}, uploadErr(UploadErrParse, "reading the file took too long")
		}
		l := strings.TrimSpace(sc.Text())
		if l == "" {
			continue
		}
		if !json.Valid([]byte(l)) {
			return Extracted{}, uploadErr(UploadErrParse, "the file is not valid JSON Lines: line %d is not a JSON value", line)
		}
		records++
		if len(preview) < uploadJSONLPreviewLines {
			preview = append(preview, l)
		}
	}
	if err := sc.Err(); err != nil {
		return Extracted{}, uploadErr(UploadErrParse, "the file is not valid JSON Lines (%s)", err)
	}
	if records == 0 {
		return Extracted{}, uploadErr(UploadErrEmpty, "the file has no JSON Lines records")
	}
	summary := fmt.Sprintf("JSON Lines, %d %s", records, plural(records, "record", "records"))
	shown, _ := previewCut(strings.Join(preview, "\n"))
	md := summary + "\n\n" + fenced("json", shown)
	if records > len(preview) {
		md += truncatedNote(len(preview), records, "records")
	}
	return Extracted{Markdown: md, Summary: summary}, nil
}

func extractYAML(ctx context.Context, text string) (Extracted, *UploadError) {
	dec := yaml.NewDecoder(strings.NewReader(text))
	docs := 0
	var first yaml.Node
	for {
		if ctx.Err() != nil {
			return Extracted{}, uploadErr(UploadErrParse, "reading the file took too long")
		}
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Extracted{}, uploadErr(UploadErrParse, "the file is not valid YAML (%s)", err)
		}
		if docs == 0 {
			first = n
		}
		docs++
	}
	if docs == 0 {
		return Extracted{}, uploadErr(UploadErrEmpty, "the YAML file has no documents")
	}
	summary := yamlSummary(&first, docs)
	shown, cut := previewCut(text)
	md := summary + "\n\n" + fenced("yaml", shown)
	if cut {
		md += truncatedNote(strings.Count(shown, "\n")+1, strings.Count(text, "\n")+1, "lines")
	}
	return Extracted{Markdown: md, Summary: summary}, nil
}

func yamlSummary(doc *yaml.Node, docs int) string {
	prefix := "YAML"
	if docs > 1 {
		prefix = fmt.Sprintf("YAML, %d documents; the first is", docs)
	}
	n := doc
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	switch n.Kind {
	case yaml.MappingNode:
		keys := make([]string, 0, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			keys = append(keys, n.Content[i].Value)
		}
		return fmt.Sprintf("%s a mapping, %d top-level %s%s", prefix, len(keys), plural(len(keys), "key", "keys"), namedKeys(keys))
	case yaml.SequenceNode:
		return fmt.Sprintf("%s a list, %d %s", prefix, len(n.Content), plural(len(n.Content), "item", "items"))
	}
	return prefix + " a single value"
}

func extractTable(ctx context.Context, text string, comma rune, label string) (Extracted, *UploadError) {
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = comma
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	if comma == '\t' {
		r.LazyQuotes = true
	}
	var preview [][]string
	rows, cols := 0, 0
	for {
		if rows%1024 == 0 && ctx.Err() != nil {
			return Extracted{}, uploadErr(UploadErrParse, "reading the file took too long")
		}
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Extracted{}, uploadErr(UploadErrParse, "the file is not valid %s (%s)", label, err)
		}
		rows++
		cols = max(cols, len(rec))
		if len(preview) < uploadTablePreviewRows {
			preview = append(preview, append([]string(nil), rec...))
		}
	}
	if rows == 0 {
		return Extracted{}, uploadErr(UploadErrEmpty, "the %s file has no rows", label)
	}
	dataRows := rows - 1
	summary := fmt.Sprintf("%s, %d %s × %d %s (first row as header)", label, dataRows, plural(dataRows, "row", "rows"), cols, plural(cols, "column", "columns"))
	md := summary + "\n\n" + markdownTable(preview, cols)
	if rows > len(preview) {
		md += truncatedNote(len(preview)-1, dataRows, "rows")
	}
	return Extracted{Markdown: md, Summary: summary}, nil
}

// markdownTable renders rows (the first is the header) as a markdown table,
// with pipes escaped, newlines flattened and cells and columns capped.
func markdownTable(rows [][]string, cols int) string {
	show := min(cols, uploadTableMaxColumns)
	cell := func(row []string, i int) string {
		if i >= len(row) {
			return ""
		}
		c := strings.Join(strings.Fields(row[i]), " ")
		c = strings.ReplaceAll(c, `\`, `\\`)
		c = strings.ReplaceAll(c, "|", `\|`)
		return truncateRunes(c, uploadTableCellRunes)
	}
	line := func(row []string) string {
		parts := make([]string, show)
		for i := range parts {
			parts[i] = cell(row, i)
		}
		if cols > show {
			parts = append(parts, "…")
		}
		return "| " + strings.Join(parts, " | ") + " |"
	}
	var b strings.Builder
	b.WriteString(line(rows[0]) + "\n")
	sep := make([]string, show)
	for i := range sep {
		sep[i] = "---"
	}
	if cols > show {
		sep = append(sep, "---")
	}
	b.WriteString("| " + strings.Join(sep, " | ") + " |\n")
	for _, row := range rows[1:] {
		b.WriteString(line(row) + "\n")
	}
	if cols > show {
		b.WriteString("\n_" + strconv.Itoa(cols-show) + " more columns are not shown._\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
