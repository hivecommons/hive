package inbox

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func extractFixture(t *testing.T, kind, file string) Extracted {
	t.Helper()
	ex, err := ExtractUpload(context.Background(), kind, readUploadFixture(t, file))
	if err != nil {
		t.Fatalf("ExtractUpload(%s, %s): %v", kind, file, err)
	}
	return ex
}

func TestExtractMarkdownAndText(t *testing.T) {
	ex := extractFixture(t, UploadKindMarkdown, "notes.md")
	if strings.Contains(ex.Markdown, "\r") {
		t.Fatalf("CRLF not normalised: %q", ex.Markdown)
	}
	if ex.Title != "Launch plan" || !strings.HasPrefix(ex.Markdown, "# Launch plan\n\nShip the drop zone first.") || ex.Summary != "" {
		t.Fatalf("markdown = %+v", ex)
	}
	ex, err := ExtractUpload(context.Background(), UploadKindText, []byte("\xef\xbb\xbfline one\rline two\r\n"))
	if err != nil || ex.Markdown != "line one\nline two" || ex.Title != "" {
		t.Fatalf("text = %+v, %v", ex, err)
	}
}

func TestExtractJSON(t *testing.T) {
	ex := extractFixture(t, UploadKindJSON, "data.json")
	if ex.Summary != "JSON object, 3 top-level keys: name, tags, version" {
		t.Fatalf("summary = %q", ex.Summary)
	}
	if !strings.Contains(ex.Markdown, "```json\n{\n  \"name\": \"hive\",") || !strings.HasSuffix(ex.Markdown, "\n```") {
		t.Fatalf("markdown = %q", ex.Markdown)
	}
	for in, want := range map[string]string{`[1,2,3]`: "JSON array, 3 items", `[1]`: "JSON array, 1 item", `"s"`: "JSON string", `4`: "JSON number", `true`: "JSON boolean", `null`: "JSON null", `{"k":1}`: "JSON object, 1 top-level key: k", `{}`: "JSON object, 0 top-level keys"} {
		ex, err := ExtractUpload(context.Background(), UploadKindJSON, []byte(in))
		if err != nil || ex.Summary != want {
			t.Errorf("%s: summary %q, %v; want %q", in, ex.Summary, err, want)
		}
	}
	var b strings.Builder
	b.WriteString("{")
	for i := range 12 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"k%02d":%d`, i, i)
	}
	b.WriteString("}")
	ex, err := ExtractUpload(context.Background(), UploadKindJSON, []byte(b.String()))
	if err != nil || !strings.HasSuffix(ex.Summary, "k09, …") || !strings.Contains(ex.Summary, "12 top-level keys") {
		t.Fatalf("long key list = %q, %v", ex.Summary, err)
	}
}

func TestExtractMalformedJSONNamesTheLine(t *testing.T) {
	_, err := ExtractUpload(context.Background(), UploadKindJSON, readUploadFixture(t, "broken.json"))
	if err == nil || err.Code != UploadErrParse || !strings.Contains(err.Message, "not valid JSON") || !strings.Contains(err.Message, "line 3") {
		t.Fatalf("broken json = %v", err)
	}
}

func TestExtractJSONL(t *testing.T) {
	ex := extractFixture(t, UploadKindJSONL, "events.jsonl")
	if ex.Summary != "JSON Lines, 3 records" || !strings.Contains(ex.Markdown, `{"id":3,"ok":true}`) || strings.Contains(ex.Markdown, "Preview:") {
		t.Fatalf("jsonl = %+v", ex)
	}
	var b strings.Builder
	for i := range uploadJSONLPreviewLines + 5 {
		fmt.Fprintf(&b, "{\"n\":%d}\n", i)
	}
	ex, err := ExtractUpload(context.Background(), UploadKindJSONL, []byte(b.String()))
	if err != nil || !strings.Contains(ex.Markdown, fmt.Sprintf("the first %d of %d records", uploadJSONLPreviewLines, uploadJSONLPreviewLines+5)) || strings.Contains(ex.Markdown, fmt.Sprintf(`{"n":%d}`, uploadJSONLPreviewLines)) {
		t.Fatalf("jsonl preview = %q, %v", ex.Markdown, err)
	}
	_, err = ExtractUpload(context.Background(), UploadKindJSONL, []byte("{\"a\":1}\n{oops}\n"))
	if err == nil || err.Code != UploadErrParse || !strings.Contains(err.Message, "line 2") {
		t.Fatalf("bad jsonl = %v", err)
	}
	if _, err := ExtractUpload(context.Background(), UploadKindJSONL, []byte("\n\n  \n")); err == nil || err.Code != UploadErrEmpty {
		t.Fatalf("empty jsonl = %v", err)
	}
}

func TestExtractYAML(t *testing.T) {
	ex := extractFixture(t, UploadKindYAML, "config.yaml")
	if ex.Summary != "YAML a mapping, 2 top-level keys: inbox, name" || !strings.Contains(ex.Markdown, "```yaml\ninbox:\n  uploads:") {
		t.Fatalf("yaml = %+v", ex)
	}
	ex, err := ExtractUpload(context.Background(), UploadKindYAML, []byte("- a\n- b\n---\nk: v\n"))
	if err != nil || ex.Summary != "YAML, 2 documents; the first is a list, 2 items" {
		t.Fatalf("multi-doc yaml = %q, %v", ex.Summary, err)
	}
	if ex, err := ExtractUpload(context.Background(), UploadKindYAML, []byte("just words")); err != nil || ex.Summary != "YAML a single value" {
		t.Fatalf("scalar yaml = %q, %v", ex.Summary, err)
	}
	if _, err := ExtractUpload(context.Background(), UploadKindYAML, []byte("a: [1, 2\nb: c\n")); err == nil || err.Code != UploadErrParse {
		t.Fatalf("bad yaml = %v", err)
	}
	// An alias bomb is refused by the YAML library instead of expanding.
	bomb := "a: &a [\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\"]\n"
	prev := "a"
	for _, n := range []string{"b", "c", "d", "e", "f", "g", "h", "i"} {
		bomb += n + ": &" + n + " [" + strings.Repeat("*"+prev+",", 8) + "*" + prev + "]\n"
		prev = n
	}
	if _, err := ExtractUpload(context.Background(), UploadKindYAML, []byte(bomb)); err != nil && err.Code != UploadErrParse {
		t.Fatalf("alias bomb = %v", err)
	}
}

