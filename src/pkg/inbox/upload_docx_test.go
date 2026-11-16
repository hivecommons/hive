package inbox

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

const docxNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`

// makeDocx builds a minimal Word document in memory; extra entries are
// added as given.
func makeDocx(t *testing.T, documentXML string, extra map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		docxDocumentPart:      documentXML,
	}
	for k, v := range extra {
		files[k] = v
	}
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func para(style, text string) string {
	ppr := ""
	if style != "" {
		ppr = `<w:pPr><w:pStyle w:val="` + style + `"/></w:pPr>`
	}
	return `<w:p>` + ppr + `<w:r><w:t>` + text + `</w:t></w:r></w:p>`
}

func listPara(level int, text string) string {
	return fmt.Sprintf(`<w:p><w:pPr><w:numPr><w:ilvl w:val="%d"/><w:numId w:val="1"/></w:numPr></w:pPr><w:r><w:t>%s</w:t></w:r></w:p>`, level, text)
}

func cell(text string) string { return `<w:tc>` + para("", text) + `</w:tc>` }

func TestExtractDOCX(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document ` + docxNS + `><w:body>` +
		para("Title", "Quarterly plan") +
		para("Heading2", "Goals") +
		`<w:p><w:r><w:t xml:space="preserve">Ship the </w:t></w:r><w:r><w:t>drop zone</w:t></w:r><w:r><w:tab/><w:t>first &amp; fast.</w:t></w:r></w:p>` +
		`<w:p><w:r><w:delText>deleted words</w:delText><w:instrText>HYPERLINK x</w:instrText></w:r></w:p>` +
		listPara(0, "docs") + listPara(1, "api reference") + listPara(0, "tests") +
		`<w:tbl><w:tr>` + cell("Owner") + cell("Task") + `</w:tr><w:tr>` + cell("Ana") + cell("a|b") + `</w:tr></w:tbl>` +
		`</w:body></w:document>`
	data := makeDocx(t, body, map[string]string{docxCorePart: `<cp:coreProperties xmlns:cp="x" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Plan 2027</dc:title></cp:coreProperties>`})
	kind, uerr := SniffUpload("plan.docx", data)
	if uerr != nil || kind != UploadKindDOCX {
		t.Fatalf("sniff = %q, %v", kind, uerr)
	}
	ex, uerr := ExtractUpload(context.Background(), kind, data)
	if uerr != nil {
		t.Fatal(uerr)
	}
	if ex.Title != "Plan 2027" {
		t.Fatalf("title = %q", ex.Title)
	}
	for _, want := range []string{"# Quarterly plan", "## Goals", "Ship the drop zone first & fast.", "- docs\n  - api reference\n- tests", "| Owner | Task |", `| Ana | a\|b |`} {
		if !strings.Contains(ex.Markdown, want) {
			t.Errorf("markdown missing %q:\n%s", want, ex.Markdown)
		}
	}
	for _, gone := range []string{"deleted words", "HYPERLINK"} {
		if strings.Contains(ex.Markdown, gone) {
			t.Errorf("markdown kept %q", gone)
		}
	}
	// Without core properties the first heading names it.
	ex, uerr = ExtractUpload(context.Background(), UploadKindDOCX, makeDocx(t, body, nil))
	if uerr != nil || ex.Title != "Quarterly plan" {
		t.Fatalf("heading title = %q, %v", ex.Title, uerr)
	}
}

func TestExtractDOCXRefusesXXE(t *testing.T) {
	xxe := `<?xml version="1.0"?><!DOCTYPE w:document [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><w:document ` + docxNS + `><w:body>` + para("", "&xxe;") + `</w:body></w:document>`
	_, uerr := ExtractUpload(context.Background(), UploadKindDOCX, makeDocx(t, xxe, nil))
	if uerr == nil || uerr.Code != UploadErrParse || !strings.Contains(uerr.Message, "DOCTYPE") {
		t.Fatalf("xxe = %v", uerr)
	}
	// An undeclared entity is refused by the strict decoder too.
	ent := `<w:document ` + docxNS + `><w:body>` + para("", "&secret;") + `</w:body></w:document>`
	if _, uerr := ExtractUpload(context.Background(), UploadKindDOCX, makeDocx(t, ent, nil)); uerr == nil || uerr.Code != UploadErrParse {
		t.Fatalf("entity = %v", uerr)
	}
	// A DTD in core.xml only drops the title.
	ok := `<w:document ` + docxNS + `><w:body>` + para("", "plain") + `</w:body></w:document>`
	ex, uerr := ExtractUpload(context.Background(), UploadKindDOCX, makeDocx(t, ok, map[string]string{docxCorePart: `<!DOCTYPE x [<!ENTITY e SYSTEM "file:///etc/passwd">]><x><title>&e;</title></x>`}))
	if uerr != nil || ex.Title != "" || ex.Markdown != "plain" {
		t.Fatalf("core dtd = %+v, %v", ex, uerr)
	}
}

func TestExtractDOCXRefusesZipBombs(t *testing.T) {
	// A highly compressible document.xml: refused by the ratio check before
	// it is inflated.
	bomb := `<w:document ` + docxNS + `><w:body>` + para("", strings.Repeat("a", 2<<20)) + `</w:body></w:document>`
	if _, uerr := ExtractUpload(context.Background(), UploadKindDOCX, makeDocx(t, bomb, nil)); uerr == nil || !strings.Contains(uerr.Message, "zip bomb") {
		t.Fatalf("ratio bomb = %v", uerr)
	}
	// Too many entries.
	extra := map[string]string{}
	for i := range docxMaxEntries + 1 {
		extra[fmt.Sprintf("x/%d.xml", i)] = "x"
	}
	if _, uerr := ExtractUpload(context.Background(), UploadKindDOCX, makeDocx(t, `<w:document `+docxNS+`/>`, extra)); uerr == nil || !strings.Contains(uerr.Message, "entries") {
		t.Fatalf("entry bomb = %v", uerr)
	}
	// Not a Word document: a zip without word/document.xml, and garbage.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("readme.txt")
	_, _ = w.Write([]byte("hi"))
	_ = zw.Close()
	if _, uerr := ExtractUpload(context.Background(), UploadKindDOCX, buf.Bytes()); uerr == nil || !strings.Contains(uerr.Message, "missing") {
		t.Fatalf("no document = %v", uerr)
	}
	if _, uerr := ExtractUpload(context.Background(), UploadKindDOCX, []byte("PK\x03\x04garbage")); uerr == nil || uerr.Code != UploadErrParse {
		t.Fatalf("garbage = %v", uerr)
	}
	if _, uerr := ExtractUpload(context.Background(), UploadKindDOCX, makeDocx(t, `<w:document `+docxNS+`><w:body/></w:document>`, nil)); uerr == nil || uerr.Code != UploadErrEmpty {
		t.Fatalf("empty = %v", uerr)
	}
	// A zip renamed to anything else is still refused as an archive.
	if _, uerr := SniffUpload("notes.md", makeDocx(t, `<w:document `+docxNS+`/>`, nil)); uerr == nil || uerr.Code != UploadErrUnsupported {
		t.Fatalf("zip as md = %v", uerr)
	}
}

func TestDocxHeadingLevel(t *testing.T) {
	for in, want := range map[string]int{"Title": 1, "Heading1": 1, "heading 3": 3, "Heading9": htmlHeadingLevels, "Normal": 0, "HeadingX": 0} {
		if got := docxHeadingLevel(in); got != want {
			t.Errorf("docxHeadingLevel(%q) = %d, want %d", in, got, want)
		}
	}
}
