package knowledge

import (
	"fmt"
	"testing"
)

func buildMalformedCMapPDF() []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		"<< /Length 37 >>\nstream\nBT /F1 12 Tf 72 720 Td (A) Tj ET\nendstream",
		"<< /Type /Font /Subtype /Type0 /BaseFont /Bad /Encoding /Identity-H /ToUnicode 6 0 R /DescendantFonts [7 0 R] >>",
		"<< /Length 9 >>\nstream\nendbfchar\nendstream",
		"<< /Type /Font /Subtype /CIDFontType2 /BaseFont /Bad /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> >>",
	}
	var buf []byte
	buf = append(buf, []byte("%PDF-1.4\n")...)
	offsets := make([]int, len(objects)+1)
	for i, obj := range objects {
		offsets[i+1] = len(buf)
		buf = append(buf, []byte(fmt.Sprintf("%d 0 obj\n%s\nendobj\n", i+1, obj))...)
	}
	xref := len(buf)
	buf = append(buf, []byte(fmt.Sprintf("xref\n0 %d\n0000000000 65535 f \n", len(objects)+1))...)
	for i := 1; i <= len(objects); i++ {
		buf = append(buf, []byte(fmt.Sprintf("%010d 00000 n \n", offsets[i]))...)
	}
	buf = append(buf, []byte(fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref))...)
	return buf
}

func TestParsePDFMalformedToUnicodeCMapDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("parsePDF panicked on malformed ToUnicode CMap: %v", r)
		}
	}()

	chunks, title := parsePDF(buildMalformedCMapPDF())
	if len(chunks) != 0 {
		t.Fatalf("malformed page should be skipped, got chunks: %+v", chunks)
	}
	if title != "" {
		t.Fatalf("malformed page should not produce a title, got %q", title)
	}
}