func TestExtractCSVAndTSV(t *testing.T) {
	ex := extractFixture(t, UploadKindCSV, "table.csv")
	if ex.Summary != "CSV, 2 rows × 3 columns (first row as header)" {
		t.Fatalf("summary = %q", ex.Summary)
	}
	for _, want := range []string{"| name | role | note |", "| --- | --- | --- |", `| Ana | owner | likes \| pipes |`, "| Bo | reader | multi line |"} {
		if !strings.Contains(ex.Markdown, want) {
			t.Errorf("csv markdown missing %q:\n%s", want, ex.Markdown)
		}
	}
	ex = extractFixture(t, UploadKindTSV, "table.tsv")
	if ex.Summary != "TSV, 1 row × 2 columns (first row as header)" || !strings.Contains(ex.Markdown, "| Ana | owner |") {
		t.Fatalf("tsv = %+v", ex)
	}
	if _, err := ExtractUpload(context.Background(), UploadKindCSV, []byte("a,b\n\"open,c\n")); err == nil || err.Code != UploadErrParse {
		t.Fatalf("bad csv = %v", err)
	}
	var b strings.Builder
	header := make([]string, uploadTableMaxColumns+2)
	for i := range header {
		header[i] = fmt.Sprintf("c%d", i)
	}
	b.WriteString(strings.Join(header, ",") + "\n")
	for i := range uploadTablePreviewRows + 3 {
		fmt.Fprintf(&b, "%d%s\n", i, strings.Repeat(",x", len(header)-1))
	}
	ex, err := ExtractUpload(context.Background(), UploadKindCSV, []byte(b.String()))
	if err != nil || !strings.Contains(ex.Markdown, "2 more columns are not shown") || !strings.Contains(ex.Markdown, fmt.Sprintf("the first %d of %d rows", uploadTablePreviewRows-1, uploadTablePreviewRows+3)) {
		t.Fatalf("wide csv = %q, %v", ex.Markdown, err)
	}
}

func TestFencedOutlivesBacktickRuns(t *testing.T) {
	got := fenced("json", "a ```` b")
	if !strings.HasPrefix(got, "`````json\n") || !strings.HasSuffix(got, "\n`````") {
		t.Fatalf("fence = %q", got)
	}
}

func TestExtractCapsAndTimeout(t *testing.T) {
	big := strings.Repeat("word ", uploadMaxMarkdownBytes/4)
	ex, err := ExtractUpload(context.Background(), UploadKindText, []byte(big))
	if err != nil || len(ex.Markdown) > uploadMaxMarkdownBytes+200 || !strings.Contains(ex.Markdown, "truncated by hive") {
		t.Fatalf("cap: len %d, %v", len(ex.Markdown), err)
	}
	long := strings.Repeat("{\"k\":\"value\"}\n", uploadPreviewBytes/8)
	shown, cut := previewCut(long)
	if !cut || len(shown) > uploadPreviewBytes || !strings.HasSuffix(shown, "}") {
		t.Fatalf("previewCut = %d %v", len(shown), cut)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ExtractUpload(ctx, UploadKindJSONL, []byte(strings.Repeat("{}\n", 5000))); err == nil {
		t.Fatal("a cancelled extraction must fail")
	}
	if _, err := ExtractUpload(context.Background(), "exe", []byte("x")); err == nil || err.Code != UploadErrUnsupported {
		t.Fatalf("unknown kind = %v", err)
	}
}
