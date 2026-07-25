package knowledge

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestExtractChunkTitle_Branches(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "first sentence split",
			body: "This is the first sentence. And more follows here in the body.",
			want: "This is the first sentence",
		},
		{
			name: "heading markers stripped",
			body: "## A Heading\nbody text",
			want: "A Heading",
		},
		{
			name: "oversized first line truncated",
			body: strings.Repeat("x", chunkTitleMaxChars+20),
			want: strings.Repeat("x", chunkTitleMaxChars),
		},
		{
			name: "empty falls back to section",
			body: "\nrest",
			want: "Section 4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractChunkTitle(tt.body, 4)
			if got != tt.want {
				t.Errorf("extractChunkTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestParsePDF_LongPage exercises the >maxChunkChars branch of parsePDF, which
// splits a single page into multiple parts.
func TestParsePDF_LongPage(t *testing.T) {
	// Build a page whose extracted text exceeds maxChunkChars. Each drawn line
	// is a separate Tj so the reader reconstructs many lines of text.
	var sb strings.Builder
	sb.WriteString("BT /F1 10 Tf ")
	y := 780
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&sb, "1 0 0 1 72 %d Tm (Line %d with enough words here to add length to the page content stream) Tj ", y, i)
		y -= 6
	}
	sb.WriteString("ET")
	data := buildPDFWithContent(sb.String())

	chunks, _ := parsePDF(data)
	if len(chunks) == 0 {
		t.Fatal("expected chunks from long PDF page")
	}
}

// buildPDFWithContent builds a single-page PDF with the given raw content stream.
func buildPDFWithContent(content string) []byte {
	objs := []string{
		"1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n",
		"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n",
		"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>\nendobj\n",
		fmt.Sprintf("4 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(content), content),
		"5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n",
	}
	buf := "%PDF-1.4\n"
	offsets := []int{}
	for _, o := range objs {
		offsets = append(offsets, len(buf))
		buf += o
	}
	xrefStart := len(buf)
	buf += fmt.Sprintf("xref\n0 %d\n", len(objs)+1)
	buf += "0000000000 65535 f \n"
	for _, off := range offsets {
		buf += fmt.Sprintf("%010d 00000 n \n", off)
	}
	buf += fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", len(objs)+1, xrefStart)
	return []byte(buf)
}

// TestGraphStore_AddTriplesAndRemove covers AddTriples (batch) and RemoveTriple.
func TestGraphStore_AddTriplesEmptyAndBatch(t *testing.T) {
	gs := newTestGraphStore(t)

	// Empty batch is a no-op.
	if err := gs.AddTriples(nil); err != nil {
		t.Fatalf("AddTriples(nil): %v", err)
	}

	batch := []Triple{
		{Subject: "a", Predicate: PredicateRelatedTo, Object: "b"},
		{Subject: "b", Predicate: PredicateRelatedTo, Object: "c"},
	}
	if err := gs.AddTriples(batch); err != nil {
		t.Fatalf("AddTriples: %v", err)
	}

	all := gs.AllTriples()
	if len(all) != 2 {
		t.Fatalf("expected 2 triples, got %d", len(all))
	}

	if err := gs.RemoveTriple(batch[0]); err != nil {
		t.Fatalf("RemoveTriple: %v", err)
	}
	if len(gs.AllTriples()) != 1 {
		t.Errorf("expected 1 triple after removal")
	}
}

func TestNewGraphStore_BadPath(t *testing.T) {
	// A path whose parent is a file (not a directory) cannot be created.
	dir := t.TempDir()
	fileAsDir := dir + "/afile"
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Attempt to open a DB "inside" the file — MkdirAll on a file parent fails.
	if _, err := NewGraphStore(fileAsDir+"/nested/graph.db", covLogger()); err == nil {
		t.Error("expected error creating graph store under a file path")
	}
}
